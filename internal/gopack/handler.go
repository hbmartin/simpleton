package gopack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/haroldmartin/simpleton/internal/domain"
	"github.com/haroldmartin/simpleton/internal/gitx"
	"github.com/haroldmartin/simpleton/internal/packrpc"
)

type Handler struct{}

func (Handler) Capability() domain.PackCapability {
	return domain.PackCapability{
		Language:        "go",
		PackVersion:     "0.1.1",
		ProtocolVersion: domain.ProtocolVersion,
		Methods: map[string]bool{
			"analyze": true,
			"probe":   false,
			"cancel":  true,
			"types":   true,
			"callers": true,
			"effects": true,
			"ssa":     false,
		},
		Requirements: map[string]string{"toolchain": "go"},
		TrustClasses: []string{"trusted_branch"},
	}
}

func (Handler) Analyze(ctx context.Context, params packrpc.AnalyzeParams) (packrpc.AnalyzeResult, error) {
	started := time.Now()
	repo, err := gitx.Open(params.Repository)
	if err != nil {
		return packrpc.AnalyzeResult{}, err
	}
	changed := map[string]bool{}
	for _, file := range params.ChangedFiles {
		if file.Language == "go" {
			changed[file.Path] = true
		}
	}
	allPaths, err := repo.ListFiles(ctx, params.HeadRevision)
	if err != nil {
		return packrpc.AnalyzeResult{}, err
	}
	parsed, declarationKeys, callers, typeErrors, summary, err := parseRepository(ctx, repo, params.HeadRevision, allPaths, changed)
	if err != nil {
		return packrpc.AnalyzeResult{}, err
	}
	var targets []domain.VerificationTarget
	var opportunities []domain.Opportunity
	changedPaths := make([]string, 0, len(changed))
	for path := range changed {
		changedPaths = append(changedPaths, path)
	}
	slices.Sort(changedPaths)
	for _, path := range changedPaths {
		file := parsed[path]
		if file == nil {
			continue
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			symbol := declarationSymbol(fn)
			id := domain.StableID("go", path, symbol)
			risks := effectRisks(file, fn)
			boundaries := make([]domain.ObservationBoundary, 0)
			for _, caller := range callers[declarationKeys[fn]] {
				if !changed[caller.Path] {
					boundaries = append(boundaries, domain.ObservationBoundary{
						Kind: "unchanged_caller", Symbol: caller.Symbol, Path: caller.Path, Stable: true, Confidence: 0.85,
					})
				}
			}
			if exported(fn.Name.Name) {
				boundaries = append(boundaries, domain.ObservationBoundary{
					Kind: "public_api", Symbol: symbol, Path: path, Stable: true, Confidence: 0.75,
				})
			}
			boundaries = append(boundaries, domain.ObservationBoundary{
				Kind: "direct_unit", Symbol: symbol, Path: path, Confidence: 0.55,
			})
			kind := "function"
			if fn.Recv != nil {
				kind = "method"
			}
			targets = append(targets, domain.VerificationTarget{
				ID: id, Language: "go", File: path, Symbol: symbol, Kind: kind, ScopeType: "symbol",
				ObservationCandidates: boundaries,
				Applicability: domain.RunApplicability{
					Applicable: len(risks) == 0, Reason: applicabilityReason(risks), Confidence: confidenceFor(risks), Risks: risks,
				},
			})
			statements := countStatements(fn.Body)
			complexity := cyclomatic(fn.Body)
			if statements >= 30 || complexity >= 10 {
				opportunities = append(opportunities, domain.Opportunity{
					ID: domain.StableID("go-opportunity", path, symbol), Language: "go", Category: "oversized_or_complex_unit",
					Region:       path + ":" + symbol,
					Evidence:     []string{fmt.Sprintf("statements=%d", statements), fmt.Sprintf("cyclomatic=%d", complexity)},
					AllowedFiles: []string{path}, Benefit: clamp(float64(statements)/80 + float64(complexity)/30),
					ApplicabilityConfidence: confidenceFor(risks), Risk: clamp(float64(len(risks)) / 5), ReviewEffort: clamp(float64(statements) / 100),
				})
			}
		}
	}
	method := domain.MethodResult{
		ID: "go_native_analysis", Language: "go", Status: domain.StatusRan,
		Budget: time.Duration(params.BudgetMS * int64(time.Millisecond)).String(), DurationMS: time.Since(started).Milliseconds(),
	}
	switch {
	case summary.failedChanged > 0:
		method.Status = domain.StatusInconclusive
		method.Reason = fmt.Sprintf("%d changed Go source files could not be analyzed", summary.failedChanged)
	case summary.eligibleChanged == 0:
		method.Reason = "no eligible changed Go sources"
	default:
		method.Coverage = &domain.Coverage{TargetsTotal: len(targets), TargetsObserved: len(targets), Ratio: ratio(len(targets), len(targets))}
	}
	typeMethod := domain.MethodResult{ID: "go_type_analysis", Language: "go", Status: domain.StatusRan}
	if len(typeErrors) > 0 {
		typeMethod.Status = domain.StatusInconclusive
		typeMethod.Reason = fmt.Sprintf("%d package type-check diagnostics; first: %s", len(typeErrors), typeErrors[0])
	}
	return packrpc.AnalyzeResult{Targets: targets, Methods: []domain.MethodResult{method, typeMethod}, Opportunities: opportunities}, nil
}

func (Handler) Probe(context.Context, packrpc.ProbeParams) (packrpc.ProbeResult, error) {
	return packrpc.ProbeResult{Method: domain.MethodResult{ID: "go_native_probe", Language: "go", Status: domain.StatusUnsupported, Reason: "native probe generation is not implemented"}}, nil
}

type caller struct {
	Path   string
	Symbol string
}

type sourceDiagnostic struct {
	path    string
	message string
}

