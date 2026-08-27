package gopack

import (
	"context"
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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	force   bool
}

type parseSummary struct {
	eligibleChanged int
	failedChanged   int
}

type goSource struct {
	path       string
	file       *ast.File
	constraint constraint.Expr
}

func parseRepository(ctx context.Context, repo gitx.Repository, revision string, paths []string, changed map[string]bool) (map[string]*ast.File, map[*ast.FuncDecl]string, map[string][]caller, []string, parseSummary, error) {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	hints := map[string]*ast.File{}
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
					sourceDiagnostics = append(sourceDiagnostics, sourceDiagnostic{path: path, message: failure.Error(), force: true})
				}
				if changed[path] {
					summary.failedChanged++
				}
				continue
			}
		}
		expression, err := sourceBuildConstraint(source)
		if err != nil {
			if hint, _ := parser.ParseFile(fset, path, source, parser.SkipObjectResolution); hint != nil {
				hints[path] = hint
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
			hints[path] = file
		}
		if err != nil {
			sourceDiagnostics = append(sourceDiagnostics, sourceDiagnostic{path: path, message: "parse " + path + ": " + err.Error()})
			if changed[path] {
				summary.failedChanged++
			}
			continue
		}
		parsed[path] = file
		goSources[path] = goSource{path: path, file: file, constraint: expression}
	}
	if len(fallbackFailures) > 0 {
		return nil, nil, nil, nil, parseSummary{}, fmt.Errorf("batch-read Go sources: %w; fallback failures: %v", batchErr, errors.Join(fallbackFailures...))
	}
	metadata := loadPackageMetadata(ctx, repo, revision, paths)
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, parseSummary{}, err
	}
	methodNames := changedFileMethodNames(parsed, changed)
	relevantDirectories := relevantSourceDirectories(hints, metadata, changed, methodNames)
	profiles := compatibleBuildProfiles(goSources, relevantDirectories, changed)
	activeParsed := map[string]*ast.File{}
	declarationKeys := map[*ast.FuncDecl]string{}
	callers := map[string][]caller{}
	var diagnostics []string
	checkedDirectories := map[string]bool{}
	for _, profile := range profiles {
		profileParsed := map[string]*ast.File{}
		for path, source := range goSources {
			if source.matches(profile) {
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
		if diagnostic.force || changed[diagnostic.path] || relevantDirectories[directory] || checkedDirectories[directory] {
			diagnostics = append(diagnostics, diagnostic.message)
		}
	}
	for path := range activeParsed {
		if changed[path] {
			summary.eligibleChanged++
		}
	}
	slices.Sort(diagnostics)
	diagnostics = slices.Compact(diagnostics)
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

func sourceBuildConstraint(source []byte) (constraint.Expr, error) {
	var goBuild constraint.Expr
	var plusBuild constraint.Expr
	file := token.NewFileSet().AddFile("", -1, len(source))
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
		if constraint.IsPlusBuild(literal) {
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
	}
	if goBuild != nil {
		return goBuild, nil
	}
	return plusBuild, nil
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

var knownGOOS = []string{
	"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js",
	"linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows", "zos",
}

var knownGOARCH = []string{
	"386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be", "loong64", "mips", "mipsle",
	"mips64", "mips64le", "mips64p32", "mips64p32le", "ppc", "ppc64", "ppc64le", "riscv", "riscv64",
	"s390", "s390x", "sparc", "sparc64", "wasm",
}

var knownGOOSSet = stringSet(knownGOOS)
var knownGOARCHSet = stringSet(knownGOARCH)
var unixGOOS = stringSet([]string{"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "linux", "netbsd", "openbsd", "solaris"})

type buildProfile struct {
	goos     string
	goarch   string
	compiler string
	cgo      bool
	tags     map[string]bool
}

func (source goSource) matches(profile buildProfile) bool {
	if !matchesBuildFilename(filepath.Base(source.path), profile) {
		return false
	}
	return source.constraint == nil || source.constraint.Eval(profile.matchesTag)
}

func (profile buildProfile) matchesTag(tag string) bool {
	switch {
	case tag == "cgo":
		return profile.cgo
	case tag == profile.goos || tag == profile.goarch || tag == profile.compiler:
		return true
	case profile.goos == "android" && tag == "linux":
		return true
	case profile.goos == "illumos" && tag == "solaris":
		return true
	case profile.goos == "ios" && tag == "darwin":
		return true
	case tag == "unix":
		return unixGOOS[profile.goos]
	case tag == "boringcrypto":
		return profile.tags["goexperiment.boringcrypto"]
	default:
		return profile.tags[tag]
	}
}

func (profile buildProfile) fixedTag(tag string) (bool, bool) {
	if tag == "ignore" {
		return false, true
	}
	if tag == "cgo" || tag == "unix" || tag == "gc" || tag == "gccgo" || knownGOOSSet[tag] || knownGOARCHSet[tag] {
		return profile.matchesTag(tag), true
	}
	if strings.HasPrefix(tag, "go1.") || strings.HasPrefix(tag, "goexperiment.") || profile.tags[tag] {
		return profile.matchesTag(tag), true
	}
	return false, false
}

func (profile buildProfile) withAssignments(assignments map[string]bool) buildProfile {
	result := profile
	result.tags = make(map[string]bool, len(profile.tags)+len(assignments))
	for tag, enabled := range profile.tags {
		result.tags[tag] = enabled
	}
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

func compatibleBuildProfiles(sources map[string]goSource, relevantDirectories map[string]bool, changed map[string]bool) []buildProfile {
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	changedSources := make([]goSource, 0)
	for _, path := range paths {
		if changed[path] {
			changedSources = append(changedSources, sources[path])
		}
	}
	if len(changedSources) == 0 {
		return nil
	}
	profiles := make([]buildProfile, 0)
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
		if profile, ok := profileForSources([]goSource{source}); ok {
			appendProfile(profile)
		}
	}
	for _, path := range paths {
		source := sources[path]
		if !relevantDirectories[filepath.Dir(path)] || profilesMatchSource(profiles, source) {
			continue
		}
		for _, changedSource := range changedSources {
			profile, ok := profileForSources([]goSource{changedSource, source})
			if ok {
				appendProfile(profile)
				break
			}
		}
	}
	return profiles
}

func profilesMatchSource(profiles []buildProfile, source goSource) bool {
	for _, profile := range profiles {
		if source.matches(profile) {
			return true
		}
	}
	return false
}

func profileForSources(sources []goSource) (buildProfile, bool) {
	gooses := preferredValues(build.Default.GOOS, knownGOOS)
	goarches := preferredValues(build.Default.GOARCH, knownGOARCH)
	cgoValues := []bool{build.Default.CgoEnabled, !build.Default.CgoEnabled}
	for _, goos := range gooses {
		for _, goarch := range goarches {
			for _, cgoEnabled := range cgoValues {
				profile := baseBuildProfile(goos, goarch, cgoEnabled)
				compatible := true
				for _, source := range sources {
					if !matchesBuildFilename(filepath.Base(source.path), profile) {
						compatible = false
						break
					}
				}
				if !compatible {
					continue
				}
				searchBudget := 4096
				assignments, ok := satisfySourceConstraints(sources, 0, profile, map[string]bool{}, &searchBudget)
				if !ok {
					continue
				}
				profile = profile.withAssignments(assignments)
				for _, source := range sources {
					if !source.matches(profile) {
						compatible = false
						break
					}
				}
				if compatible {
					return profile, true
				}
			}
		}
	}
	return buildProfile{}, false
}

func baseBuildProfile(goos, goarch string, cgoEnabled bool) buildProfile {
	tags := make(map[string]bool, len(build.Default.BuildTags)+len(build.Default.ToolTags)+len(build.Default.ReleaseTags))
	for _, values := range [][]string{build.Default.BuildTags, build.Default.ToolTags, build.Default.ReleaseTags} {
		for _, tag := range values {
			tags[tag] = true
		}
	}
	return buildProfile{goos: goos, goarch: goarch, compiler: build.Default.Compiler, cgo: cgoEnabled, tags: tags}
}

type constraintContinuation func(map[string]bool) (map[string]bool, bool)

func satisfySourceConstraints(sources []goSource, index int, profile buildProfile, assignments map[string]bool, searchBudget *int) (map[string]bool, bool) {
	if index == len(sources) {
		return assignments, true
	}
	if sources[index].constraint == nil {
		return satisfySourceConstraints(sources, index+1, profile, assignments, searchBudget)
	}
	return satisfyConstraint(sources[index].constraint, true, profile, assignments, searchBudget, func(next map[string]bool) (map[string]bool, bool) {
		return satisfySourceConstraints(sources, index+1, profile, next, searchBudget)
	})
}

func satisfyConstraint(expression constraint.Expr, wanted bool, profile buildProfile, assignments map[string]bool, searchBudget *int, continuation constraintContinuation) (map[string]bool, bool) {
	if *searchBudget <= 0 {
		return nil, false
	}
	*searchBudget = *searchBudget - 1
	switch expression := expression.(type) {
	case *constraint.TagExpr:
		if value, fixed := profile.fixedTag(expression.Tag); fixed {
			if value != wanted {
				return nil, false
			}
			return continuation(assignments)
		}
		if value, exists := assignments[expression.Tag]; exists {
			if value != wanted {
				return nil, false
			}
			return continuation(assignments)
		}
		result := cloneBoolMap(assignments)
		result[expression.Tag] = wanted
		return continuation(result)
	case *constraint.NotExpr:
		return satisfyConstraint(expression.X, !wanted, profile, assignments, searchBudget, continuation)
	case *constraint.AndExpr:
		if wanted {
			return satisfyConstraint(expression.X, true, profile, assignments, searchBudget, func(left map[string]bool) (map[string]bool, bool) {
				return satisfyConstraint(expression.Y, true, profile, left, searchBudget, continuation)
			})
		}
		if left, ok := satisfyConstraint(expression.X, false, profile, assignments, searchBudget, continuation); ok {
			return left, true
		}
		return satisfyConstraint(expression.Y, false, profile, assignments, searchBudget, continuation)
	case *constraint.OrExpr:
		if wanted {
			if left, ok := satisfyConstraint(expression.X, true, profile, assignments, searchBudget, continuation); ok {
				return left, true
			}
			return satisfyConstraint(expression.Y, true, profile, assignments, searchBudget, continuation)
		}
		return satisfyConstraint(expression.X, false, profile, assignments, searchBudget, func(left map[string]bool) (map[string]bool, bool) {
			return satisfyConstraint(expression.Y, false, profile, left, searchBudget, continuation)
		})
	default:
		return nil, false
	}
}

func matchesBuildFilename(filename string, profile buildProfile) bool {
	stem, _, _ := strings.Cut(filename, ".")
	underscore := strings.Index(stem, "_")
	if underscore < 0 {
		return true
	}
	parts := strings.Split(stem[underscore+1:], "_")
	if len(parts) > 0 && parts[len(parts)-1] == "test" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) >= 2 && knownGOOSSet[parts[len(parts)-2]] && knownGOARCHSet[parts[len(parts)-1]] {
		return profile.matchesTag(parts[len(parts)-2]) && profile.matchesTag(parts[len(parts)-1])
	}
	if len(parts) >= 1 {
		last := parts[len(parts)-1]
		if knownGOOSSet[last] || knownGOARCHSet[last] {
			return profile.matchesTag(last)
		}
	}
	return true
}

func preferredValues(preferred string, values []string) []string {
	result := make([]string, 0, len(values)+1)
	result = append(result, preferred)
	for _, value := range values {
		if value != preferred {
			result = append(result, value)
		}
	}
	return result
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func cloneBoolMap(values map[string]bool) map[string]bool {
	result := make(map[string]bool, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func changedFileMethodNames(parsed map[string]*ast.File, changed map[string]bool) map[string]bool {
	result := map[string]bool{}
	for path := range changed {
		file := parsed[path]
		if file == nil {
			continue
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv != nil {
				result[function.Name.Name] = true
			}
		}
	}
	return result
}

type sourcePackageGroup struct {
	paths []string
	files []*ast.File
}

func relevantSourceDirectories(files map[string]*ast.File, metadata packageMetadata, changed map[string]bool, methodNames map[string]bool) map[string]bool {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	groups := map[string]*sourcePackageGroup{}
	for _, path := range paths {
		file := files[path]
		if file == nil || file.Name == nil {
			continue
		}
		key := filepath.Dir(path) + ":" + file.Name.Name
		group := groups[key]
		if group == nil {
			group = &sourcePackageGroup{}
			groups[key] = group
		}
		group.paths = append(group.paths, path)
		group.files = append(group.files, file)
	}
	packages := map[string]*sourcePackageGroup{}
	packagePaths := make([]string, 0, len(groups))
	for key, group := range groups {
		directory := filepath.ToSlash(filepath.Dir(group.paths[0]))
		packagePath := metadata.packagePath(directory, key)
		if packages[packagePath] != nil {
			packagePath = key
		}
		packages[packagePath] = group
		packagePaths = append(packagePaths, packagePath)
	}
	slices.Sort(packagePaths)
	selected := map[string]bool{}
	importers := make(map[string][]string, len(packages))
	for _, packagePath := range packagePaths {
		group := packages[packagePath]
		for index, file := range group.files {
			if changed[group.paths[index]] {
				selected[packagePath] = true
			}
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err == nil && packages[importPath] != nil {
					importers[importPath] = append(importers[importPath], packagePath)
				}
			}
		}
	}
	expandReverseImporters(selected, importers)
	if len(methodNames) > 0 {
		for _, packagePath := range packagePaths {
			if selected[packagePath] {
				continue
			}
			for _, file := range packages[packagePath].files {
				if fileCallsMethodNamed(file, methodNames) {
					selected[packagePath] = true
					break
				}
			}
		}
	}
	directories := map[string]bool{}
	for packagePath := range selected {
		for _, path := range packages[packagePath].paths {
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
	paths      []string
	files      []*ast.File
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
	groups := map[string]*parsedPackage{}
	paths := make([]string, 0, len(parsed))
	for path := range parsed {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		file := parsed[path]
		directory := filepath.Dir(path)
		key := directory + ":" + file.Name.Name
		group := groups[key]
		if group == nil {
			group = &parsedPackage{}
			groups[key] = group
		}
		group.paths = append(group.paths, path)
		group.files = append(group.files, file)
	}
	diagnostics := slices.Clone(metadata.diagnostics)
	declarationKeys := map[*ast.FuncDecl]string{}
	callers := map[string][]caller{}
	packages := map[string]*parsedPackage{}
	packagePaths := make([]string, 0, len(groups))
	for key, group := range groups {
		directory := filepath.ToSlash(filepath.Dir(group.paths[0]))
		packagePath := metadata.packagePath(directory, key)
		if packages[packagePath] != nil {
			diagnostics = append(diagnostics, packagePath+": multiple packages resolve to the same import path")
			packagePath = key
		}
		packages[packagePath] = group
		packagePaths = append(packagePaths, packagePath)
	}
	slices.Sort(packagePaths)
	relevantPaths := relevantPackagePaths(packagePaths, packages, changed, relevantDirectories)
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
	checkedPaths := make([]string, 0, len(packagePaths))
	for _, packagePath := range packagePaths {
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
	checkedDirectories := map[string]bool{}
	for _, packagePath := range checkedPaths {
		for _, path := range packages[packagePath].paths {
			checkedDirectories[filepath.Dir(path)] = true
		}
	}
	return declarationKeys, callers, diagnostics, checkedDirectories, nil
}

func relevantPackagePaths(packagePaths []string, packages map[string]*parsedPackage, changed map[string]bool, relevantDirectories map[string]bool) []string {
	if len(changed) == 0 {
		return slices.Clone(packagePaths)
	}
	selected := map[string]bool{}
	importers := make(map[string][]string, len(packages))
	for _, packagePath := range packagePaths {
		group := packages[packagePath]
		for index, file := range group.files {
			if changed[group.paths[index]] || relevantDirectories[filepath.Dir(group.paths[index])] {
				selected[packagePath] = true
			}
			for _, spec := range file.Imports {
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err == nil && packages[importPath] != nil {
					importers[importPath] = append(importers[importPath], packagePath)
				}
			}
		}
	}
	expandReverseImporters(selected, importers)
	result := make([]string, 0, len(selected))
	for _, packagePath := range packagePaths {
		if selected[packagePath] {
			result = append(result, packagePath)
		}
	}
	return result
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
