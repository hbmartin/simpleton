package gitx

import (
	"bytes"
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

func TestFilesAtReadsMultipleBlobsInOneBatch(t *testing.T) {
	path := t.TempDir()
	gitCommand(t, path, "init", "-q")
	gitCommand(t, path, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, path, "config", "user.name", "Simpleton Test")
	if err := os.WriteFile(filepath.Join(path, "first.txt"), []byte("first\nvalue"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "second.txt"), []byte{0, 1, 2, '\n'}, 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", ".")
	gitCommand(t, path, "commit", "-qm", "sample")
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	revision := gitCommand(t, path, "rev-parse", "HEAD")
	files, err := repository.FilesAt(context.Background(), revision, []string{"first.txt", "second.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if string(files["first.txt"]) != "first\nvalue" || !bytes.Equal(files["second.txt"], []byte{0, 1, 2, '\n'}) {
		t.Fatalf("batch blob contents changed: %#v", files)
	}
}

func TestFilesAtRejectsRevisionLineBreaksBeforePaths(t *testing.T) {
	_, err := (Repository{}).FilesAt(context.Background(), "HEAD\nsecond-request", []string{"/absolute"})
	if err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("revision line break was not rejected first: %v", err)
	}
	_, err = (Repository{}).FilesAt(context.Background(), "HEAD\x00second-request", []string{"relative"})
	if err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("revision NUL was not rejected: %v", err)
	}
}

func TestFilesAtOmitsIndividualNonBlobAndMissingPath(t *testing.T) {
	path := t.TempDir()
	gitCommand(t, path, "init", "-q")
	gitCommand(t, path, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, path, "config", "user.name", "Simpleton Test")
	if err := os.Mkdir(filepath.Join(path, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "directory", "value.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", ".")
	gitCommand(t, path, "commit", "-qm", "sample")
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	files, err := repository.FilesAt(context.Background(), "HEAD", []string{"directory", "missing\n.txt", "directory/value.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || string(files["directory/value.txt"]) != "value" {
		t.Fatalf("per-path failures affected valid blobs: %#v", files)
	}
}

func TestWorktreeDigestHashesDanglingSymlinkTarget(t *testing.T) {
	path := t.TempDir()
	gitCommand(t, path, "init", "-q")
	gitCommand(t, path, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, path, "config", "user.name", "Simpleton Test")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", "tracked.txt")
	gitCommand(t, path, "commit", "-qm", "base")
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(path, "dangling")
	if err := os.Symlink("missing-one", link); err != nil {
		t.Fatal(err)
	}
	first, err := repository.WorktreeDigest(context.Background())
	if err != nil {
		t.Fatalf("dangling symlink must be digestible: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-two", link); err != nil {
		t.Fatal(err)
	}
	second, err := repository.WorktreeDigest(context.Background())
	if err != nil || first == second {
		t.Fatalf("symlink target must affect digest: first=%s second=%s err=%v", first, second, err)
	}
}

func TestWorktreeDigestFramesUntrackedContents(t *testing.T) {
	first := repositoryWithUntrackedFiles(t, map[string][]byte{
		"a": append(append([]byte("x"), []byte("b\x00regular\x00")...), 'y'),
		"b": []byte("z"),
	})
	second := repositoryWithUntrackedFiles(t, map[string][]byte{
		"a": []byte("x"),
		"b": []byte("yb\x00regular\x00z"),
	})
	firstDigest, err := first.WorktreeDigest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := second.WorktreeDigest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatalf("distinct framed worktrees collided: %s", firstDigest)
	}
}

func TestWorktreeDigestIncludesNestedRepositoryState(t *testing.T) {
	path := t.TempDir()
	gitCommand(t, path, "init", "-q")
	gitCommand(t, path, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, path, "config", "user.name", "Simpleton Test")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", "tracked.txt")
	gitCommand(t, path, "commit", "-qm", "base")
	nested := filepath.Join(path, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, nested, "init", "-q")
	gitCommand(t, nested, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, nested, "config", "user.name", "Simpleton Test")
	nestedFile := filepath.Join(nested, "value.txt")
	if err := os.WriteFile(nestedFile, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, nested, "add", "value.txt")
	gitCommand(t, nested, "commit", "-qm", "nested")
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := repository.WorktreeDigest(context.Background())
	if err != nil {
		t.Fatalf("nested repository aborted digesting: %v", err)
	}
	if err := os.WriteFile(nestedFile, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := repository.WorktreeDigest(context.Background())
	if err != nil || first == second {
		t.Fatalf("nested repository state did not affect digest: first=%s second=%s err=%v", first, second, err)
	}
}

func TestWorktreeDigestRejectsOrdinaryUntrackedDirectory(t *testing.T) {
	path := t.TempDir()
	gitCommand(t, path, "init", "-q")
	gitCommand(t, path, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, path, "config", "user.name", "Simpleton Test")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", ".")
	gitCommand(t, path, "commit", "-qm", "base")
	if err := os.Mkdir(filepath.Join(path, "ordinary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "ordinary", "value.txt"), []byte("untracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	var digest bytes.Buffer
	err := writeWorktreeEntry(context.Background(), &digest, filepath.Join(path, "ordinary"), worktreeDigestLimits{maxFileBytes: 1024, maxNestedDepth: 1}, 0)
	if err == nil || !strings.Contains(err.Error(), "independent repository root") {
		t.Fatalf("ordinary directory was treated as a nested repository: %v", err)
	}
}

func TestWorktreeDigestEnforcesFileAndNestedRepositoryLimits(t *testing.T) {
	repository := repositoryWithUntrackedFiles(t, map[string][]byte{"large.bin": []byte("1234")})
	_, err := repository.worktreeDigest(context.Background(), worktreeDigestLimits{maxFileBytes: 3, maxNestedDepth: 1}, 0)
	if err == nil || !strings.Contains(err.Error(), "digest limit") {
		t.Fatalf("oversized untracked file was not rejected: %v", err)
	}

	path := t.TempDir()
	gitCommand(t, path, "init", "-q")
	gitCommand(t, path, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, path, "config", "user.name", "Simpleton Test")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", ".")
	gitCommand(t, path, "commit", "-qm", "root")
	nested := filepath.Join(path, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, nested, "init", "-q")
	gitCommand(t, nested, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, nested, "config", "user.name", "Simpleton Test")
	if err := os.WriteFile(filepath.Join(nested, "value.txt"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, nested, "add", ".")
	gitCommand(t, nested, "commit", "-qm", "nested")
	repository, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repository.worktreeDigest(context.Background(), worktreeDigestLimits{maxFileBytes: 1024, maxNestedDepth: 0}, 0)
	if err == nil || !strings.Contains(err.Error(), "depth exceeds") {
		t.Fatalf("nested repository depth limit was not enforced: %v", err)
	}
}

func repositoryWithUntrackedFiles(t *testing.T, files map[string][]byte) Repository {
	t.Helper()
	path := t.TempDir()
	gitCommand(t, path, "init", "-q")
	gitCommand(t, path, "config", "user.email", "simpleton@example.invalid")
	gitCommand(t, path, "config", "user.name", "Simpleton Test")
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", "tracked.txt")
	gitCommand(t, path, "commit", "-qm", "base")
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(path, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return repository
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