type parseSummary struct {
	eligibleChanged int
	failedChanged   int
}

type goSource struct {
	path       string
	file       *ast.File
	constraint constraint.Expr
	contents   []byte
	analyzable bool
}

func parseRepository(ctx context.Context, repo gitx.Repository, revision string, paths []string, changed map[string]bool) (map[string]*ast.File, map[*ast.FuncDecl]string, map[string][]caller, []string, parseSummary, error) {
	fset := token.NewFileSet()
	goSources := map[string]goSource{}
	summary := parseSummary{}
	goPaths := make([]string, 0)
	for _, path := range paths {
		if !goSourceCandidate(path) {
			continue
		}
		goPaths = append(goPaths, path)
	}
	sources, batchErr := repo.FilesAt(ctx, revision, goPaths)
	if sources == nil {
		sources = map[string][]byte{}
	}
	var fallbackFailures []error
	var sourceDiagnostics []sourceDiagnostic
	for _, path := range goPaths {
		if ctx.Err() != nil {
			return nil, nil, nil, nil, parseSummary{}, ctx.Err()
		}
		source, ok := sources[path]
		if !ok {
			var err error
			source, err = repo.FileAt(ctx, revision, path)
			if err != nil {
				failure := fmt.Errorf("read %s: %w", path, err)
				if batchErr != nil {
					fallbackFailures = append(fallbackFailures, failure)
				} else {
					sourceDiagnostics = append(sourceDiagnostics, sourceDiagnostic{path: path, message: failure.Error()})
				}
				if changed[path] {
					summary.failedChanged++
				}
				continue
			}
		}
		expression, err := sourceBuildConstraint(fset, path, source)
		if err != nil {
			if hint, _ := parser.ParseFile(fset, path, source, parser.SkipObjectResolution); hint != nil {
				goSources[path] = goSource{path: path, file: hint, contents: source}
			}
			sourceDiagnostics = append(sourceDiagnostics, sourceDiagnostic{path: path, message: "build constraints " + path + ": " + err.Error()})
			if changed[path] {
				summary.failedChanged++
			}
			continue
		}
		if expression != nil && requiresIgnoreTag(expression) {
			continue
		}
		file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
		if file != nil {
			goSources[path] = goSource{path: path, file: file, constraint: expression, contents: source}
		}
		if err != nil {
			sourceDiagnostics = append(sourceDiagnostics, sourceDiagnostic{path: path, message: "parse " + path + ": " + err.Error()})
			if changed[path] {
				summary.failedChanged++
			}
			continue
		}
		goSources[path] = goSource{path: path, file: file, constraint: expression, contents: source, analyzable: true}
	}
	if len(fallbackFailures) > 0 {
		return nil, nil, nil, nil, parseSummary{}, fmt.Errorf("batch-read Go sources: %w; fallback failures: %v", batchErr, errors.Join(fallbackFailures...))
	}
	metadata := loadPackageMetadata(ctx, repo, revision, paths)
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, parseSummary{}, err
	}
	platforms, err := loadBuildPlatforms(ctx)
	if err != nil {
		return nil, nil, nil, nil, parseSummary{}, err
	}
	methodNames := changedFileMethodNames(goSources, changed)
	relevantDirectories := relevantSourceDirectories(goSources, metadata, changed, methodNames)
	profiles, profileDiagnostics := compatibleBuildProfiles(goSources, relevantDirectories, changed, platforms)
	activeParsed := map[string]*ast.File{}
	declarationKeys := map[*ast.FuncDecl]string{}
	callers := map[string][]caller{}
	diagnostics := slices.Clone(profileDiagnostics)
	checkedDirectories := map[string]bool{}
	for _, profile := range profiles {
		profileParsed := map[string]*ast.File{}
		for path, source := range goSources {
			if source.analyzable && source.matches(profile) {
				profileParsed[path] = source.file
				activeParsed[path] = source.file
			}
		}
		profileDeclarations, profileCallers, profileDiagnostics, profileDirectories, err := typeCheck(ctx, fset, profileParsed, metadata, changed, relevantDirectories)
		if err != nil {
			return nil, nil, nil, nil, parseSummary{}, err
		}
		for declaration, key := range profileDeclarations {
			declarationKeys[declaration] = key
		}
		for key, entries := range profileCallers {
			callers[key] = append(callers[key], entries...)
		}
		diagnostics = append(diagnostics, profileDiagnostics...)
		for directory := range profileDirectories {
			checkedDirectories[directory] = true
		}
	}
	if len(profiles) == 0 {
		diagnostics = append(diagnostics, metadata.diagnostics...)
	}
	for _, diagnostic := range sourceDiagnostics {
		directory := filepath.Dir(diagnostic.path)
		if changed[diagnostic.path] || relevantDirectories[directory] || checkedDirectories[directory] {
			diagnostics = append(diagnostics, diagnostic.message)
		}
	}
	for path, source := range goSources {
		if !changed[path] || !source.analyzable {
			continue
		}
		if activeParsed[path] != nil {
			summary.eligibleChanged++
		} else {
			summary.failedChanged++
		}
	}
	slices.Sort(diagnostics)
	diagnostics = slices.Compact(diagnostics)
	normalizeCallers(callers)
	return activeParsed, declarationKeys, callers, diagnostics, summary, nil
}

func goSourceCandidate(path string) bool {
	if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
		return false
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	for index, part := range parts {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return false
		}
		if index < len(parts)-1 && (part == "testdata" || part == "vendor") {
			return false
		}
	}
	return true
}

