package gitx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const filesystemRetryDelay = 10 * time.Millisecond

var errFileChangedWhileHashing = errors.New("file changed while it was being hashed")

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

// FilesAt reads repository blobs through one git cat-file batch process. The
// returned map omits paths that do not resolve to blobs at the revision.
func (r Repository) FilesAt(ctx context.Context, revision string, paths []string) (map[string][]byte, error) {
	if strings.ContainsAny(revision, "\x00\r\n") {
		return nil, errors.New("git revision must contain no NUL, carriage returns, or newlines")
	}
	result := make(map[string][]byte, len(paths))
	batchPaths := make([]string, 0, len(paths))
	var input strings.Builder
	for _, path := range paths {
		if strings.ContainsRune(path, 0) || filepath.IsAbs(path) {
			return nil, errors.New("git path must be relative and contain no NUL")
		}
		if strings.ContainsAny(path, "\r\n") {
			content, err := r.FileAt(ctx, revision, path)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			result[path] = content
			continue
		}
		batchPaths = append(batchPaths, path)
		input.WriteString(revision)
		input.WriteByte(':')
		input.WriteString(filepath.ToSlash(path))
		input.WriteByte('\n')
	}
	if len(batchPaths) == 0 {
		return result, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", r.Path, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(input.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open git cat-file --batch output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start git cat-file --batch: %w", err)
	}
	abort := func(cause error) (map[string][]byte, error) {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return nil, cause
	}
	reader := bufio.NewReader(stdout)
	for _, path := range batchPaths {
		header, err := reader.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return abort(ctx.Err())
			}
			return abort(fmt.Errorf("read git cat-file header for %q: %w", path, err))
		}
		fields := strings.Fields(strings.TrimSuffix(header, "\n"))
		if len(fields) >= 2 && fields[len(fields)-1] == "missing" {
			continue
		}
		if len(fields) < 3 {
			return abort(fmt.Errorf("unexpected git cat-file header for %q: %q", path, strings.TrimSpace(header)))
		}
		size, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err != nil || size < 0 {
			return abort(fmt.Errorf("invalid git object size for %q: %q", path, fields[len(fields)-1]))
		}
		if fields[len(fields)-2] != "blob" {
			if _, err := io.CopyN(io.Discard, reader, size); err != nil {
				return abort(fmt.Errorf("discard non-blob git object for %q: %w", path, err))
			}
			separator, err := reader.ReadByte()
			if err != nil || separator != '\n' {
				return abort(fmt.Errorf("invalid git object delimiter for %q", path))
			}
			continue
		}
		if size > int64(int(^uint(0)>>1)) {
			return abort(fmt.Errorf("git blob for %q is too large: %d", path, size))
		}
		content := make([]byte, int(size))
		if _, err := io.ReadFull(reader, content); err != nil {
			return abort(fmt.Errorf("read git blob for %q: %w", path, err))
		}
		separator, err := reader.ReadByte()
		if err != nil || separator != '\n' {
			return abort(fmt.Errorf("invalid git blob delimiter for %q", path))
		}
		result[path] = content
	}
	if err := cmd.Wait(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git cat-file --batch: %s", message)
	}
	return result, nil
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
	return r.worktreeDigest(ctx, worktreeDigestLimits{maxFileBytes: 256 << 20, maxNestedDepth: 8}, 0)
}

type worktreeDigestLimits struct {
	maxFileBytes   int64
	maxNestedDepth int
}

