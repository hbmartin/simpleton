package evaluation

import (
	"errors"
	"fmt"
	"math"
)

type QualitativeScore struct {
	BoundaryConstructibility int `json:"boundary_constructibility"`
	LegalInputConfidence     int `json:"legal_input_confidence"`
	ContractRelevance        int `json:"contract_relevance"`
	ReplayStability          int `json:"replay_stability"`
	InfrastructureEffort     int `json:"infrastructure_effort"`
	ReviewerUsefulness       int `json:"reviewer_usefulness"`
}

type Decision struct {
	Average        float64 `json:"average"`
	Disposition    string  `json:"disposition"`
	CriticalBelow3 bool    `json:"critical_below_3"`
}

func Decide(score QualitativeScore) (Decision, error) {
	values := []int{
		score.BoundaryConstructibility, score.LegalInputConfidence, score.ContractRelevance,
		score.ReplayStability, score.InfrastructureEffort, score.ReviewerUsefulness,
	}
	total := 0
	critical := false
	for _, value := range values {
		if value < 1 || value > 5 {
			return Decision{}, errors.New("all qualitative dimensions must be scored from 1 to 5")
		}
		total += value
		if value < 3 {
			critical = true
		}
	}
	decision := Decision{Average: float64(total) / float64(len(values)), CriticalBelow3: critical}
	switch {
	case decision.Average >= 4 && !critical:
		decision.Disposition = "continue"
	case decision.Average >= 3:
		decision.Disposition = "specialize"
	default:
		decision.Disposition = "defer"
	}
	return decision, nil
}

type PromotionSample struct {
	Targets       int `json:"targets"`
	NativeHits    int `json:"native_validated_witnesses"`
	GeneratedHits int `json:"generated_validated_witnesses"`
}

type PromotionDecision struct {
	Promote             bool    `json:"promote"`
	NativeYield         float64 `json:"native_yield"`
	GeneratedYield      float64 `json:"generated_yield"`
	ObservedLoss        float64 `json:"observed_loss"`
	OneSided95UpperLoss float64 `json:"one_sided_95_upper_loss"`
	MaximumAcceptedLoss float64 `json:"maximum_accepted_loss"`
	Reason              string  `json:"reason"`
}

// AssessPromotion uses the documented independent-proportion normal
// approximation. Pilot reports must retain raw target outcomes so a paired or
// exact analysis can replace it when the design supplies those data.
func AssessPromotion(sample PromotionSample) (PromotionDecision, error) {
	if sample.Targets <= 0 || sample.NativeHits < 0 || sample.GeneratedHits < 0 || sample.NativeHits > sample.Targets || sample.GeneratedHits > sample.Targets {
		return PromotionDecision{}, fmt.Errorf("invalid promotion sample: %#v", sample)
	}
	nativeYield := float64(sample.NativeHits) / float64(sample.Targets)
	generatedYield := float64(sample.GeneratedHits) / float64(sample.Targets)
	loss := nativeYield - generatedYield
	standardError := math.Sqrt(nativeYield*(1-nativeYield)/float64(sample.Targets) + generatedYield*(1-generatedYield)/float64(sample.Targets))
	upper := loss + 1.6448536269514722*standardError
	decision := PromotionDecision{
		NativeYield: nativeYield, GeneratedYield: generatedYield, ObservedLoss: loss,
		OneSided95UpperLoss: upper, MaximumAcceptedLoss: 0.20,
	}
	if sample.Targets < 30 {
		decision.Reason = "at least 30 targets are required for this language/change class"
		return decision, nil
	}
	if upper >= 0.20 {
		decision.Reason = "the one-sided 95% interval does not exclude a yield loss of 20 percentage points"
		return decision, nil
	}
	decision.Promote = true
	decision.Reason = "sample size and non-inferiority requirements are satisfied"
	return decision, nil
}