func sourceBuildConstraint(fset *token.FileSet, path string, source []byte) (constraint.Expr, error) {
	var goBuild constraint.Expr
	var plusBuild constraint.Expr
	file := fset.AddFile(path+":build-constraints", -1, len(source))
	var lexer scanner.Scanner
	lexer.Init(file, source, nil, scanner.ScanComments)
	for {
		_, kind, literal := lexer.Scan()
		if kind == token.EOF || kind == token.PACKAGE {
			break
		}
		if kind != token.COMMENT || !strings.HasPrefix(literal, "//") {
			continue
		}
		if constraint.IsGoBuild(literal) {
			if goBuild != nil {
				return nil, errors.New("multiple //go:build constraints")
			}
			expression, err := constraint.Parse(literal)
			if err != nil {
				return nil, err
			}
			goBuild = expression
			continue
		}
	}
	if goBuild != nil {
		return goBuild, nil
	}
	for _, rawLine := range bytes.Split(legacyBuildHeader(source), []byte{'\n'}) {
		literal := string(bytes.TrimSpace(rawLine))
		if !constraint.IsPlusBuild(literal) {
			continue
		}
		expression, err := constraint.Parse(literal)
		if err != nil {
			// The Go build system ignores malformed legacy +build lines.
			continue
		}
		if plusBuild == nil {
			plusBuild = expression
		} else {
			plusBuild = &constraint.AndExpr{X: plusBuild, Y: expression}
		}
	}
	return plusBuild, nil
}

func legacyBuildHeader(source []byte) []byte {
	end := 0
	offset := 0
	for len(source) > offset {
		next := bytes.IndexByte(source[offset:], '\n')
		lineEnd := len(source)
		if next >= 0 {
			lineEnd = offset + next + 1
		}
		line := bytes.TrimSpace(source[offset:lineEnd])
		switch {
		case len(line) == 0:
			end = lineEnd
		case bytes.HasPrefix(line, []byte("//")):
		default:
			return source[:end]
		}
		offset = lineEnd
	}
	return source[:end]
}

func requiresIgnoreTag(expression constraint.Expr) bool {
	switch expression := expression.(type) {
	case *constraint.TagExpr:
		return expression.Tag == "ignore"
	case *constraint.AndExpr:
		return requiresIgnoreTag(expression.X) || requiresIgnoreTag(expression.Y)
	case *constraint.OrExpr:
		return requiresIgnoreTag(expression.X) && requiresIgnoreTag(expression.Y)
	default:
		return false
	}
}

type buildPlatform struct {
	GOOS         string `json:"GOOS"`
	GOARCH       string `json:"GOARCH"`
	CgoSupported bool   `json:"CgoSupported"`
}

var buildPlatformCache struct {
	sync.Mutex
	platforms []buildPlatform
}

func loadBuildPlatforms(ctx context.Context) ([]buildPlatform, error) {
	buildPlatformCache.Lock()
	if len(buildPlatformCache.platforms) > 0 {
		platforms := slices.Clone(buildPlatformCache.platforms)
		buildPlatformCache.Unlock()
		return platforms, nil
	}
	buildPlatformCache.Unlock()

	output, err := exec.CommandContext(ctx, "go", "tool", "dist", "list", "-json").Output()
	if err != nil {
		return nil, fmt.Errorf("list supported Go platforms: %w", err)
	}
	var platforms []buildPlatform
	if err := json.Unmarshal(output, &platforms); err != nil {
		return nil, fmt.Errorf("decode supported Go platforms: %w", err)
	}
	if len(platforms) == 0 {
		return nil, errors.New("Go toolchain returned no supported platforms")
	}
	slices.SortStableFunc(platforms, func(left, right buildPlatform) int {
		leftRank := platformPreference(left)
		rightRank := platformPreference(right)
		if leftRank != rightRank {
			return leftRank - rightRank
		}
		if left.GOOS != right.GOOS {
			return strings.Compare(left.GOOS, right.GOOS)
		}
		return strings.Compare(left.GOARCH, right.GOARCH)
	})
	buildPlatformCache.Lock()
	if len(buildPlatformCache.platforms) == 0 {
		buildPlatformCache.platforms = slices.Clone(platforms)
	}
	platforms = slices.Clone(buildPlatformCache.platforms)
	buildPlatformCache.Unlock()
	return platforms, nil
}

func platformPreference(platform buildPlatform) int {
	switch {
	case platform.GOOS == build.Default.GOOS && platform.GOARCH == build.Default.GOARCH:
		return 0
	case platform.GOOS == build.Default.GOOS:
		return 1
	case platform.GOARCH == build.Default.GOARCH:
		return 2
	default:
		return 3
	}
}

type buildProfile struct {
	goos         string
	goarch       string
	compiler     string
	cgo          bool
	tags         map[string]bool
	platformTags map[string]bool
}

func (source goSource) matches(profile buildProfile) bool {
	buildContext := build.Default
	buildContext.GOOS = profile.goos
	buildContext.GOARCH = profile.goarch
	buildContext.Compiler = profile.compiler
	buildContext.CgoEnabled = profile.cgo
	buildContext.BuildTags = enabledTags(profile.tags)
	buildContext.OpenFile = func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(source.contents)), nil
	}
	matched, err := buildContext.MatchFile(".", filepath.Base(source.path))
	return err == nil && matched
}

func (profile buildProfile) matchesTag(tag string) bool {
	source := goSource{path: "tag.go", contents: []byte("//go:build " + tag + "\n\npackage tag\n")}
	return source.matches(profile)
}

func (profile buildProfile) fixedTag(tag string) (bool, bool) {
	if tag == "ignore" {
		return false, true
	}
	if tag == "cgo" || tag == "unix" || tag == "gc" || tag == "gccgo" || tag == "boringcrypto" || profile.platformTags[tag] {
		return profile.matchesTag(tag), true
	}
	if strings.HasPrefix(tag, "go1.") || strings.HasPrefix(tag, "goexperiment.") || profile.tags[tag] {
		return profile.matchesTag(tag), true
	}
	return false, false
}

