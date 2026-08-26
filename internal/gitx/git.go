package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type ChangedFile struct {
	Status   string `json:"status"`
	Path     string `json:"path"`
	OldPath  string `json:"old_path,omitempty"`
	Language string `json:"language,omitempty"`
}

type Repository struct {
	Path string
}

func Open(path string) (Repository, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Repository{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Repository{}, err
	}
	if !info.IsDir() {
		return Repository{}, fmt.Errorf("repository path %q is not a directory", abs)
	}
	r := Repository{Path: abs}
	if _, err := r.run(context.Background(), "rev-parse", "--git-dir"); err != nil {
		return Repository{}, fmt.Errorf("%q is not a git repository: %w", abs, err)
	}
	return r, nil
}

func (r Repository) Resolve(ctx context.Context, revision string) (string, error) {
	if strings.TrimSpace(revision) == "" {
		return "", errors.New("revision is required")
	}
	out, err := r.run(ctx, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve revision %q: %w", revision, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (r Repository) TreeDigest(ctx context.Context, revision string) (string, error) {
	out, err := r.run(ctx, "rev-parse", revision+"^{tree}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r Repository) ChangedFiles(ctx context.Context, base, head string) ([]ChangedFile, error) {
	out, err := r.run(ctx, "diff", "--name-status", "-z", "--find-renames", base, head)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return []ChangedFile{}, nil
	}
	parts := bytes.Split(out, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	}
	var files []ChangedFile
	for n := 0; n < len(parts); {
		status := string(parts[n])
		n++
		if n >= len(parts) {
			return nil, errors.New("malformed git diff name-status output")
		}
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if n+1 >= len(parts) {
				return nil, errors.New("malformed rename/copy entry")
			}
			oldPath := string(parts[n])
			newPath := string(parts[n+1])
			n += 2
			files = append(files, ChangedFile{Status: status, Path: newPath, OldPath: oldPath, Language: LanguageForPath(newPath)})
			continue
		}
		path := string(parts[n])
		n++
		files = append(files, ChangedFile{Status: status, Path: path, Language: LanguageForPath(path)})
	}
	return files, nil
}

func (r Repository) FileAt(ctx context.Context, revision, path string) ([]byte, error) {
	if strings.ContainsRune(path, 0) || filepath.IsAbs(path) {
		return nil, errors.New("git path must be relative and contain no NUL")
	}
	return r.run(ctx, "show", revision+":"+filepath.ToSlash(path))
}

func (r Repository) Patch(ctx context.Context, base, head string) ([]byte, error) {
	return r.run(ctx, "diff", "--binary", "--no-ext-diff", base, head)
}

func (r Repository) ListFiles(ctx context.Context, revision string) ([]string, error) {
	out, err := r.run(ctx, "ls-tree", "-r", "--name-only", "-z", revision)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return []string{}, nil
	}
	parts := bytes.Split(out, []byte{0})
	files := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			files = append(files, string(part))
		}
	}
	return files, nil
}

func (r Repository) WorktreeDigest(ctx context.Context) (string, error) {
	status, err := r.run(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	unstaged, err := r.run(ctx, "diff", "--binary", "--no-ext-diff")
	if err != nil {
		return "", err
	}
	staged, err := r.run(ctx, "diff", "--cached", "--binary", "--no-ext-diff")
	if err != nil {
		return "", err
	}
	untrackedOutput, err := r.run(ctx, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	paths := bytes.Split(untrackedOutput, []byte{0})
	sort.Slice(paths, func(i, j int) bool { return bytes.Compare(paths[i], paths[j]) < 0 })
	hash := sha256.New()
	_, _ = hash.Write(status)
	_, _ = hash.Write(unstaged)
	_, _ = hash.Write(staged)
	for _, rawPath := range paths {
		if len(rawPath) == 0 {
			continue
		}
		path := string(rawPath)
		content, readErr := os.ReadFile(filepath.Join(r.Path, filepath.FromSlash(path)))
		if readErr != nil {
			return "", readErr
		}
		_, _ = hash.Write(rawPath)
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(content)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func (r Repository) DetachedWorktree(ctx context.Context, revision string) (string, func() error, error) {
	path, err := os.MkdirTemp("", "simpleton-worktree-*")
	if err != nil {
		return "", nil, err
	}
	if _, err := r.run(ctx, "worktree", "add", "--detach", "--force", path, revision); err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	cleanup := func() error {
		_, removeErr := r.run(context.Background(), "worktree", "remove", "--force", path)
		if removeErr != nil {
			return removeErr
		}
		return nil
	}
	return path, cleanup, nil
}

func (r Repository) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Path}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return out, nil
}

func LanguageForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".py", ".pyi":
		return "python"
	case ".swift":
		return "swift"
	default:
		return ""
	}
}
