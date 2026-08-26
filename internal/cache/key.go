package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"

	"github.com/haroldmartin/simpleton/internal/domain"
	"github.com/haroldmartin/simpleton/internal/gitx"
)

// Inputs names every dimension whose change invalidates reusable evidence.
// The budget is included because truncation can change both coverage and status.
type Inputs struct {
	BaseTree     string            `json:"base_tree"`
	HeadTree     string            `json:"head_tree"`
	Worktree     string            `json:"worktree"`
	Lockfiles    map[string]string `json:"lockfiles"`
	Toolchains   map[string]string `json:"toolchains"`
	Environment  string            `json:"environment"`
	Contract     string            `json:"contract"`
	Policy       string            `json:"policy"`
	Comparator   string            `json:"comparator"`
	Seed         string            `json:"seed"`
	Budgets      map[string]string `json:"budgets"`
	CoreVersion  string            `json:"core_version"`
	PackVersions map[string]string `json:"pack_versions"`
}

func Key(inputs Inputs) (string, error) {
	return domain.DigestJSON(inputs)
}

func LockfileDigests(ctx context.Context, repository gitx.Repository, revision string) (map[string]string, error) {
	paths, err := repository.ListFiles(ctx, revision)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	sort.Strings(paths)
	for _, path := range paths {
		if !isLockfile(path) {
			continue
		}
		content, err := repository.FileAt(ctx, revision, path)
		if err != nil {
			return nil, err
		}
		result[path] = digestContent(content)
	}
	return result, nil
}

func digestContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func isLockfile(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	switch name {
	case "go.sum", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "poetry.lock", "pdm.lock", "pipfile.lock", "uv.lock", "package.resolved":
		return true
	default:
		return false
	}
}