func (profile buildProfile) withAssignments(assignments map[string]bool) buildProfile {
	result := profile
	result.tags = maps.Clone(profile.tags)
	for tag, enabled := range assignments {
		if enabled {
			result.tags[tag] = true
		}
	}
	return result
}

func (profile buildProfile) key() string {
	tags := make([]string, 0, len(profile.tags))
	for tag, enabled := range profile.tags {
		if enabled {
			tags = append(tags, tag)
		}
	}
	slices.Sort(tags)
	return fmt.Sprintf("%s/%s/%s/%t/%s", profile.goos, profile.goarch, profile.compiler, profile.cgo, strings.Join(tags, ","))
}

func compatibleBuildProfiles(sources map[string]goSource, relevantDirectories map[string]bool, changed map[string]bool, platforms []buildPlatform) ([]buildProfile, []string) {
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	changedSources := make([]goSource, 0)
	for _, path := range paths {
		if changed[path] && sources[path].analyzable {
			changedSources = append(changedSources, sources[path])
		}
	}
	if len(changedSources) == 0 {
		return nil, nil
	}
	profiles := make([]buildProfile, 0)
	var diagnostics []string
	seen := map[string]bool{}
	appendProfile := func(profile buildProfile) {
		key := profile.key()
		if !seen[key] {
			seen[key] = true
			profiles = append(profiles, profile)
		}
	}
	for _, source := range changedSources {
		if profilesMatchSource(profiles, source) {
			continue
		}
		profile, status := profileForSources([]goSource{source}, platforms)
		if status == constraintSatisfied {
			appendProfile(profile)
		}
		if status == constraintBudgetExhausted {
			diagnostics = append(diagnostics, "build profile search budget exhausted for "+source.path)
		} else if status == constraintUnsatisfied {
			diagnostics = append(diagnostics, "no compatible build profile for "+source.path)
		}
	}
	for _, path := range paths {
		source := sources[path]
		if !source.analyzable || !relevantDirectories[filepath.Dir(path)] || profilesMatchSource(profiles, source) {
			continue
		}
		exhausted := false
		matched := false
		for _, changedSource := range changedSources {
			profile, status := profileForSources([]goSource{changedSource, source}, platforms)
			if status == constraintSatisfied {
				appendProfile(profile)
				matched = true
				break
			}
			if status == constraintBudgetExhausted {
				exhausted = true
			}
		}
		if !matched && exhausted {
			diagnostics = append(diagnostics, "build profile search budget exhausted while pairing changed sources with "+source.path)
		}
	}
	slices.Sort(diagnostics)
	return profiles, slices.Compact(diagnostics)
}

func profilesMatchSource(profiles []buildProfile, source goSource) bool {
	for _, profile := range profiles {
		if source.matches(profile) {
			return true
		}
	}
	return false
}

func profileForSources(sources []goSource, platforms []buildPlatform) (buildProfile, constraintSatisfaction) {
	platformTags := map[string]bool{}
	for _, platform := range platforms {
		platformTags[platform.GOOS] = true
		platformTags[platform.GOARCH] = true
	}
	exhausted := false
	for _, platform := range platforms {
		cgoValues := []bool{false}
		if platform.CgoSupported {
			cgoValues = []bool{build.Default.CgoEnabled, !build.Default.CgoEnabled}
		}
		for _, cgoEnabled := range cgoValues {
			profile := baseBuildProfile(platform, cgoEnabled, platformTags)
			searchBudget := buildConstraintSearchBudget
			assignments, status := satisfySourceConstraints(sources, 0, profile, map[string]bool{}, &searchBudget)
			if status == constraintBudgetExhausted {
				exhausted = true
				continue
			}
			if status != constraintSatisfied {
				continue
			}
			profile = profile.withAssignments(assignments)
			compatible := true
			for _, source := range sources {
				if !source.matches(profile) {
					compatible = false
					break
				}
			}
			if compatible {
				return profile, constraintSatisfied
			}
		}
	}
	if exhausted {
		return buildProfile{}, constraintBudgetExhausted
	}
	return buildProfile{}, constraintUnsatisfied
}

func baseBuildProfile(platform buildPlatform, cgoEnabled bool, platformTags map[string]bool) buildProfile {
	tags := make(map[string]bool, len(build.Default.BuildTags)+len(build.Default.ToolTags)+len(build.Default.ReleaseTags))
	for _, values := range [][]string{build.Default.BuildTags, build.Default.ToolTags, build.Default.ReleaseTags} {
		for _, tag := range values {
			tags[tag] = true
		}
	}
	return buildProfile{
		goos: platform.GOOS, goarch: platform.GOARCH, compiler: build.Default.Compiler, cgo: cgoEnabled,
		tags: tags, platformTags: platformTags,
	}
}

type constraintSatisfaction uint8

const buildConstraintSearchBudget = 4096

const (
	constraintUnsatisfied constraintSatisfaction = iota
	constraintSatisfied
	constraintBudgetExhausted
)

type constraintContinuation func(map[string]bool) (map[string]bool, constraintSatisfaction)

func satisfySourceConstraints(sources []goSource, index int, profile buildProfile, assignments map[string]bool, searchBudget *int) (map[string]bool, constraintSatisfaction) {
	if index == len(sources) {
		return assignments, constraintSatisfied
	}
	if sources[index].constraint == nil {
		return satisfySourceConstraints(sources, index+1, profile, assignments, searchBudget)
	}
	return satisfyConstraint(sources[index].constraint, true, profile, assignments, searchBudget, func(next map[string]bool) (map[string]bool, constraintSatisfaction) {
		return satisfySourceConstraints(sources, index+1, profile, next, searchBudget)
	})
}

