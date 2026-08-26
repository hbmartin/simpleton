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
	write(t, filepath.Join(repo, "calc.go"), "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	write(t, filepath.Join(repo, "caller.go"), "package calc\n\nfunc Public(x int) int { return Add(x, 1) }\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "base")
	base := git(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "calc.go"), "package calc\n\nfunc Add(a, b int) int { return a - b }\n")
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
