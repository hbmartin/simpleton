package witness

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/haroldmartin/simpleton/internal/domain"
)

func TestStructuralJSONIgnoresMapOrdering(t *testing.T) {
	violated, _, err := Compare(domain.ComparatorSpec{BuiltIn: "structural_json"}, nil,
		`{"b":2,"a":1}`, `{"a":1,"b":2}`)
	if err != nil || violated {
		t.Fatalf("expected equivalent structures, violated=%t err=%v", violated, err)
	}
}

func TestExactComparisonPreservesLargeIntegerPrecision(t *testing.T) {
	before := json.Number("9007199254740992")
	after := json.Number("9007199254740993")
	violated, _, err := Compare(domain.ComparatorSpec{BuiltIn: "exact"}, nil, before, after)
	if err != nil || !violated {
		t.Fatalf("distinct integers beyond float64 precision must differ: violated=%t err=%v", violated, err)
	}
}

func TestExactComparisonRejectsResourceIntensiveJSONNumber(t *testing.T) {
	_, _, err := Compare(domain.ComparatorSpec{BuiltIn: "exact"}, nil, json.Number("1e10001"), json.Number("0"))
	if err == nil || !strings.Contains(err.Error(), "resource limits") {
		t.Fatalf("resource-intensive number was not rejected explicitly: %v", err)
	}
}

func TestFloatingTolerance(t *testing.T) {
	abs := 0.01
	violated, _, err := Compare(domain.ComparatorSpec{BuiltIn: "exact"}, &domain.Tolerances{Absolute: &abs}, 1.0, 1.005)
	if err != nil || violated {
		t.Fatalf("difference inside tolerance must not violate: %v", err)
	}
	violated, _, err = Compare(domain.ComparatorSpec{BuiltIn: "exact"}, &domain.Tolerances{Absolute: &abs}, 1.0, 1.02)
	if err != nil || !violated {
		t.Fatalf("difference outside tolerance must violate: %v", err)
	}
}

func TestCompareRejectsInvalidTolerance(t *testing.T) {
	for name, value := range map[string]float64{
		"negative": -0.01,
		"nan":      math.NaN(),
		"infinite": math.Inf(1),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Compare(domain.ComparatorSpec{BuiltIn: "exact"}, &domain.Tolerances{Absolute: &value}, 1.0, 1.0)
			if err == nil || !strings.Contains(err.Error(), "finite and nonnegative") {
				t.Fatalf("invalid tolerance was not rejected explicitly: %v", err)
			}
		})
	}
}

func TestExceptionMessageOnlyMattersWhenComparatorObservesIt(t *testing.T) {
	before := map[string]any{"type": "ValueError", "message": "old wording"}
	after := map[string]any{"type": "ValueError", "message": "new wording"}
	violated, _, err := Compare(domain.ComparatorSpec{BuiltIn: "exception_type"}, nil, before, after)
	if err != nil || violated {
		t.Fatalf("type-only comparator must ignore message: %v", err)
	}
	violated, _, err = Compare(domain.ComparatorSpec{BuiltIn: "exception"}, nil, before, after)
	if err != nil || !violated {
		t.Fatalf("full exception comparator must observe message: %v", err)
	}
}

func TestIllegalInputIsInconclusive(t *testing.T) {
	assessment, err := AssessReplays(domain.ComparatorSpec{BuiltIn: "exact"}, nil, []ObservationPair{
		{Before: 1, After: 2, LegalInput: false},
		{Before: 1, After: 2, LegalInput: false},
	})
	if err != nil || assessment.Status != domain.StatusInconclusive || assessment.Stable {
		t.Fatalf("illegal input must remain inconclusive: %#v err=%v", assessment, err)
	}
}

func TestRandomReplayIsFlaky(t *testing.T) {
	pairs := []ObservationPair{
		{Before: 1, After: 2, LegalInput: true, DomainEvidence: []string{"precondition passed"}},
		{Before: 4, After: 5, LegalInput: true, DomainEvidence: []string{"precondition passed"}},
	}
	assessment, err := AssessReplays(domain.ComparatorSpec{BuiltIn: "exact"}, nil, pairs)
	if err != nil || assessment.Status != domain.StatusFlaky || assessment.Stable {
		t.Fatalf("changing observations must be flaky: %#v err=%v", assessment, err)
	}
}

func TestPromoteRequiresAllWitnessConditions(t *testing.T) {
	divergence := domain.ObservedDivergence{
		ID: "d1", Status: domain.ObservationDivergenceConfirmed, Stable: true, ReplayCount: 2,
		ReplayCapsuleDigest: "capsule", EnvironmentDigest: "environment",
		BaselineArtifactDigest: "before", CandidateArtifactDigest: "after",
	}
	witness, err := Promote(divergence, true, "contract", []string{"repository helper accepted input"}, true)
	if err != nil || witness.ContractRelevanceStatus != "violated" {
		t.Fatalf("expected witness: %#v err=%v", witness, err)
	}
	if _, err := Promote(divergence, false, "contract", []string{"valid"}, true); err == nil {
		t.Fatal("missing current-head approval must reject promotion")
	}
}