func satisfyConstraint(expression constraint.Expr, wanted bool, profile buildProfile, assignments map[string]bool, searchBudget *int, continuation constraintContinuation) (map[string]bool, constraintSatisfaction) {
	if *searchBudget <= 0 {
		return nil, constraintBudgetExhausted
	}
	*searchBudget = *searchBudget - 1
	switch expression := expression.(type) {
	case *constraint.TagExpr:
		if value, fixed := profile.fixedTag(expression.Tag); fixed {
			if value != wanted {
				return nil, constraintUnsatisfied
			}
			return continuation(assignments)
		}
		if value, exists := assignments[expression.Tag]; exists {
			if value != wanted {
				return nil, constraintUnsatisfied
			}
			return continuation(assignments)
		}
		result := maps.Clone(assignments)
		result[expression.Tag] = wanted
		return continuation(result)
	case *constraint.NotExpr:
		return satisfyConstraint(expression.X, !wanted, profile, assignments, searchBudget, continuation)
	case *constraint.AndExpr:
		if wanted {
			return satisfyConstraint(expression.X, true, profile, assignments, searchBudget, func(left map[string]bool) (map[string]bool, constraintSatisfaction) {
				return satisfyConstraint(expression.Y, true, profile, left, searchBudget, continuation)
			})
		}
		left, status := satisfyConstraint(expression.X, false, profile, assignments, searchBudget, continuation)
		if status != constraintUnsatisfied {
			return left, status
		}
		return satisfyConstraint(expression.Y, false, profile, assignments, searchBudget, continuation)
	case *constraint.OrExpr:
		if wanted {
			left, status := satisfyConstraint(expression.X, true, profile, assignments, searchBudget, continuation)
			if status != constraintUnsatisfied {
				return left, status
			}
			return satisfyConstraint(expression.Y, true, profile, assignments, searchBudget, continuation)
		}
		return satisfyConstraint(expression.X, false, profile, assignments, searchBudget, func(left map[string]bool) (map[string]bool, constraintSatisfaction) {
			return satisfyConstraint(expression.Y, false, profile, left, searchBudget, continuation)
		})
	default:
		return nil, constraintUnsatisfied
	}
}

func enabledTags(tags map[string]bool) []string {
	result := make([]string, 0, len(tags))
	for tag, enabled := range tags {
		if enabled {
			result = append(result, tag)
		}
	}
	slices.Sort(result)
	return result
}

func changedFileMethodNames(sources map[string]goSource, changed map[string]bool) map[string]bool {
	result := map[string]bool{}
	for path := range changed {
		source := sources[path]
		if !source.analyzable || source.file == nil {
			continue
		}
		for _, declaration := range source.file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv != nil {
				result[function.Name.Name] = true
			}
		}
	}
	return result
}

type sourcePackageGroup struct {
	key       string
	paths     []string
	files     []*ast.File
	preferred bool
}

type sourcePackageIndex struct {
	paths       []string
	packages    map[string]*sourcePackageGroup
	diagnostics []string
}

func indexSourcePackages(files map[string]*ast.File, metadata packageMetadata, preferredPaths map[string]bool) sourcePackageIndex {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	groups := map[string]*sourcePackageGroup{}
	for _, path := range paths {
		file := files[path]
		if file == nil || file.Name == nil || file.Name.Name == "" {
			continue
		}
		key := filepath.Dir(path) + ":" + file.Name.Name
		group := groups[key]
		if group == nil {
			group = &sourcePackageGroup{key: key}
			groups[key] = group
		}
		group.paths = append(group.paths, path)
		group.files = append(group.files, file)
		group.preferred = group.preferred || preferredPaths[path]
	}
	groupKeys := make([]string, 0, len(groups))
	for key := range groups {
		groupKeys = append(groupKeys, key)
	}
	slices.Sort(groupKeys)
	groupsByImportPath := map[string][]*sourcePackageGroup{}
	for _, key := range groupKeys {
		group := groups[key]
		directory := filepath.ToSlash(filepath.Dir(group.paths[0]))
		packagePath := metadata.packagePath(directory, group.key)
		groupsByImportPath[packagePath] = append(groupsByImportPath[packagePath], group)
	}
	resolvedPaths := make([]string, 0, len(groupsByImportPath))
	for packagePath := range groupsByImportPath {
		resolvedPaths = append(resolvedPaths, packagePath)
	}
	slices.Sort(resolvedPaths)
	index := sourcePackageIndex{packages: map[string]*sourcePackageGroup{}}
	for _, resolvedPath := range resolvedPaths {
		candidates := groupsByImportPath[resolvedPath]
		slices.SortStableFunc(candidates, func(left, right *sourcePackageGroup) int {
			if left.preferred != right.preferred {
				if left.preferred {
					return -1
				}
				return 1
			}
			return strings.Compare(left.key, right.key)
		})
		for indexInPath, group := range candidates {
			packagePath := resolvedPath
			if indexInPath > 0 {
				packagePath = group.key
				index.diagnostics = append(index.diagnostics, resolvedPath+": multiple packages resolve to the same import path")
			}
			index.packages[packagePath] = group
			index.paths = append(index.paths, packagePath)
		}
	}
	slices.Sort(index.paths)
	slices.Sort(index.diagnostics)
	index.diagnostics = slices.Compact(index.diagnostics)
	return index
}

func relevantSourceDirectories(sources map[string]goSource, metadata packageMetadata, changed map[string]bool, methodNames map[string]bool) map[string]bool {
	files := map[string]*ast.File{}
	preferredPaths := map[string]bool{}
	for path, source := range sources {
		if source.file != nil {
			files[path] = source.file
		}
		if source.analyzable {
			preferredPaths[path] = true
		}
	}
	index := indexSourcePackages(files, metadata, preferredPaths)
	selected := map[string]bool{}
	importers := make(map[string][]string, len(index.packages))
	for _, packagePath := range index.paths {
		group := index.packages[packagePath]
		for fileIndex, file := range group.files {
			if changed[group.paths[fileIndex]] {
				selected[packagePath] = true
			}
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err == nil && index.packages[importPath] != nil {
					importers[importPath] = append(importers[importPath], packagePath)
				}
			}
		}
	}
	expandReverseImporters(selected, importers)
	if len(methodNames) > 0 {
		for _, packagePath := range index.paths {
			if selected[packagePath] {
				continue
			}
			for _, file := range index.packages[packagePath].files {
				if fileCallsMethodNamed(file, methodNames) {
					selected[packagePath] = true
					break
				}
			}
		}
	}
	directories := map[string]bool{}
	for packagePath := range selected {
		for _, path := range index.packages[packagePath].paths {
			directories[filepath.Dir(path)] = true
		}
	}
	return directories
}

