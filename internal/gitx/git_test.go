package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeDigestAndDetachedExecutionTree(t *testing.T) {
	path := t.TempDir()
	gitCommand(t, path, "init", "-q")
	gitCommand(t, path, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, path, "config", "user.name", "Simpleton Test")
	file := filepath.Join(path, "value.txt")
	if err := os.WriteFile(file, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", ".")
	gitCommand(t, path, "commit", "-qm", "base")
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := repository.WorktreeDigest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty, err := repository.WorktreeDigest(context.Background())
	if err != nil || clean == dirty {
		t.Fatalf("worktree change did not invalidate digest: clean=%s dirty=%s err=%v", clean, dirty, err)
	}
	revision := gitCommand(t, path, "rev-parse", "HEAD")
	detached, cleanup, err := repository.DetachedWorktree(context.Background(), revision)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	content, err := os.ReadFile(filepath.Join(detached, "value.txt"))
	if err != nil || string(content) != "base" {
		t.Fatalf("detached execution tree is not the requested commit: %q err=%v", content, err)
	}
}

func gitCommand(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
