package gopack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		PackVersion:     "0.1.0",
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
	parsed, declarationKeys, callers, typeErrors, err := parseRepository(ctx, repo, params.HeadRevision, allPaths)
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
		Coverage: &domain.Coverage{TargetsTotal: len(targets), TargetsObserved: len(targets), Ratio: ratio(len(targets), len(targets))},
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

func parseRepository(ctx context.Context, repo gitx.Repository, revision string, paths []string) (map[string]*ast.File, map[*ast.FuncDecl]string, map[string][]caller, []string, error) {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, path := range paths {
		if ctx.Err() != nil {
			return nil, nil, nil, nil, ctx.Err()
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := repo.FileAt(ctx, revision, path)
		if err != nil {
			continue
		}
		file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		parsed[path] = file
	}
	metadata := loadPackageMetadata(ctx, repo, revision)
	declarationKeys, callers, diagnostics := typeCheck(fset, parsed, metadata)
	return parsed, declarationKeys, callers, diagnostics, nil
}

type packageMetadata struct {
	pathByDirectory map[string]string
	exports         map[string]string
	diagnostics     []string
}

type listedPackageError struct {
	Err string `json:"Err"`
}

type listedPackage struct {
	ImportPath string               `json:"ImportPath"`
	Dir        string               `json:"Dir"`
	Export     string               `json:"Export"`
	Error      *listedPackageError  `json:"Error"`
	DepsErrors []listedPackageError `json:"DepsErrors"`
}

func loadPackageMetadata(ctx context.Context, repo gitx.Repository, revision string) packageMetadata {
	metadata := packageMetadata{pathByDirectory: map[string]string{}, exports: map[string]string{}}
	worktree, cleanup, err := repo.DetachedWorktree(ctx, revision)
	if err != nil {
		metadata.diagnostics = append(metadata.diagnostics, "prepare module-aware type analysis: "+err.Error())
		return metadata
	}
	defer cleanup()
	canonicalWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		canonicalWorktree = worktree
	}
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-deps", "-export", "-json", "./...")
	cmd.Dir = canonicalWorktree
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, runErr := cmd.Output()
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var listed listedPackage
		if err := decoder.Decode(&listed); err != nil {
			if err != io.EOF {
				metadata.diagnostics = append(metadata.diagnostics, "decode go list metadata: "+err.Error())
			}
			break
		}
		if listed.ImportPath != "" && listed.Export != "" {
			metadata.exports[listed.ImportPath] = listed.Export
		}
		if relative, err := filepath.Rel(canonicalWorktree, listed.Dir); listed.Dir != "" && err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			metadata.pathByDirectory[filepath.ToSlash(relative)] = listed.ImportPath
		}
		if listed.Error != nil && listed.Error.Err != "" {
			metadata.diagnostics = append(metadata.diagnostics, listed.ImportPath+": "+listed.Error.Err)
		}
		for _, dependencyError := range listed.DepsErrors {
			if dependencyError.Err != "" {
				metadata.diagnostics = append(metadata.diagnostics, listed.ImportPath+": "+dependencyError.Err)
			}
		}
	}
	if runErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = runErr.Error()
		}
		metadata.diagnostics = append(metadata.diagnostics, "go list: "+message)
	}
	slices.Sort(metadata.diagnostics)
	metadata.diagnostics = slices.Compact(metadata.diagnostics)
	return metadata
}

type parsedPackage struct {
	paths []string
	files []*ast.File
}

func typeCheck(fset *token.FileSet, parsed map[string]*ast.File, metadata packageMetadata) (map[*ast.FuncDecl]string, map[string][]caller, []string) {
	packages := map[string]*parsedPackage{}
	paths := make([]string, 0, len(parsed))
	for path := range parsed {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		file := parsed[path]
		directory := filepath.Dir(path)
		key := directory + ":" + file.Name.Name
		group := packages[key]
		if group == nil {
			group = &parsedPackage{}
			packages[key] = group
		}
		group.paths = append(group.paths, path)
		group.files = append(group.files, file)
	}
	diagnostics := slices.Clone(metadata.diagnostics)
	declarationKeys := map[*ast.FuncDecl]string{}
	callers := map[string][]caller{}
	packageKeys := make([]string, 0, len(packages))
	for key := range packages {
		packageKeys = append(packageKeys, key)
	}
	slices.Sort(packageKeys)
	lookup := func(path string) (io.ReadCloser, error) {
		export := metadata.exports[path]
		if export == "" {
			return nil, fmt.Errorf("module export data is unavailable for %q", path)
		}
		return os.Open(export)
	}
	for _, key := range packageKeys {
		group := packages[key]
		directory := filepath.ToSlash(filepath.Dir(group.paths[0]))
		packagePath := metadata.pathByDirectory[directory]
		if packagePath == "" {
			packagePath = key
		}
		info := &types.Info{
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
		configuration := types.Config{
			Importer: importer.ForCompiler(fset, "gc", lookup),
			Error: func(err error) {
				diagnostics = append(diagnostics, packagePath+": "+err.Error())
			},
		}
		_, _ = configuration.Check(packagePath, fset, group.files, info)
		for index, file := range group.files {
			path := group.paths[index]
			for _, declaration := range file.Decls {
				fn, ok := declaration.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				object, _ := info.Defs[fn.Name].(*types.Func)
				key := functionKey(object)
				if key != "" {
					declarationKeys[fn] = key
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					called := calledFunction(info, call.Fun)
					calledKey := functionKey(called)
					if calledKey != "" {
						callers[calledKey] = append(callers[calledKey], caller{Path: path, Symbol: declarationSymbol(fn)})
					}
					return true
				})
			}
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
	return declarationKeys, callers, diagnostics
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
