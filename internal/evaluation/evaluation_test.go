package evaluation

import "testing"

func TestQualitativeDecisionBands(t *testing.T) {
	continued, err := Decide(QualitativeScore{4, 4, 4, 4, 4, 4})
	if err != nil || continued.Disposition != "continue" {
		t.Fatalf("unexpected continue decision: %#v err=%v", continued, err)
	}
	specialized, err := Decide(QualitativeScore{5, 5, 5, 5, 5, 2})
	if err != nil || specialized.Disposition != "specialize" {
		t.Fatalf("critical weakness must specialize: %#v err=%v", specialized, err)
	}
	deferred, err := Decide(QualitativeScore{2, 2, 2, 2, 2, 2})
	if err != nil || deferred.Disposition != "defer" {
		t.Fatalf("unexpected defer decision: %#v err=%v", deferred, err)
	}
}

func TestPromotionRequiresSampleAndNonInferiority(t *testing.T) {
	tooSmall, err := AssessPromotion(PromotionSample{Targets: 29, NativeHits: 10, GeneratedHits: 10})
	if err != nil || tooSmall.Promote {
		t.Fatalf("small sample cannot promote: %#v err=%v", tooSmall, err)
	}
	good, err := AssessPromotion(PromotionSample{Targets: 100, NativeHits: 40, GeneratedHits: 42})
	if err != nil || !good.Promote {
		t.Fatalf("expected non-inferior promotion: %#v err=%v", good, err)
	}
	bad, err := AssessPromotion(PromotionSample{Targets: 100, NativeHits: 60, GeneratedHits: 30})
	if err != nil || bad.Promote {
		t.Fatalf("large loss cannot promote: %#v err=%v", bad, err)
	}
}
