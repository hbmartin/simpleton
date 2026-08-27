package store

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
)

func TestOpenAcceptsRelativeRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	state, err := Open(filepath.Join("relative", "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	if _, err := os.Stat(filepath.Join("relative", "state", "simpleton.db")); err != nil {
		t.Fatalf("relative database path was not created: %v", err)
	}
}

func TestSQLiteFileURLHandlesWindowsPaths(t *testing.T) {
	tests := []struct {
		filename string
		path     string
	}{
		{filename: `C:\state dir\simpleton.db`, path: "/C:/state dir/simpleton.db"},
		{filename: `\\server\share\simpleton.db`, path: "//server/share/simpleton.db"},
	}
	for _, test := range tests {
		databaseURL := sqliteFileURL(test.filename)
		parsed, err := url.Parse(databaseURL.String())
		if err != nil {
			t.Fatalf("parse SQLite URL for %q: %v", test.filename, err)
		}
		if parsed.Scheme != "file" || parsed.Host != "" || parsed.Path != test.path {
			t.Fatalf("invalid SQLite URL for %q: %s", test.filename, databaseURL.String())
		}
		if strings.HasPrefix(databaseURL.String(), "file://server/") {
			t.Fatalf("SQLite URL retained a rejected UNC authority: %s", databaseURL.String())
		}
	}
}

func TestContentAddressedEvidenceAndOutcome(t *testing.T) {
	state, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	first, err := state.PutObject([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := state.PutObject([]byte("same"))
	if err != nil || first != second {
		t.Fatalf("content address must deduplicate: %q %q %v", first, second, err)
	}
	path, err := state.ObjectPath(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	pack := domain.EvidencePack{
		RunID: "run-1", CreatedAt: time.Unix(0, 0).UTC(), SemanticAdvisory: true,
		Provenance:     domain.Provenance{Repository: "/repo", BaseRevision: "base", HeadRevision: "head"},
		Methods:        []domain.MethodResult{{ID: "method", Status: domain.StatusRan, DurationMS: 10}},
		Scoping:        domain.ScopingPack{Opportunities: []domain.Opportunity{{ID: "opportunity", Rank: 0.5}}},
		AdvisoryReview: domain.AdvisoryReview{Status: domain.StatusUnsupported},
	}
	if _, err := state.SaveEvidence(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	// Force database/sql to discard idle connections so the next statement uses
	// a fresh driver connection. Foreign keys must be enabled by the DSN there too.
	state.db.SetMaxIdleConns(0)
	accepted := true
	if err := state.RecordOutcome(context.Background(), Outcome{RunID: "run-1", Accepted: &accepted}); err != nil {
		t.Fatal(err)
	}
	if err := state.RecordOutcome(context.Background(), Outcome{RunID: "missing"}); err == nil {
		t.Fatal("foreign-key enforcement should reject an unknown run")
	}
	for table, expected := range map[string]int{"method_results": 1, "opportunities": 1, "managed_reviews": 1} {
		var count int
		if err := state.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE run_id = ?`, "run-1").Scan(&count); err != nil || count != expected {
			t.Fatalf("unexpected %s count=%d err=%v", table, count, err)
		}
	}
}
