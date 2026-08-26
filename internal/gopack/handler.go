package gopack

import (
	"context"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
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
	parsed, callers, typeErrors, err := parseRepository(ctx, repo, params.HeadRevision, allPaths)
	if err != nil {
		return packrpc.AnalyzeResult{}, err
	}
	var targets []domain.VerificationTarget
	var opportunities []domain.Opportunity
	for path := range changed {
		file := parsed[path]
		if file == nil {
			continue
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			id := domain.StableID("go", path, fn.Name.Name)
			risks := effectRisks(file, fn)
			boundaries := make([]domain.ObservationBoundary, 0)
			for _, caller := range callers[fn.Name.Name] {
				if !changed[caller.Path] {
					boundaries = append(boundaries, domain.ObservationBoundary{
						Kind: "unchanged_caller", Symbol: caller.Symbol, Path: caller.Path, Stable: true, Confidence: 0.85,
					})
				}
			}
			if exported(fn.Name.Name) {
				boundaries = append(boundaries, domain.ObservationBoundary{
					Kind: "public_api", Symbol: fn.Name.Name, Path: path, Stable: true, Confidence: 0.75,
				})
			}
			boundaries = append(boundaries, domain.ObservationBoundary{
				Kind: "direct_unit", Symbol: fn.Name.Name, Path: path, Confidence: 0.55,
			})
			targets = append(targets, domain.VerificationTarget{
				ID: id, Language: "go", File: path, Symbol: fn.Name.Name, Kind: "function", ScopeType: "symbol",
				ObservationCandidates: boundaries,
				Applicability: domain.RunApplicability{
					Applicable: len(risks) == 0, Reason: applicabilityReason(risks), Confidence: confidenceFor(risks), Risks: risks,
				},
			})
			statements := countStatements(fn.Body)
			complexity := cyclomatic(fn.Body)
			if statements >= 30 || complexity >= 10 {
				opportunities = append(opportunities, domain.Opportunity{
					ID: domain.StableID("go-opportunity", path, fn.Name.Name), Language: "go", Category: "oversized_or_complex_unit",
					Region:       path + ":" + fn.Name.Name,
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

func parseRepository(ctx context.Context, repo gitx.Repository, revision string, paths []string) (map[string]*ast.File, map[string][]caller, []string, error) {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	callers := map[string][]caller{}
	for _, path := range paths {
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
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
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calledName(call.Fun)
				if name != "" {
					callers[name] = append(callers[name], caller{Path: path, Symbol: fn.Name.Name})
				}
				return true
			})
		}
	}
	return parsed, callers, typeCheck(fset, parsed), nil
}

func typeCheck(fset *token.FileSet, parsed map[string]*ast.File) []string {
	packages := map[string][]*ast.File{}
	for path, file := range parsed {
		directory := filepath.Dir(path)
		key := directory + ":" + file.Name.Name
		packages[key] = append(packages[key], file)
	}
	var diagnostics []string
	for key, files := range packages {
		configuration := types.Config{
			Importer: importer.Default(),
			Error: func(err error) {
				diagnostics = append(diagnostics, key+": "+err.Error())
			},
		}
		_, _ = configuration.Check(key, fset, files, nil)
	}
	slices.Sort(diagnostics)
	return diagnostics
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

func calledName(expr ast.Expr) string {
	switch n := expr.(type) {
	case *ast.Ident:
		return n.Name
	case *ast.SelectorExpr:
		return n.Sel.Name
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
