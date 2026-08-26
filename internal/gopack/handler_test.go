package gopack

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
	"github.com/haroldmartin/simpleton/internal/gitx"
	"github.com/haroldmartin/simpleton/internal/packrpc"
)

func TestAnalyzePrefersUnchangedCallerBoundary(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "simpleton@example.invalid")
	git(t, repo, "config", "user.name", "Simpleton Test")
	write(t, filepath.Join(repo, "go.mod"), "module example.invalid/calc\n\ngo 1.24\n")
	write(t, filepath.Join(repo, "dep", "value.go"), "package dep\n\nfunc Adjust(value int) int { return value }\n")
	write(t, filepath.Join(repo, "calc.go"), "package calc\n\nimport \"example.invalid/calc/dep\"\n\nfunc Add(a, b int) int { return dep.Adjust(a + b) }\n")
	write(t, filepath.Join(repo, "caller.go"), "package calc\n\nfunc Public(x int) int { return Add(x, 1) }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "calc.go"), "package calc\n\nimport \"example.invalid/calc/dep\"\n\nfunc Add(a, b int) int { return dep.Adjust(a - b) }\n")
	git(t, repo, "add", "calc.go")
	git(t, repo, "commit", "-qm", "candidate")
	head := git(t, repo, "rev-parse", "HEAD")

	result, err := (Handler{}).Analyze(context.Background(), packrpc.AnalyzeParams{
		Repository: repo, BaseRevision: base, HeadRevision: head,
		ChangedFiles: []gitx.ChangedFile{{Path: "calc.go", Language: "go"}}, BudgetMS: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Targets) != 1 || result.Targets[0].Symbol != "Add" {
		t.Fatalf("unexpected targets: %#v", result.Targets)
	}
	boundaries := result.Targets[0].ObservationCandidates
	if len(boundaries) < 2 || boundaries[0].Kind != "unchanged_caller" || boundaries[0].Symbol != "Public" {
		t.Fatalf("unchanged caller should be preferred: %#v", boundaries)
	}
	if result.Methods[1].ID != "go_type_analysis" || result.Methods[1].Status != domain.StatusRan {
		t.Fatalf("type analysis should run cleanly: %#v", result.Methods)
	}
}