func expandReverseImporters(selected map[string]bool, importers map[string][]string) {
	worklist := make([]string, 0, len(selected))
	for packagePath := range selected {
		worklist = append(worklist, packagePath)
	}
	for len(worklist) > 0 {
		dependency := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]
		for _, packagePath := range importers[dependency] {
			if !selected[packagePath] {
				selected[packagePath] = true
				worklist = append(worklist, packagePath)
			}
		}
	}
}

type packageMetadata struct {
	modules     []moduleMetadata
	diagnostics []string
}

type moduleMetadata struct {
	root string
	path string
}

func loadPackageMetadata(ctx context.Context, repo gitx.Repository, revision string, paths []string) packageMetadata {
	metadata := packageMetadata{}
	modulePaths := make([]string, 0)
	for _, path := range paths {
		if filepath.Base(path) == "go.mod" {
			modulePaths = append(modulePaths, path)
		}
	}
	contentsByPath, batchErr := repo.FilesAt(ctx, revision, modulePaths)
	if contentsByPath == nil {
		contentsByPath = map[string][]byte{}
	}
	for _, path := range modulePaths {
		contents, ok := contentsByPath[path]
		if !ok {
			var err error
			contents, err = repo.FileAt(ctx, revision, path)
			if err != nil {
				diagnostic := "read " + path + ": " + err.Error()
				if batchErr != nil {
					diagnostic = "batch-read module metadata: " + batchErr.Error() + "; " + diagnostic
				}
				metadata.diagnostics = append(metadata.diagnostics, diagnostic)
				continue
			}
		}
		modulePath, err := parseModulePath(contents)
		if err != nil {
			metadata.diagnostics = append(metadata.diagnostics, path+": "+err.Error())
			continue
		}
		metadata.modules = append(metadata.modules, moduleMetadata{
			root: filepath.ToSlash(filepath.Dir(path)),
			path: modulePath,
		})
	}
	slices.SortFunc(metadata.modules, func(left, right moduleMetadata) int {
		if len(left.root) != len(right.root) {
			return len(right.root) - len(left.root)
		}
		return strings.Compare(left.root, right.root)
	})
	slices.Sort(metadata.diagnostics)
	metadata.diagnostics = slices.Compact(metadata.diagnostics)
	return metadata
}

func parseModulePath(contents []byte) (string, error) {
	for _, rawLine := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(rawLine)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		path := fields[1]
		if strings.HasPrefix(path, "\"") || strings.HasPrefix(path, "`") {
			unquoted, err := strconv.Unquote(path)
			if err != nil {
				return "", fmt.Errorf("invalid quoted module path: %w", err)
			}
			path = unquoted
		}
		if path == "" {
			return "", fmt.Errorf("module path is empty")
		}
		return path, nil
	}
	return "", fmt.Errorf("module directive is missing")
}

func (m packageMetadata) packagePath(directory, fallback string) string {
	directory = filepath.ToSlash(directory)
	for _, module := range m.modules {
		if module.root != "." && directory != module.root && !strings.HasPrefix(directory, module.root+"/") {
			continue
		}
		relative := directory
		if module.root != "." {
			relative = strings.TrimPrefix(strings.TrimPrefix(directory, module.root), "/")
		}
		if relative == "." || relative == "" {
			return module.path
		}
		return strings.TrimSuffix(module.path, "/") + "/" + relative
	}
	return fallback
}

type parsedPackage struct {
	sourcePackageGroup
	info       *types.Info
	types      *types.Package
	checking   bool
	checked    bool
	checkError error
}

type repositoryImporter struct {
	ctx         context.Context
	fset        *token.FileSet
	packages    map[string]*parsedPackage
	fallback    types.Importer
	diagnostics *[]string
}

type importResult struct {
	pack *types.Package
	err  error
}

type typeCheckResult struct {
	declarationKeys map[*ast.FuncDecl]string
	callers         map[string][]caller
	diagnostics     []string
	checkedDirs     map[string]bool
	err             error
}

func (r *repositoryImporter) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if group := r.packages[path]; group != nil {
		return r.check(path, group)
	}
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if r.fallback == nil {
		return nil, fmt.Errorf("no importer is available for %q", path)
	}
	completed := make(chan importResult, 1)
	go func() {
		pack, err := r.fallback.Import(path)
		completed <- importResult{pack: pack, err: err}
	}()
	select {
	case <-r.ctx.Done():
		return nil, r.ctx.Err()
	case result := <-completed:
		return result.pack, result.err
	}
}

func (r *repositoryImporter) check(path string, group *parsedPackage) (*types.Package, error) {
	if group.checked {
		return group.types, group.checkError
	}
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if group.checking {
		return group.types, fmt.Errorf("import cycle involving %q", path)
	}
	group.checking = true
	defer func() { group.checking = false }()
	group.info = &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	group.types = types.NewPackage(path, group.files[0].Name.Name)
	configuration := types.Config{
		Importer: r,
		Error: func(err error) {
			*r.diagnostics = append(*r.diagnostics, path+": "+err.Error())
		},
	}
	group.checkError = types.NewChecker(&configuration, r.fset, group.types, group.info).Files(group.files)
	group.checked = true
	return group.types, group.checkError
}

