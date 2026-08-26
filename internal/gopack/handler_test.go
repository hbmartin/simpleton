package gopack

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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

func TestModuleMetadataIncludesStandardLibraryExports(t *testing.T) {
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
	metadata := loadPackageMetadata(context.Background(), repository, revision)
	if metadata.exports["os"] == "" {
		t.Fatalf("standard library export data is missing: %#v", metadata.diagnostics)
	}
	if metadata.pathByDirectory["."] != "example.invalid/metadata" {
		t.Fatalf("module package path is missing: %#v", metadata.pathByDirectory)
	}
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