func TestAnalyzeQualifiesReceiverMethodsAndResolvesCallers(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "simpleton@example.invalid")
	git(t, repo, "config", "user.name", "Simpleton Test")
	write(t, filepath.Join(repo, "go.mod"), "module example.invalid/methods\n\ngo 1.24\n")
	body := ""
	for range 30 {
		body += "\t_ = 1\n"
	}
	methods := "package methods\n\ntype A struct{}\ntype B struct{}\n\nfunc (A) Run() {\n" + body + "}\n\nfunc (B) Run() {\n" + body + "}\n"
	write(t, filepath.Join(repo, "methods.go"), methods)
	write(t, filepath.Join(repo, "caller.go"), "package methods\n\nfunc Public() { var value A; value.Run() }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "methods.go"), methods+"\n")
	git(t, repo, "add", "methods.go")
	git(t, repo, "commit", "-qm", "candidate")
	head := git(t, repo, "rev-parse", "HEAD")

	result, err := (Handler{}).Analyze(context.Background(), packrpc.AnalyzeParams{
		Repository: repo, BaseRevision: base, HeadRevision: head,
		ChangedFiles: []gitx.ChangedFile{{Path: "methods.go", Language: "go"}}, BudgetMS: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Targets) != 2 || result.Targets[0].Symbol != "A.Run" || result.Targets[1].Symbol != "B.Run" {
		t.Fatalf("receiver methods were not qualified: %#v", result.Targets)
	}
	if result.Targets[0].ID == result.Targets[1].ID {
		t.Fatalf("receiver methods share target ID %q", result.Targets[0].ID)
	}
	if len(result.Targets[0].ObservationCandidates) == 0 || result.Targets[0].ObservationCandidates[0].Symbol != "Public" {
		t.Fatalf("A.Run should have the resolved unchanged caller: %#v", result.Targets[0].ObservationCandidates)
	}
	for _, boundary := range result.Targets[1].ObservationCandidates {
		if boundary.Kind == "unchanged_caller" {
			t.Fatalf("B.Run acquired an unrelated caller: %#v", result.Targets[1].ObservationCandidates)
		}
	}
	if len(result.Opportunities) != 2 || result.Opportunities[0].ID == result.Opportunities[1].ID {
		t.Fatalf("receiver method opportunities must be distinct: %#v", result.Opportunities)
	}
}

func TestAnalyzeResolvesInterfaceDispatchToConcreteMethod(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "simpleton@example.invalid")
	git(t, repo, "config", "user.name", "Simpleton Test")
	write(t, filepath.Join(repo, "go.mod"), "module example.invalid/dispatch\n\ngo 1.24\n")
	write(t, filepath.Join(repo, "method.go"), "package dispatch\n\ntype Runner interface { Run() }\ntype Task struct{}\nfunc (Task) Run() {}\n")
	write(t, filepath.Join(repo, "caller.go"), "package dispatch\n\nfunc Public(value Runner) { value.Run() }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "method.go"), "package dispatch\n\ntype Runner interface { Run() }\ntype Task struct{}\nfunc (Task) Run() { _ = 1 }\n")
	git(t, repo, "commit", "-qam", "candidate")
	head := git(t, repo, "rev-parse", "HEAD")

	result, err := (Handler{}).Analyze(context.Background(), packrpc.AnalyzeParams{
		Repository: repo, BaseRevision: base, HeadRevision: head,
		ChangedFiles: []gitx.ChangedFile{{Path: "method.go", Language: "go"}}, BudgetMS: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range result.Targets {
		if target.Symbol != "Task.Run" {
			continue
		}
		for _, boundary := range target.ObservationCandidates {
			if boundary.Kind == "unchanged_caller" && boundary.Symbol == "Public" {
				return
			}
		}
		t.Fatalf("interface-dispatched caller was not resolved: %#v", target.ObservationCandidates)
	}
	t.Fatalf("Task.Run target was not found: %#v", result.Targets)
}

func TestAnalyzeResolvesInterfaceDispatchToPromotedMethod(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "simpleton@example.invalid")
	git(t, repo, "config", "user.name", "Simpleton Test")
	write(t, filepath.Join(repo, "go.mod"), "module example.invalid/promoted\n\ngo 1.24\n")
	write(t, filepath.Join(repo, "method.go"), "package promoted\n\ntype Runner interface { Run(); Stop() }\ntype base struct{}\nfunc (base) Run() {}\ntype Task struct { base }\nfunc (Task) Stop() {}\n")
	write(t, filepath.Join(repo, "caller.go"), "package promoted\n\nfunc Public(value Runner) { value.Run() }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "method.go"), "package promoted\n\ntype Runner interface { Run(); Stop() }\ntype base struct{}\nfunc (base) Run() { _ = 1 }\ntype Task struct { base }\nfunc (Task) Stop() {}\n")
	git(t, repo, "commit", "-qam", "candidate")
	head := git(t, repo, "rev-parse", "HEAD")

	result, err := (Handler{}).Analyze(context.Background(), packrpc.AnalyzeParams{
		Repository: repo, BaseRevision: base, HeadRevision: head,
		ChangedFiles: []gitx.ChangedFile{{Path: "method.go", Language: "go"}}, BudgetMS: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range result.Targets {
		if target.Symbol != "base.Run" {
			continue
		}
		for _, boundary := range target.ObservationCandidates {
			if boundary.Kind == "unchanged_caller" && boundary.Symbol == "Public" {
				return
			}
		}
		t.Fatalf("promoted interface implementation lost its caller: %#v", target.ObservationCandidates)
	}
	t.Fatalf("base.Run target was not found: %#v", result.Targets)
}

func TestAnalyzeResolvesCallersAcrossRepositoryPackages(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "simpleton@example.invalid")
	git(t, repo, "config", "user.name", "Simpleton Test")
	write(t, filepath.Join(repo, "go.mod"), "module example.invalid/localimport\n\ngo 1.24\n")
	write(t, filepath.Join(repo, "dep", "value.go"), "package dep\n\nfunc Adjust(value int) int { return value }\n")
	write(t, filepath.Join(repo, "caller.go"), "package localimport\n\nimport \"example.invalid/localimport/dep\"\n\nfunc Public(value int) int { return dep.Adjust(value) }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "dep", "value.go"), "package dep\n\nfunc Adjust(value int) int { return value + 1 }\n")
	git(t, repo, "commit", "-qam", "candidate")
	head := git(t, repo, "rev-parse", "HEAD")

	result, err := (Handler{}).Analyze(context.Background(), packrpc.AnalyzeParams{
		Repository: repo, BaseRevision: base, HeadRevision: head,
		ChangedFiles: []gitx.ChangedFile{{Path: "dep/value.go", Language: "go"}}, BudgetMS: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Targets) != 1 || result.Targets[0].Symbol != "Adjust" {
		t.Fatalf("unexpected targets: %#v", result.Targets)
	}
	boundaries := result.Targets[0].ObservationCandidates
	if len(boundaries) == 0 || boundaries[0].Kind != "unchanged_caller" || boundaries[0].Symbol != "Public" {
		t.Fatalf("cross-package caller was not resolved: %#v", boundaries)
	}
	if result.Methods[1].Status != domain.StatusRan {
		t.Fatalf("type analysis should run cleanly: %#v", result.Methods)
	}
}

func TestAnalyzeSortsChangedFiles(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "simpleton@example.invalid")
	git(t, repo, "config", "user.name", "Simpleton Test")
	write(t, filepath.Join(repo, "go.mod"), "module example.invalid/order\n\ngo 1.24\n")
	write(t, filepath.Join(repo, "a.go"), "package order\n\nfunc A() int { return 1 }\n")
	write(t, filepath.Join(repo, "z.go"), "package order\n\nfunc Z() int { return 1 }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "a.go"), "package order\n\nfunc A() int { return 2 }\n")
	write(t, filepath.Join(repo, "z.go"), "package order\n\nfunc Z() int { return 2 }\n")
	git(t, repo, "commit", "-qam", "candidate")
	head := git(t, repo, "rev-parse", "HEAD")

	result, err := (Handler{}).Analyze(context.Background(), packrpc.AnalyzeParams{
		Repository: repo, BaseRevision: base, HeadRevision: head,
		ChangedFiles: []gitx.ChangedFile{{Path: "z.go", Language: "go"}, {Path: "a.go", Language: "go"}}, BudgetMS: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Targets) != 2 || result.Targets[0].File != "a.go" || result.Targets[1].File != "z.go" {
		t.Fatalf("targets are not deterministic: %#v", result.Targets)
	}
}

func TestAnalyzeSkipsUnrelatedPackageTypeChecking(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "simpleton@example.invalid")
	git(t, repo, "config", "user.name", "Simpleton Test")
	write(t, filepath.Join(repo, "go.mod"), "module example.invalid/scoped\n\ngo 1.24\n")
	write(t, filepath.Join(repo, "dep", "value.go"), "package dep\n\nfunc Adjust(value int) int { return value }\n")
	write(t, filepath.Join(repo, "caller", "caller.go"), "package caller\n\nimport \"example.invalid/scoped/dep\"\nfunc Public(value int) int { return dep.Adjust(value) }\n")
	write(t, filepath.Join(repo, "unrelated", "broken.go"), "package unrelated\n\nvar Broken int = \"not an int\"\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "dep", "value.go"), "package dep\n\nfunc Adjust(value int) int { return value + 1 }\n")
	git(t, repo, "commit", "-qam", "candidate")
	head := git(t, repo, "rev-parse", "HEAD")

	result, err := (Handler{}).Analyze(context.Background(), packrpc.AnalyzeParams{
		Repository: repo, BaseRevision: base, HeadRevision: head,
		ChangedFiles: []gitx.ChangedFile{{Path: "dep/value.go", Language: "go"}}, BudgetMS: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Methods[1].Status != domain.StatusRan {
		t.Fatalf("unrelated package diagnostics contaminated scoped analysis: %#v", result.Methods[1])
	}
	if len(result.Targets) != 1 || len(result.Targets[0].ObservationCandidates) == 0 || result.Targets[0].ObservationCandidates[0].Symbol != "Public" {
		t.Fatalf("reverse-dependent caller was not included: %#v", result.Targets)
	}
}

func TestModuleMetadataDerivesPackagePathsWithoutCompilingRepository(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "simpleton@example.invalid")
	git(t, repo, "config", "user.name", "Simpleton Test")
	write(t, filepath.Join(repo, "go.mod"), "module example.invalid/metadata\n\ngo 1.24\n")
	write(t, filepath.Join(repo, "main.go"), "package metadata\n\nimport \"os\"\n\nvar _ = os.ErrNotExist\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	revision := git(t, repo, "rev-parse", "HEAD")
	repository, err := gitx.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "go-was-executed")
	fakeGo := filepath.Join(fakeBin, "go")
	if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\n: > \"$SIMPLETON_GO_MARKER\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIMPLETON_GO_MARKER", marker)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	metadata := loadPackageMetadata(context.Background(), repository, revision, []string{"go.mod", "main.go"})
	if got := metadata.packagePath(".", "fallback"); got != "example.invalid/metadata" {
		t.Fatalf("module package path is missing: got=%q diagnostics=%#v", got, metadata.diagnostics)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("metadata discovery executed the candidate Go toolchain: %v", err)
	}
}

type blockingImporter struct {
	started chan struct{}
	release chan struct{}
}

func (b blockingImporter) Import(path string) (*types.Package, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.release
	return nil, fmt.Errorf("import %q was released", path)
}

type cancelAfterFirstErrContext struct {
	context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelAfterFirstErrContext) Err() error {
	err := c.Context.Err()
	if err == nil {
		c.once.Do(c.cancel)
	}
	return err
}

func TestTypeCheckHandlesCancellationBetweenPackageChecks(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", "package sample\nfunc Example() {}\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := &cancelAfterFirstErrContext{Context: baseCtx, cancel: cancel}
	_, _, _, err = typeCheckSynchronously(ctx, fset, map[string]*ast.File{"sample.go": file}, packageMetadata{}, map[string]bool{"sample.go": true}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("type checking returned the wrong cancellation error: %v", err)
	}
}

func TestTypeCheckReturnsWhenFallbackImporterIgnoresCancellation(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", "package sample\nimport _ \"blocked.invalid/dependency\"\nfunc Example() {}\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	importer := blockingImporter{started: make(chan struct{}, 1), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, _, _, err := typeCheckWithImporter(ctx, fset, map[string]*ast.File{"sample.go": file}, packageMetadata{}, map[string]bool{"sample.go": true}, importer)
		returned <- err
	}()
	<-importer.started
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("type checking returned the wrong cancellation error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("type checking did not return promptly after cancellation")
	}
	close(importer.release)
}

func git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(bytesTrimSpace(output))
}

func write(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bytesTrimSpace(value []byte) []byte {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\n' || value[start] == '\r' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
}