func typeCheck(ctx context.Context, fset *token.FileSet, parsed map[string]*ast.File, metadata packageMetadata, changed map[string]bool, relevantDirectories map[string]bool) (map[*ast.FuncDecl]string, map[string][]caller, []string, map[string]bool, error) {
	return typeCheckWithImporter(ctx, fset, parsed, metadata, changed, relevantDirectories, importer.ForCompiler(fset, "source", nil))
}

func typeCheckWithImporter(ctx context.Context, fset *token.FileSet, parsed map[string]*ast.File, metadata packageMetadata, changed map[string]bool, relevantDirectories map[string]bool, fallback types.Importer) (map[*ast.FuncDecl]string, map[string][]caller, []string, map[string]bool, error) {
	completed := make(chan typeCheckResult, 1)
	go func() {
		declarationKeys, callers, diagnostics, checkedDirs, err := typeCheckSynchronously(ctx, fset, parsed, metadata, changed, relevantDirectories, fallback)
		completed <- typeCheckResult{declarationKeys: declarationKeys, callers: callers, diagnostics: diagnostics, checkedDirs: checkedDirs, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, nil, nil, nil, ctx.Err()
	case result := <-completed:
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, err
		}
		return result.declarationKeys, result.callers, result.diagnostics, result.checkedDirs, result.err
	}
}

func typeCheckSynchronously(ctx context.Context, fset *token.FileSet, parsed map[string]*ast.File, metadata packageMetadata, changed map[string]bool, relevantDirectories map[string]bool, fallback types.Importer) (map[*ast.FuncDecl]string, map[string][]caller, []string, map[string]bool, error) {
	diagnostics := slices.Clone(metadata.diagnostics)
	declarationKeys := map[*ast.FuncDecl]string{}
	callers := map[string][]caller{}
	index := indexSourcePackages(parsed, metadata, nil)
	diagnostics = append(diagnostics, index.diagnostics...)
	packages := map[string]*parsedPackage{}
	for packagePath, group := range index.packages {
		packages[packagePath] = &parsedPackage{sourcePackageGroup: *group}
	}
	relevantPaths := relevantPackagePaths(index.paths, packages, changed, relevantDirectories)
	checker := &repositoryImporter{
		ctx: ctx, fset: fset, packages: packages,
		fallback: fallback, diagnostics: &diagnostics,
	}
	for _, packagePath := range relevantPaths {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, err
		}
		_, _ = checker.check(packagePath, packages[packagePath])
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	checkedPaths := make([]string, 0, len(index.paths))
	for _, packagePath := range index.paths {
		if packages[packagePath].checked {
			checkedPaths = append(checkedPaths, packagePath)
		}
	}
	receiverTypes := make([]types.Type, 0)
	for _, packagePath := range checkedPaths {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, err
		}
		group := packages[packagePath]
		for _, name := range group.types.Scope().Names() {
			typeName, ok := group.types.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := typeName.Type().(*types.Named)
			if !ok {
				continue
			}
			if _, isInterface := named.Underlying().(*types.Interface); isInterface {
				continue
			}
			receiverTypes = append(receiverTypes, named, types.NewPointer(named))
		}
		for _, file := range group.files {
			for _, declaration := range file.Decls {
				fn, ok := declaration.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				object, _ := group.info.Defs[fn.Name].(*types.Func)
				key := functionKey(object)
				if key != "" {
					declarationKeys[fn] = key
				}
			}
		}
	}
	implementingTypesByInterface := map[*types.Interface][]types.Type{}
	for _, packagePath := range checkedPaths {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, err
		}
		group := packages[packagePath]
		for index, file := range group.files {
			path := group.paths[index]
			for _, declaration := range file.Decls {
				fn, ok := declaration.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					if ctx.Err() != nil {
						return false
					}
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					for _, called := range calledFunctions(group.info, call.Fun, receiverTypes, implementingTypesByInterface) {
						calledKey := functionKey(called)
						if calledKey != "" {
							callers[calledKey] = append(callers[calledKey], caller{Path: path, Symbol: declarationSymbol(fn)})
						}
					}
					return true
				})
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, err
	}
	slices.Sort(diagnostics)
	diagnostics = slices.Compact(diagnostics)
	normalizeCallers(callers)
	checkedDirectories := map[string]bool{}
	for _, packagePath := range checkedPaths {
		for _, path := range packages[packagePath].paths {
			checkedDirectories[filepath.Dir(path)] = true
		}
	}
	return declarationKeys, callers, diagnostics, checkedDirectories, nil
}

func relevantPackagePaths(packagePaths []string, packages map[string]*parsedPackage, changed map[string]bool, relevantDirectories map[string]bool) []string {
	result := make([]string, 0)
	for _, packagePath := range packagePaths {
		group := packages[packagePath]
		for _, path := range group.paths {
			if changed[path] || relevantDirectories[filepath.Dir(path)] {
				result = append(result, packagePath)
				break
			}
		}
	}
	return result
}

func normalizeCallers(callers map[string][]caller) {
	for key, entries := range callers {
		slices.SortFunc(entries, func(left, right caller) int {
			if left.Path != right.Path {
				return strings.Compare(left.Path, right.Path)
			}
			return strings.Compare(left.Symbol, right.Symbol)
		})
		callers[key] = slices.CompactFunc(entries, func(left, right caller) bool {
			return left.Path == right.Path && left.Symbol == right.Symbol
		})
	}
}

func fileCallsMethodNamed(file *ast.File, methodNames map[string]bool) bool {
	if len(methodNames) == 0 {
		return false
	}
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		if found {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selection := calledSelector(call.Fun)
		if selection != nil && methodNames[selection.Sel.Name] {
			found = true
			return false
		}
		return true
	})
	return found
}

