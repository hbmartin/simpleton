package sense

import (
	"testing"

	"github.com/haroldmartin/simpleton/internal/domain"
)

func TestRankBalancesValueAndVerifiability(t *testing.T) {
	items := []domain.Opportunity{
		{ID: "risky", Benefit: 1, ApplicabilityConfidence: .2, Risk: 1, ReviewEffort: 1},
		{ID: "useful", Benefit: .8, ApplicabilityConfidence: .9, Risk: .2, ReviewEffort: .2},
	}
	ranked := Rank(items)
	if ranked[0].ID != "useful" {
		t.Fatalf("unexpected rank order: %#v", ranked)
	}
}