func (r Repository) worktreeDigest(ctx context.Context, limits worktreeDigestLimits, depth int) (string, error) {
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
	writeDigestField(hash, []byte("status"))
	writeDigestField(hash, status)
	writeDigestField(hash, []byte("unstaged"))
	writeDigestField(hash, unstaged)
	writeDigestField(hash, []byte("staged"))
	writeDigestField(hash, staged)
	for _, rawPath := range paths {
		if len(rawPath) == 0 {
			continue
		}
		path := string(rawPath)
		writeDigestField(hash, []byte("entry"))
		writeDigestField(hash, rawPath)
		if err := writeWorktreeEntry(ctx, hash, filepath.Join(r.Path, filepath.FromSlash(path)), limits, depth); err != nil {
			return "", fmt.Errorf("read untracked path %q: %w", path, err)
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func writeDigestField(writer io.Writer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write(value)
}

func writeWorktreeEntry(ctx context.Context, writer io.Writer, path string, limits worktreeDigestLimits, depth int) error {
	info, err := lstatWithRetry(ctx, path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := readlinkWithRetry(ctx, path)
		if err != nil {
			return err
		}
		writeDigestField(writer, []byte("symlink"))
		writeDigestField(writer, []byte(target))
		return nil
	}
	if info.IsDir() {
		nested := Repository{Path: path}
		topLevel, err := nested.run(ctx, "rev-parse", "--show-toplevel")
		if err != nil {
			return fmt.Errorf("unsupported directory entry: %w", err)
		}
		matches, err := pathsResolveEqual(path, strings.TrimRight(string(topLevel), "\r\n"))
		if err != nil {
			return fmt.Errorf("resolve nested repository root: %w", err)
		}
		if !matches {
			return errors.New("unsupported directory entry: directory is not an independent repository root")
		}
		if depth >= limits.maxNestedDepth {
			return fmt.Errorf("nested repository depth exceeds %d", limits.maxNestedDepth)
		}
		head, err := nested.run(ctx, "rev-parse", "--verify", "HEAD")
		if err != nil {
			head, err = nested.run(ctx, "symbolic-ref", "-q", "HEAD")
			if err != nil {
				head = []byte("unborn")
			}
		}
		worktree, err := nested.worktreeDigest(ctx, limits, depth+1)
		if err != nil {
			return err
		}
		writeDigestField(writer, []byte("git_repository"))
		writeDigestField(writer, bytes.TrimSpace(head))
		writeDigestField(writer, []byte(worktree))
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported file type %s", info.Mode().Type())
	}
	digest, size, err := digestRegularFile(ctx, path, limits.maxFileBytes)
	if err != nil {
		return err
	}
	var encodedSize [8]byte
	binary.BigEndian.PutUint64(encodedSize[:], uint64(size))
	writeDigestField(writer, []byte("regular_sha256"))
	writeDigestField(writer, encodedSize[:])
	writeDigestField(writer, digest)
	return nil
}

func digestRegularFile(ctx context.Context, path string, maxBytes int64) ([]byte, int64, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		digest, size, err := digestRegularFileOnce(ctx, path, maxBytes)
		if err == nil {
			return digest, size, nil
		}
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if !retryableFilesystemError(err) {
			return nil, 0, err
		}
		lastErr = err
		if attempt < 2 {
			if err := waitForFilesystemRetry(ctx); err != nil {
				return nil, 0, err
			}
		}
	}
	return nil, 0, fmt.Errorf("file remained unreadable after 3 attempts: %w", lastErr)
}

func digestRegularFileOnce(ctx context.Context, path string, maxBytes int64) ([]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = file.Close() }()
	before, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if before.Size() > maxBytes {
		return nil, 0, fmt.Errorf("file size %d exceeds the %d-byte worktree digest limit", before.Size(), maxBytes)
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > maxBytes {
				return nil, 0, fmt.Errorf("file exceeds the %d-byte worktree digest limit", maxBytes)
			}
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, 0, readErr
		}
	}
	after, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if total != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, 0, errFileChangedWhileHashing
	}
	return hash.Sum(nil), total, nil
}

func lstatWithRetry(ctx context.Context, path string) (os.FileInfo, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err == nil {
			return info, nil
		}
		if !retryableFilesystemError(err) {
			return nil, err
		}
		lastErr = err
		if attempt < 2 {
			if err := waitForFilesystemRetry(ctx); err != nil {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("lstat failed after 3 attempts: %w", lastErr)
}

func readlinkWithRetry(ctx context.Context, path string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		target, err := os.Readlink(path)
		if err == nil {
			return target, nil
		}
		if !retryableFilesystemError(err) {
			return "", err
		}
		lastErr = err
		if attempt < 2 {
			if err := waitForFilesystemRetry(ctx); err != nil {
				return "", err
			}
		}
	}
	return "", fmt.Errorf("readlink failed after 3 attempts: %w", lastErr)
}

func retryableFilesystemError(err error) bool {
	if errors.Is(err, errFileChangedWhileHashing) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && retryableFilesystemErrno(errno, runtime.GOOS) {
		return true
	}
	var temporary interface{ Temporary() bool }
	return errors.As(err, &temporary) && temporary.Temporary()
}

func retryableFilesystemErrno(errno syscall.Errno, goos string) bool {
	if goos == "windows" {
		// Windows file APIs return native Win32 error numbers. The syscall
		// package's ESTALE and EIO values are synthetic on Windows and cannot
		// match errors returned by those APIs.
		switch errno {
		case syscall.Errno(32), // ERROR_SHARING_VIOLATION
			syscall.Errno(33),   // ERROR_LOCK_VIOLATION
			syscall.Errno(54),   // ERROR_NETWORK_BUSY
			syscall.Errno(59),   // ERROR_UNEXP_NET_ERR
			syscall.Errno(64),   // ERROR_NETNAME_DELETED
			syscall.Errno(121),  // ERROR_SEM_TIMEOUT
			syscall.Errno(1237): // ERROR_RETRY
			return true
		default:
			return false
		}
	}
	return errno == syscall.ESTALE || errno == syscall.EIO
}

func pathsResolveEqual(left, right string) (bool, error) {
	resolve := func(path string) (string, error) {
		absolute, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(absolute)
	}
	resolvedLeft, err := resolve(left)
	if err != nil {
		return false, err
	}
	resolvedRight, err := resolve(right)
	if err != nil {
		return false, err
	}
	return filepath.Clean(resolvedLeft) == filepath.Clean(resolvedRight), nil
}

func waitForFilesystemRetry(ctx context.Context) error {
	timer := time.NewTimer(filesystemRetryDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