func calledSelector(expression ast.Expr) *ast.SelectorExpr {
	switch expression := expression.(type) {
	case *ast.SelectorExpr:
		return expression
	case *ast.IndexExpr:
		return calledSelector(expression.X)
	case *ast.IndexListExpr:
		return calledSelector(expression.X)
	case *ast.ParenExpr:
		return calledSelector(expression.X)
	default:
		return nil
	}
}

func calledFunctions(info *types.Info, expression ast.Expr, receiverTypes []types.Type, implementingTypesByInterface map[*types.Interface][]types.Type) []*types.Func {
	called := calledFunction(info, expression)
	functions := []*types.Func{}
	seen := map[string]bool{}
	appendFunction := func(function *types.Func) {
		key := functionKey(function)
		if key != "" && !seen[key] {
			seen[key] = true
			functions = append(functions, function)
		}
	}
	appendFunction(called)
	selection := calledSelection(info, expression)
	if selection == nil || called == nil {
		return functions
	}
	interfaceType, ok := selection.Recv().Underlying().(*types.Interface)
	if !ok {
		return functions
	}
	implementingTypes, cached := implementingTypesByInterface[interfaceType]
	if !cached {
		interfaceType.Complete()
		for _, receiverType := range receiverTypes {
			if types.Implements(receiverType, interfaceType) {
				implementingTypes = append(implementingTypes, receiverType)
			}
		}
		implementingTypesByInterface[interfaceType] = implementingTypes
	}
	for _, receiverType := range implementingTypes {
		candidate, _, _ := types.LookupFieldOrMethod(receiverType, true, called.Pkg(), called.Name())
		function, _ := candidate.(*types.Func)
		appendFunction(function)
	}
	return functions
}

func calledSelection(info *types.Info, expression ast.Expr) *types.Selection {
	switch expression := expression.(type) {
	case *ast.SelectorExpr:
		return info.Selections[expression]
	case *ast.IndexExpr:
		return calledSelection(info, expression.X)
	case *ast.IndexListExpr:
		return calledSelection(info, expression.X)
	case *ast.ParenExpr:
		return calledSelection(info, expression.X)
	default:
		return nil
	}
}

func calledFunction(info *types.Info, expression ast.Expr) *types.Func {
	switch expression := expression.(type) {
	case *ast.Ident:
		function, _ := info.Uses[expression].(*types.Func)
		return function
	case *ast.SelectorExpr:
		if selection := info.Selections[expression]; selection != nil {
			function, _ := selection.Obj().(*types.Func)
			return function
		}
		function, _ := info.Uses[expression.Sel].(*types.Func)
		return function
	case *ast.IndexExpr:
		return calledFunction(info, expression.X)
	case *ast.IndexListExpr:
		return calledFunction(info, expression.X)
	case *ast.ParenExpr:
		return calledFunction(info, expression.X)
	default:
		return nil
	}
}

func functionKey(function *types.Func) string {
	if function == nil || function.Pkg() == nil {
		return ""
	}
	receiver := ""
	if signature, ok := function.Type().(*types.Signature); ok && signature.Recv() != nil {
		receiver = receiverTypeKey(signature.Recv().Type())
	}
	return function.Pkg().Path() + "\x00" + receiver + "\x00" + function.Name()
}

func receiverTypeKey(receiver types.Type) string {
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	if named, ok := receiver.(*types.Named); ok {
		object := named.Obj()
		if object.Pkg() != nil {
			return object.Pkg().Path() + "." + object.Name()
		}
		return object.Name()
	}
	return types.TypeString(receiver, func(pkg *types.Package) string { return pkg.Path() })
}

func effectRisks(file *ast.File, fn *ast.FuncDecl) []string {
	imports := map[string]bool{}
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		imports[path] = true
	}
	var risks []string
	for _, path := range []string{"net", "net/http", "os", "io", "time", "math/rand", "crypto/rand", "database/sql", "sync"} {
		if imports[path] {
			risks = append(risks, "imports_"+strings.ReplaceAll(path, "/", "_"))
		}
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.GoStmt:
			risks = append(risks, "concurrency")
		case *ast.SendStmt:
			risks = append(risks, "channel_send")
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if selector, ok := lhs.(*ast.SelectorExpr); ok {
					if id, ok := selector.X.(*ast.Ident); ok && id.Name != "" {
						risks = append(risks, "external_state_write")
					}
				}
			}
		}
		return true
	})
	slices.Sort(risks)
	return slices.Compact(risks)
}

func declarationSymbol(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return function.Name.Name
	}
	receiver := receiverName(function.Recv.List[0].Type)
	if receiver == "" {
		return function.Name.Name
	}
	return receiver + "." + function.Name.Name
}

func receiverName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.StarExpr:
		return receiverName(expression.X)
	case *ast.IndexExpr:
		return receiverName(expression.X)
	case *ast.IndexListExpr:
		return receiverName(expression.X)
	case *ast.ParenExpr:
		return receiverName(expression.X)
	default:
		return ""
	}
}

func countStatements(body *ast.BlockStmt) int {
	count := 0
	ast.Inspect(body, func(node ast.Node) bool {
		if _, ok := node.(ast.Stmt); ok {
			count++
		}
		return true
	})
	return count
}

func cyclomatic(body *ast.BlockStmt) int {
	complexity := 1
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.CaseClause, *ast.CommClause:
			complexity++
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				complexity++
			}
		}
		return true
	})
	return complexity
}

func exported(name string) bool {
	if name == "" {
		return false
	}
	return unicode.IsUpper([]rune(name)[0])
}

func applicabilityReason(risks []string) string {
	if len(risks) == 0 {
		return "no statically detected effects"
	}
	return "effect risks require an approved stable observation boundary"
}

func confidenceFor(risks []string) float64 {
	return clamp(0.9 - 0.12*float64(len(risks)))
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}
