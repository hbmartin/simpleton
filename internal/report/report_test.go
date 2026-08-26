package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haroldmartin/simpleton/internal/domain"
)

func TestWriteAllProducesEvidenceAndStaticSenseReports(t *testing.T) {
	root := t.TempDir()
	pack := domain.EvidencePack{
		RunID: "run", Scoping: domain.ScopingPack{SchemaVersion: "1", RunID: "run", Opportunities: []domain.Opportunity{{
			ID: "o1", Language: "go", Category: "complex", Region: "a.go:F", Rank: 0.8,
		}}},
	}
	if err := WriteAll(root, pack); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"evidence-pack.json", "scoping-pack.json", "backlog.md", "backlog.html"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	markdown, err := os.ReadFile(filepath.Join(root, "backlog.md"))
	if err != nil || !strings.Contains(string(markdown), "a.go:F") {
		t.Fatalf("unexpected backlog report: %s err=%v", markdown, err)
	}
}
