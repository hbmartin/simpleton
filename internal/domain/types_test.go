package domain

import (
	"testing"
	"time"
)

func TestChangeIntentRequiresExecutableContractParts(t *testing.T) {
	intent := ChangeIntent{
		SchemaVersion: SchemaVersion,
		ID:            "change-1", Rationale: "Preserve prices", Classification: "behavior_preserving",
		Scope:          []IntentScope{{Package: "pricing"}},
		AllowedChanges: []string{},
		EquivalenceContract: EquivalenceContract{Observations: []ObservationSpec{{
			ID: "price", Target: TargetRef{Language: "go", ScopeType: "api", Symbol: "Calculate"}, Inputs: []any{},
			Preconditions: []HookRef{{BuiltIn: "json_schema"}}, CallsOrEvents: []string{"Calculate"},
			Observables: []string{"return"}, Comparator: ComparatorSpec{BuiltIn: "structural_json"},
		}}},
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("valid intent rejected: %v", err)
	}
	intent.EquivalenceContract.Observations[0].Preconditions[0].Symbol = "also.set"
	if err := intent.Validate(); err == nil {
		t.Fatal("hook with built_in and symbol must be rejected")
	}
}

func TestObservationRejectsToleranceForExceptionComparator(t *testing.T) {
	absolute := 0.1
	observation := ObservationSpec{
		ID: "exception", Target: TargetRef{Language: "go", ScopeType: "api", Symbol: "Run"},
		Inputs:        []any{},
		Preconditions: []HookRef{{BuiltIn: "always"}}, CallsOrEvents: []string{"Run"}, Observables: []string{"exception"},
		Comparator: ComparatorSpec{BuiltIn: "exception"}, Tolerances: &Tolerances{Absolute: &absolute},
	}
	if err := observation.Validate(); err == nil {
		t.Fatal("exception comparator silently accepted an inapplicable tolerance")
	}
}

func TestPolicyCannotPromoteAdvisoryEvidence(t *testing.T) {
	policy := testPolicy()
	policy.Mode = "blocking"
	pack := EvidencePack{
		SemanticAdvisory: true,
		Methods: []MethodResult{{Status: StatusRan, Findings: []Finding{
			{ID: "build", Category: CategoryBuildRegression, Validated: true},
			{ID: "fm", Category: CategoryFMAdvisory, Validated: true},
		}}},
		Witnesses: []BehavioralWitness{{ID: "witness", ContractRelevanceStatus: "violated", ApprovedContractDigest: "digest"}},
	}
	blockers := policy.SelectBlockers(pack)
	if len(blockers) != 1 || blockers[0].ID != "build" {
		t.Fatalf("expected only deterministic blocker, got %#v", blockers)
	}
}

func TestPolicyRejectsUnsafeBlockingAndBudget(t *testing.T) {
	policy := testPolicy()
	policy.Blocking.Allow = append(policy.Blocking.Allow, CategoryFMAdvisory)
	if err := policy.Validate(); err == nil {
		t.Fatal("FM advisory must never be block eligible")
	}
	policy = testPolicy()
	policy.Budgets.HardCeiling = Duration(61 * time.Minute)
	if err := policy.Validate(); err == nil {
		t.Fatal("hard ceiling above 60m must be rejected")
	}
}

func TestPolicyRejectsNonPositiveMethodBudget(t *testing.T) {
	for name, budget := range map[string]Duration{
		"zero":     0,
		"negative": Duration(-time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			policy := testPolicy()
			policy.Budgets.Methods = map[string]Duration{"managed_fm": budget}
			if err := policy.Validate(); err == nil {
				t.Fatal("non-positive method budget was accepted")
			}
		})
	}
}

func TestBlockingPolicyAllowsExplicitlyEmptyCategorySet(t *testing.T) {
	policy := testPolicy()
	policy.Mode = "blocking"
	policy.Blocking.Allow = nil
	if err := policy.Validate(); err != nil {
		t.Fatalf("schema-valid nonblocking configuration was rejected: %v", err)
	}
	if blockers := policy.SelectBlockers(EvidencePack{Witnesses: []BehavioralWitness{{
		ID: "witness", ContractRelevanceStatus: "violated", ApprovedContractDigest: "contract",
	}}}); len(blockers) != 0 {
		t.Fatalf("empty allow list selected blockers: %#v", blockers)
	}
}

func TestNonRanMethodStatusesCannotBlock(t *testing.T) {
	policy := testPolicy()
	policy.Mode = "blocking"
	for _, status := range []MethodStatus{StatusUnsupported, StatusInconclusive, StatusBudgetExhausted, StatusFlaky, StatusExecutionFailed} {
		pack := EvidencePack{Methods: []MethodResult{{Status: status, Findings: []Finding{{
			ID: string(status), Category: CategoryBuildRegression, Validated: true,
		}}}}}
		if blockers := policy.SelectBlockers(pack); len(blockers) != 0 {
			t.Fatalf("status %s produced blockers: %#v", status, blockers)
		}
	}
}

func TestStaticCommandRequiresHighConfidenceAndPinnedImage(t *testing.T) {
	policy := testPolicy()
	policy.Commands = []CommandSpec{{
		ID: "static", Category: CategoryStaticRegression, Command: []string{"scanner"}, Image: "scanner:latest",
	}}
	if err := policy.Validate(); err == nil {
		t.Fatal("unpinned, non-high-confidence static command must be rejected")
	}
	policy.Commands[0].Image = "scanner@sha256:abc"
	policy.Commands[0].Confidence = "high"
	if err := policy.Validate(); err != nil {
		t.Fatalf("high-confidence pinned static command rejected: %v", err)
	}
}

func testPolicy() Policy {
	return Policy{
		SchemaVersion: SchemaVersion, Mode: "shadow",
		Budgets:   BudgetPolicy{Total: Duration(30 * time.Minute), HardCeiling: Duration(60 * time.Minute)},
		Blocking:  BlockingPolicy{Allow: []FindingCategory{CategoryBuildRegression, CategoryBehavioralWitness}},
		Execution: ExecutionPolicy{TrustClass: "trusted_branch", Network: "disabled", Secrets: "none"},
		Telemetry: TelemetryPolicy{RetentionDays: 30},
	}
}
