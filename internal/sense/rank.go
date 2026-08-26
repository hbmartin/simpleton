package sense

import (
	"slices"

	"github.com/haroldmartin/simpleton/internal/domain"
)

func Rank(opportunities []domain.Opportunity) []domain.Opportunity {
	ranked := slices.Clone(opportunities)
	for index := range ranked {
		opportunity := &ranked[index]
		opportunity.Benefit = clamp(opportunity.Benefit)
		opportunity.ApplicabilityConfidence = clamp(opportunity.ApplicabilityConfidence)
		opportunity.Risk = clamp(opportunity.Risk)
		opportunity.ReviewEffort = clamp(opportunity.ReviewEffort)
		opportunity.Rank = opportunity.Benefit * opportunity.ApplicabilityConfidence /
			(1 + opportunity.Risk + opportunity.ReviewEffort)
	}
	slices.SortFunc(ranked, func(a, b domain.Opportunity) int {
		switch {
		case a.Rank > b.Rank:
			return -1
		case a.Rank < b.Rank:
			return 1
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	return ranked
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
