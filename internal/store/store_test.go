package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
)

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
