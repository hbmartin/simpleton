package planner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/haroldmartin/simpleton/internal/config"
	"github.com/haroldmartin/simpleton/internal/domain"
	"github.com/haroldmartin/simpleton/internal/execution"
)

type fakeRunner struct {
	result execution.CommandResult
	err    error
}

func (f fakeRunner) Run(context.Context, string, domain.ExecutionPolicy, string, []string) (execution.CommandResult, error) {
	return f.result, f.err
}

func TestAnalyzeProposesIntentAndCanStillBlockBuildRegression(t *testing.T) {
	repository, base, head := testRepository(t, "before\n", "after\n")
	policy := config.DefaultPolicy()
	policy.Mode = "blocking"
	policy.Commands = []domain.CommandSpec{{
		ID: "build", Category: domain.CategoryBuildRegression, Command: []string{"go", "build", "./..."}, Image: "example.invalid/toolchain@sha256:abc",
	}}
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := config.WriteYAML(policyPath, policy); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "run")
	planner := Planner{
		Runner: fakeRunner{result: execution.CommandResult{ExitCode: 1, Stderr: "compile failed"}},
		Now:    func() time.Time { return time.Unix(10, 0) },
	}
	result, err := planner.Analyze(context.Background(), Request{
		Repository: repository, Base: base, Head: head, Output: output, PolicyPath: policyPath, CoreVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Pack.SemanticAdvisory || result.ExitCode != 1 || len(result.Pack.Blockers) != 1 {
		t.Fatalf("expected advisory semantics and deterministic blocker: %#v", result)
	}
	if result.Pack.Blockers[0].Category != domain.CategoryBuildRegression {
		t.Fatalf("unexpected blocker: %#v", result.Pack.Blockers)
	}
	if result.Pack.ProposedIntentPath == "" {
		t.Fatal("missing proposed Change Intent")
	}
	for _, name := range []string{"evidence-pack.json", "scoping-pack.json", "backlog.md", "backlog.html", "simpleton.db"} {
		if _, err := os.Stat(filepath.Join(output, name)); err != nil {
			t.Fatalf("missing output %s: %v", name, err)
		}
	}
}

func TestBudgetExhaustionCannotBlock(t *testing.T) {
	repository, base, head := testRepository(t, "before\n", "after\n")
	policy := config.DefaultPolicy()
	policy.Mode = "blocking"
	policy.Commands = []domain.CommandSpec{{ID: "tests", Category: domain.CategoryTestRegression, Command: []string{"test"}, Image: "image@sha256:abc"}}
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := config.WriteYAML(policyPath, policy); err != nil {
		t.Fatal(err)
	}
	result, err := (Planner{Runner: fakeRunner{err: context.DeadlineExceeded}}).Analyze(context.Background(), Request{
		Repository: repository, Base: base, Head: head, Output: filepath.Join(t.TempDir(), "run"), PolicyPath: policyPath, CoreVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || len(result.Pack.Blockers) != 0 || result.Pack.Methods[1].Status != domain.StatusBudgetExhausted {
		t.Fatalf("budget exhaustion must be nonblocking: %#v", result)
	}
}

func testRepository(t *testing.T, before, after string) (string, string, string) {
	t.Helper()
	repository := t.TempDir()
	runGit(t, repository, "init", "-q")
	runGit(t, repository, "config", "user.email", "simpleton@example.invalid")
	runGit(t, repository, "config", "user.name", "Simpleton Test")
	path := filepath.Join(repository, "README.md")
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "-qm", "base")
	base := runGit(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "commit", "-qam", "head")
	head := runGit(t, repository, "rev-parse", "HEAD")
	return repository, base, head
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
