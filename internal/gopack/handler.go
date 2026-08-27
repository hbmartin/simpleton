package gopack

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
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
	parsed, declarationKeys, callers, typeErrors, err := parseRepository(ctx, repo, params.HeadRevision, allPaths, changed)
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

func parseRepository(ctx context.Context, repo gitx.Repository, revision string, paths []string, changed map[string]bool) (map[string]*ast.File, map[*ast.FuncDecl]string, map[string][]caller, []string, error) {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	goPaths := make([]string, 0)
	for _, path := range paths {
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			continue
		}
		goPaths = append(goPaths, path)
	}
	sources, batchErr := repo.FilesAt(ctx, revision, goPaths)
	if sources == nil {
		sources = map[string][]byte{}
	}
	var fallbackFailures []error
	for _, path := range goPaths {
		if ctx.Err() != nil {
			return nil, nil, nil, nil, ctx.Err()
		}
		source, ok := sources[path]
		if !ok {
			var err error
			source, err = repo.FileAt(ctx, revision, path)
			if err != nil {
				if batchErr != nil || changed[path] {
					fallbackFailures = append(fallbackFailures, fmt.Errorf("read %s: %w", path, err))
				}
				continue
			}
		}
		file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		parsed[path] = file
	}
	if len(fallbackFailures) > 0 {
		if batchErr != nil {
			return nil, nil, nil, nil, fmt.Errorf("batch-read Go sources: %w; fallback failures: %v", batchErr, errors.Join(fallbackFailures...))
		}
		return nil, nil, nil, nil, errors.Join(fallbackFailures...)
	}
	metadata := loadPackageMetadata(ctx, repo, revision, paths)
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, err
	}
	declarationKeys, callers, diagnostics, err := typeCheck(ctx, fset, parsed, metadata, changed)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return parsed, declarationKeys, callers, diagnostics, nil
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

func typeCheck(ctx context.Context, fset *token.FileSet, parsed map[string]*ast.File, metadata packageMetadata, changed map[string]bool) (map[*ast.FuncDecl]string, map[string][]caller, []string, error) {
	return typeCheckWithImporter(ctx, fset, parsed, metadata, changed, importer.ForCompiler(fset, "source", nil))
}

func typeCheckWithImporter(ctx context.Context, fset *token.FileSet, parsed map[string]*ast.File, metadata packageMetadata, changed map[string]bool, fallback types.Importer) (map[*ast.FuncDecl]string, map[string][]caller, []string, error) {
	completed := make(chan typeCheckResult, 1)
	go func() {
		declarationKeys, callers, diagnostics, err := typeCheckSynchronously(ctx, fset, parsed, metadata, changed, fallback)
		completed <- typeCheckResult{declarationKeys: declarationKeys, callers: callers, diagnostics: diagnostics, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	case result := <-completed:
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		return result.declarationKeys, result.callers, result.diagnostics, result.err
	}
}

func typeCheckSynchronously(ctx context.Context, fset *token.FileSet, parsed map[string]*ast.File, metadata packageMetadata, changed map[string]bool, fallback types.Importer) (map[*ast.FuncDecl]string, map[string][]caller, []string, error) {
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
	relevantPaths := relevantPackagePaths(packagePaths, packages, changed)
	checker := &repositoryImporter{
		ctx: ctx, fset: fset, packages: packages,
		fallback: fallback, diagnostics: &diagnostics,
	}
	for _, packagePath := range relevantPaths {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		_, _ = checker.check(packagePath, packages[packagePath])
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
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
			return nil, nil, nil, err
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
			return nil, nil, nil, err
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
		return nil, nil, nil, err
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
	return declarationKeys, callers, diagnostics, nil
}

func relevantPackagePaths(packagePaths []string, packages map[string]*parsedPackage, changed map[string]bool) []string {
	if len(changed) == 0 {
		return slices.Clone(packagePaths)
	}
	// Interface-dispatched callers can depend only on an interface package and
	// never import its implementation. Seed the reverse-import closure with
	// packages that declare potentially relevant interfaces instead of
	// type-checking the entire repository for every method-bearing changed file.
	methodNames := map[string]bool{}
	for _, group := range packages {
		for index, file := range group.files {
			if !changed[group.paths[index]] {
				continue
			}
			for _, declaration := range file.Decls {
				if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv != nil {
					methodNames[function.Name.Name] = true
				}
			}
		}
	}
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
			if len(methodNames) > 0 && declaresInterfaceMethod(file, methodNames) {
				selected[packagePath] = true
			}
		}
	}
	worklist := make([]string, 0, len(selected))
	for _, packagePath := range packagePaths {
		if selected[packagePath] {
			worklist = append(worklist, packagePath)
		}
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
	result := make([]string, 0, len(selected))
	for _, packagePath := range packagePaths {
		if selected[packagePath] {
			result = append(result, packagePath)
		}
	}
	return result
}

func declaresInterfaceMethod(file *ast.File, methodNames map[string]bool) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		if found {
			return false
		}
		interfaceType, ok := node.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, field := range interfaceType.Methods.List {
			for _, name := range field.Names {
				if methodNames[name.Name] {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
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
