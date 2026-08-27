package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/haroldmartin/simpleton/internal/config"
	"github.com/haroldmartin/simpleton/internal/domain"
	"github.com/haroldmartin/simpleton/internal/execution"
	"github.com/haroldmartin/simpleton/internal/managed"
	"github.com/haroldmartin/simpleton/internal/packrpc"
)

type fakeRunner struct {
	result execution.CommandResult
	err    error
}

type sequenceRunner struct {
	mu      sync.Mutex
	results []execution.CommandResult
	errors  []error
	calls   int
}

func (r *sequenceRunner) Run(context.Context, string, domain.ExecutionPolicy, string, []string) (execution.CommandResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.calls
	r.calls++
	var result execution.CommandResult
	var err error
	if index < len(r.results) {
		result = r.results[index]
	}
	if index < len(r.errors) {
		err = r.errors[index]
	}
	return result, err
}

type deadlineRunner struct{}

func (deadlineRunner) Run(ctx context.Context, _ string, _ domain.ExecutionPolicy, _ string, _ []string) (execution.CommandResult, error) {
	<-ctx.Done()
	return execution.CommandResult{}, ctx.Err()
}

func (f fakeRunner) Run(context.Context, string, domain.ExecutionPolicy, string, []string) (execution.CommandResult, error) {
	return f.result, f.err
}

type sharedContextRunner struct {
	contexts []context.Context
}

func (r *sharedContextRunner) Run(ctx context.Context, _ string, _ domain.ExecutionPolicy, _ string, _ []string) (execution.CommandResult, error) {
	r.contexts = append(r.contexts, ctx)
	return execution.CommandResult{}, nil
}

func TestAnalyzeProposesIntentAndCanStillBlockBuildRegression(t *testing.T) {
	repository, base, head := testRepository(t, "before\n", "after\n")
	policy := config.DefaultPolicy()
	policy.Mode = "blocking"
	policy.Commands = []domain.CommandSpec{{
		ID: "build", Category: domain.CategoryBuildRegression, Command: []string{"go", "build", "./..."}, Image: "example.invalid/toolchain@sha256:abc",
	}}
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := config.WriteYAML(policyPath, policy); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "run")
	runner := &sequenceRunner{results: []execution.CommandResult{
		{},
		{ExitCode: 1, Stderr: "compile failed"},
	}}
	planner := Planner{
		Runner: runner,
		Now:    func() time.Time { return time.Unix(10, 0) },
	}
	result, err := planner.Analyze(context.Background(), Request{
		Repository: repository, Base: base, Head: head, Output: output, PolicyPath: policyPath, CoreVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Pack.SemanticAdvisory || result.ExitCode != 1 || len(result.Pack.Blockers) != 1 {
		t.Fatalf("expected advisory semantics and deterministic blocker: %#v", result)
	}
	if result.Pack.Blockers[0].Category != domain.CategoryBuildRegression {
		t.Fatalf("unexpected blocker: %#v", result.Pack.Blockers)
	}
	if result.Pack.ProposedIntentPath == "" {
		t.Fatal("missing proposed Change Intent")
	}
	for _, name := range []string{"evidence-pack.json", "scoping-pack.json", "backlog.md", "backlog.html", "simpleton.db"} {
		if _, err := os.Stat(filepath.Join(output, name)); err != nil {
			t.Fatalf("missing output %s: %v", name, err)
		}
	}
}

func TestPolicyCommandRequiresPassingBaseline(t *testing.T) {
	policy := config.DefaultPolicy()
	policy.Mode = "blocking"
	policy.Commands = []domain.CommandSpec{{
		ID: "tests", Category: domain.CategoryTestRegression, Command: []string{"test"}, Image: "image@sha256:abc",
	}}
	runner := &sequenceRunner{results: []execution.CommandResult{{ExitCode: 1, Stderr: "already failing"}}}
	methods := (Planner{Runner: runner}).runCommands(context.Background(), "/baseline", "/candidate", policy)
	if len(methods) != 1 || methods[0].Status != domain.StatusInconclusive || len(methods[0].Findings) != 0 {
		t.Fatalf("baseline failure was promoted as a regression: %#v", methods)
	}
	if runner.calls != 1 {
		t.Fatalf("candidate should not run when the baseline is not a usable oracle: calls=%d", runner.calls)
	}
}

func TestPolicyCommandInfrastructureExitCannotBecomeFinding(t *testing.T) {
	policy := config.DefaultPolicy()
	policy.Commands = []domain.CommandSpec{{
		ID: "build", Category: domain.CategoryBuildRegression, Command: []string{"build"}, Image: "image@sha256:abc",
	}}
	runner := &sequenceRunner{results: []execution.CommandResult{{}, {ExitCode: 125, Stderr: "daemon unavailable"}}}
	methods := (Planner{Runner: runner}).runCommands(context.Background(), "/baseline", "/candidate", policy)
	if len(methods) != 1 || methods[0].Status != domain.StatusExecutionFailed || len(methods[0].Findings) != 0 {
		t.Fatalf("infrastructure exit was promoted as a regression: %#v", methods)
	}
}

func TestPolicyCommandExecutionsShareMethodTimeout(t *testing.T) {
	policy := config.DefaultPolicy()
	policy.Commands = []domain.CommandSpec{{
		ID: "tests", Category: domain.CategoryTestRegression, Command: []string{"test"}, Image: "image@sha256:abc",
	}}
	runner := &sharedContextRunner{}
	methods := (Planner{Runner: runner}).runCommands(context.Background(), "/baseline", "/candidate", policy)
	if len(methods) != 1 || methods[0].Status != domain.StatusRan {
		t.Fatalf("unexpected command result: %#v", methods)
	}
	if len(runner.contexts) != 2 || runner.contexts[0] != runner.contexts[1] {
		t.Fatalf("baseline and candidate did not share one method timeout: %#v", runner)
	}
}

func TestMatchingTargetRejectsUnqualifiedLegacyMethodNames(t *testing.T) {
	reference := domain.TargetRef{Language: "python", ScopeType: "symbol", Path: "sample.py", Symbol: "same"}
	targets := []domain.VerificationTarget{{
		ID: "first", Language: "python", ScopeType: "symbol", File: "sample.py", Symbol: "First.same",
	}}
	if target, found := matchingTarget(reference, targets); found {
		t.Fatalf("unqualified approved symbol matched a qualified target: %#v", target)
	}
	reference.Symbol = "First.same"
	if target, found := matchingTarget(reference, targets); !found || target.ID != "first" {
		t.Fatalf("exact qualified target did not match: target=%#v found=%t", target, found)
	}
}

func TestCachePackVersionsIncludeNegotiatedPackVersions(t *testing.T) {
	versions := cachePackVersions([]domain.PackCapability{
		{Language: "python", PackVersion: "0.1.1"},
		{Language: "typescript", PackVersion: "0.2.0"},
	})
	if versions["protocol"] != domain.ProtocolVersion || versions["python"] != "0.1.1" || versions["typescript"] != "0.2.0" {
		t.Fatalf("cache versions omitted negotiated capabilities: %#v", versions)
	}
}

func TestBudgetExhaustionCannotBlock(t *testing.T) {
	repository, base, head := testRepository(t, "before\n", "after\n")
	policy := config.DefaultPolicy()
	policy.Mode = "blocking"
	policy.Commands = []domain.CommandSpec{{ID: "tests", Category: domain.CategoryTestRegression, Command: []string{"test"}, Image: "image@sha256:abc"}}
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := config.WriteYAML(policyPath, policy); err != nil {
		t.Fatal(err)
	}
	result, err := (Planner{Runner: fakeRunner{err: context.DeadlineExceeded}}).Analyze(context.Background(), Request{
		Repository: repository, Base: base, Head: head, Output: filepath.Join(t.TempDir(), "run"), PolicyPath: policyPath, CoreVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || len(result.Pack.Blockers) != 0 || result.Pack.Methods[1].Status != domain.StatusBudgetExhausted {
		t.Fatalf("budget exhaustion must be nonblocking: %#v", result)
	}
}

func TestHardCeilingStillAllowsEvidenceFinalization(t *testing.T) {
	repository, base, _ := testRepository(t, "before\n", "after\n")
	if err := os.WriteFile(filepath.Join(repository, "sample.go"), []byte("package sample\n\nfunc Value() int { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "sample.go")
	runGit(t, repository, "commit", "-qm", "add Go source")
	head := runGit(t, repository, "rev-parse", "HEAD")
	policy := config.DefaultPolicy()
	policy.Budgets.Total = domain.Duration(2 * time.Second)
	policy.Budgets.HardCeiling = domain.Duration(2 * time.Second)
	policy.Packs["go"] = domain.PackCommand{Command: []string{"/bin/sh", "-c", "exec sleep 60"}}
	policy.Telemetry.Enabled = true
	policy.Telemetry.OrgAdminOptIn = true
	policy.Telemetry.Organization = "acme"
	policy.Telemetry.Repository = "acme/repo"
	policy.Telemetry.AllowedRepos = []string{"acme/repo"}
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := config.WriteYAML(policyPath, policy); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "run")
	result, err := (Planner{}).Analyze(context.Background(), Request{
		Repository: repository, Base: base, Head: head, Output: output, PolicyPath: policyPath, CoreVersion: "test",
	})
	if err != nil {
		t.Fatalf("hard-ceiling exhaustion should finalize evidence: %v", err)
	}
	if len(result.Pack.Methods) < 2 || result.Pack.Methods[1].Status != domain.StatusBudgetExhausted {
		t.Fatalf("hard-ceiling status was not preserved: %#v", result.Pack.Methods)
	}
	if result.EvidenceDigest == "" {
		t.Fatal("finalized evidence has no digest")
	}
	if result.Pack.AdvisoryReview.Status != domain.StatusBudgetExhausted || result.Pack.Telemetry.Status != "not_uploaded" {
		t.Fatalf("hard-ceiling finalization attempted managed work: review=%#v telemetry=%#v", result.Pack.AdvisoryReview, result.Pack.Telemetry)
	}
}

func TestIndependentReplayDerivesWitnessConditionsInCore(t *testing.T) {
	seed := "seed"
	capsuleDigest := strings.Repeat("a", 64)
	capsule := domain.ReplayCapsule{
		Digest: capsuleDigest, ObjectDigests: []string{strings.Repeat("b", 64)},
		ReplayCommand: []string{"replay"}, Seed: seed, Comparator: "exact",
	}
	observation := domain.ObservationSpec{
		ID: "observation", Inputs: []any{"fixture"}, Preconditions: []domain.HookRef{{BuiltIn: "always"}},
		Comparator: domain.ComparatorSpec{BuiltIn: "exact"},
	}
	target := domain.VerificationTarget{ID: "target", Language: "go"}
	first := domain.ObservedDivergence{
		ID: "divergence", FixtureOrInput: "fixture", BeforeObservation: 1, AfterObservation: 2,
		Stable: true, ReplayCount: 99, ReplayCapsuleDigest: capsuleDigest,
	}
	second := first
	pack := &domain.EvidencePack{Approval: domain.ApprovalContext{Approved: true}, Provenance: domain.Provenance{EnvironmentDigest: "environment"}}
	recorded, promoted, status, reason := validateIndependentReplay(
		observation, target, seed, "contract", pack, first, []domain.ReplayCapsule{capsule},
		packrpc.ProbeResult{Divergences: []domain.ObservedDivergence{second}, Capsules: []domain.ReplayCapsule{capsule}},
	)
	if promoted == nil || status != domain.StatusRan || reason != "" {
		t.Fatalf("independent replay should promote: witness=%#v status=%s reason=%q", promoted, status, reason)
	}
	if !recorded.Stable || recorded.ReplayCount != 2 || recorded.EnvironmentDigest != "environment" {
		t.Fatalf("core did not derive replay fields: %#v", recorded)
	}
}

func TestIndependentReplayRecordsUnverifiableComparator(t *testing.T) {
	capsuleDigest := strings.Repeat("a", 64)
	capsule := domain.ReplayCapsule{
		Digest: capsuleDigest, ObjectDigests: []string{strings.Repeat("b", 64)}, ReplayCommand: []string{"replay"}, Seed: "seed",
	}
	observation := domain.ObservationSpec{
		ID: "observation", Inputs: []any{"fixture"}, Preconditions: []domain.HookRef{{BuiltIn: "always"}},
		Comparator: domain.ComparatorSpec{Symbol: "helpers.Compare"},
	}
	divergence := domain.ObservedDivergence{
		ID: "divergence", FixtureOrInput: "fixture", BeforeObservation: 1, AfterObservation: 2, ReplayCapsuleDigest: capsuleDigest,
	}
	_, promoted, status, reason := validateIndependentReplay(
		observation, domain.VerificationTarget{ID: "target"}, "seed", "contract",
		&domain.EvidencePack{Approval: domain.ApprovalContext{Approved: true}}, divergence, []domain.ReplayCapsule{capsule},
		packrpc.ProbeResult{Divergences: []domain.ObservedDivergence{divergence}, Capsules: []domain.ReplayCapsule{capsule}},
	)
	if promoted != nil || status != domain.StatusInconclusive || !strings.Contains(reason, "repository comparator symbols") {
		t.Fatalf("custom comparator failure must be recorded: witness=%#v status=%s reason=%q", promoted, status, reason)
	}
}

func TestIndependentReplaySanitizesPackAssertedTrustFields(t *testing.T) {
	observation := domain.ObservationSpec{ID: "observation"}
	target := domain.VerificationTarget{ID: "target"}
	reported := domain.ObservedDivergence{
		Status: domain.ObservationDivergenceConfirmed, Stable: true, ReplayCount: 99,
		EnvironmentDigest: "forged-environment", BaselineArtifactDigest: "forged-before",
		CandidateArtifactDigest: "forged-after", ReplayCapsuleDigest: "forged-capsule",
		PreconditionEvidence: []string{"forged"},
	}
	recorded, promoted, status, _ := validateIndependentReplay(
		observation, target, "seed", "contract", &domain.EvidencePack{}, reported, nil, packrpc.ProbeResult{},
	)
	if promoted != nil || status != domain.StatusInconclusive {
		t.Fatalf("invalid divergence was promoted: witness=%#v status=%s", promoted, status)
	}
	if recorded.Status != domain.ObservationNotObserved || recorded.Stable || recorded.ReplayCount != 0 ||
		recorded.EnvironmentDigest != "" || recorded.BaselineArtifactDigest != "" || recorded.CandidateArtifactDigest != "" ||
		recorded.ReplayCapsuleDigest != "" || len(recorded.PreconditionEvidence) != 0 {
		t.Fatalf("pack-asserted trust fields survived sanitization: %#v", recorded)
	}
}

func TestReplayCapsuleRequiresCoreIssuedSeed(t *testing.T) {
	capsule := domain.ReplayCapsule{
		Digest: strings.Repeat("a", 64), ObjectDigests: []string{strings.Repeat("b", 64)},
		ReplayCommand: []string{"replay"}, Comparator: "exact",
	}
	if reason := validateReplayCapsule(capsule, "seed", "exact"); !strings.Contains(reason, "core-issued seed") {
		t.Fatalf("missing seed was accepted: %q", reason)
	}
	capsule.Seed = "seed"
	if reason := validateReplayCapsule(capsule, "seed", "exact"); reason != "" {
		t.Fatalf("matching seed was rejected: %q", reason)
	}
	if reason := validateReplayCapsule(capsule, "different-seed", "exact"); !strings.Contains(reason, "does not match") {
		t.Fatalf("wrong seed was accepted: %q", reason)
	}
}

func TestManagedCallsUsePostExecutionContexts(t *testing.T) {
	repository, base, head := testRepository(t, "before\n", "after\n")
	output := filepath.Join(t.TempDir(), "run")
	var localEvidencePresent atomic.Bool
	var managedFMRequest atomic.Value
	var uploadedPayload atomic.Value
	fmServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		managedFMRequest.Store(string(body))
		if _, err := fmt.Fprint(response, `{"explanation":"reviewed","suspicion":"low"}`); err != nil {
			t.Errorf("write managed FM response: %v", err)
		}
	}))
	defer fmServer.Close()
	telemetryServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if _, err := os.Stat(filepath.Join(output, "evidence-pack.json")); err == nil {
			localEvidencePresent.Store(true)
		}
		body, _ := io.ReadAll(request.Body)
		uploadedPayload.Store(string(body))
		if _, err := fmt.Fprint(response, `{"id":"remote-evidence"}`); err != nil {
			t.Errorf("write telemetry response: %v", err)
		}
	}))
	defer telemetryServer.Close()
	policy := config.DefaultPolicy()
	policy.Budgets.Total = domain.Duration(time.Second)
	policy.Commands = []domain.CommandSpec{{
		ID: "tests", Category: domain.CategoryTestRegression, Command: []string{"test"}, Image: "image@sha256:abc",
	}}
	policy.Telemetry.Enabled = true
	policy.Telemetry.OrgAdminOptIn = true
	policy.Telemetry.Organization = "acme"
	policy.Telemetry.Repository = "acme/repo"
	policy.Telemetry.AllowedRepos = []string{"acme/repo"}
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := config.WriteYAML(policyPath, policy); err != nil {
		t.Fatal(err)
	}
	result, err := (Planner{
		Runner:          deadlineRunner{},
		FMClient:        managed.FMClient{Endpoint: fmServer.URL, AllowInsecure: true},
		TelemetryClient: managed.TelemetryClient{Endpoint: telemetryServer.URL, AllowInsecure: true},
	}).Analyze(context.Background(), Request{
		Repository: repository, Base: base, Head: head, Output: output, PolicyPath: policyPath, CoreVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pack.AdvisoryReview.Status != domain.StatusRan || result.Pack.Telemetry.Status != "uploaded" {
		t.Fatalf("managed calls inherited the exhausted execution context: review=%#v telemetry=%#v", result.Pack.AdvisoryReview, result.Pack.Telemetry)
	}
	if !localEvidencePresent.Load() {
		t.Fatal("telemetry upload happened before evidence was persisted locally")
	}
	fmPayload, _ := managedFMRequest.Load().(string)
	payload, _ := uploadedPayload.Load().(string)
	if strings.Contains(fmPayload, repository) || strings.Contains(fmPayload, output) {
		t.Fatalf("managed FM request exposed local paths: %s", fmPayload)
	}
	if strings.Contains(payload, repository) || strings.Contains(payload, output) || !strings.Contains(payload, `"repository":"acme/repo"`) {
		t.Fatalf("managed payload exposed local paths or lost repository identity: %s", payload)
	}
}

func TestManagedReviewPayloadPreservesAllowlistedSemantics(t *testing.T) {
	targets := []domain.VerificationTarget{{
		ID: "target", Language: "go", File: "internal/value.go", Symbol: "Value.Adjust", Kind: "method", ScopeType: "symbol",
		ObservationCandidates: []domain.ObservationBoundary{{Kind: "unchanged_caller", Symbol: "Caller.Run", Path: "internal/caller.go"}},
		Applicability: domain.RunApplicability{
			Applicable: false, Reason: "effect risks require an approved stable observation boundary",
			Risks: []string{"concurrency", "untrusted-secret-risk"},
		},
	}, {
		ID: "second-target", Language: "typescript", File: "web/value.ts", Symbol: "private/invalid", Kind: "method", ScopeType: "symbol",
		ObservationCandidates: []domain.ObservationBoundary{{Kind: "direct_unit", Symbol: "Second.Run", Path: "web/value.ts"}},
		Applicability:         domain.RunApplicability{Applicable: false, Reason: "no statically detected effects", Risks: []string{"imports_node_http2"}},
	}}
	paths := map[string]struct{}{"internal/value.go": {}, "internal/caller.go": {}, "web/value.ts": {}}
	reviewTargets := managedReviewTargets(targets, paths)
	methods := []domain.MethodResult{
		{ID: "go_type_analysis", Status: domain.StatusRan, Findings: []domain.Finding{{ID: "type-finding", MethodID: "go_type_analysis"}}},
		{ID: "acme_unreleased_fuzzer", Status: domain.StatusExecutionFailed, Reason: "/Users/alice/private/diagnostic", Findings: []domain.Finding{{ID: "custom-finding", MethodID: "acme_unreleased_fuzzer"}}},
	}
	reviewMethods := managedReviewMethodResults(methods)
	if reviewTargets[0].Symbol != "Value.Adjust" || reviewTargets[0].ObservationCandidates[0].Symbol != "Caller.Run" ||
		!slices.Equal(reviewTargets[0].Applicability.Risks, []string{"concurrency"}) || reviewTargets[1].Symbol != "" ||
		reviewTargets[1].ObservationCandidates[0].Symbol != "Second.Run" || !slices.Equal(reviewTargets[1].Applicability.Risks, []string{"imports_node_http2"}) ||
		reviewMethods[0].ID != "go_type_analysis" || reviewMethods[0].Findings[0].MethodID != reviewMethods[0].ID ||
		reviewMethods[1].ID == "acme_unreleased_fuzzer" || reviewMethods[1].Findings[0].MethodID != reviewMethods[1].ID {
		t.Fatalf("managed review lost safe semantic evidence: targets=%#v methods=%#v", reviewTargets, reviewMethods)
	}
	payload, err := json.Marshal(reviewMethods)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"acme_unreleased_fuzzer", "/Users/alice/private/diagnostic"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("managed review exposed user-controlled method metadata %q: %s", forbidden, payload)
		}
	}
	if methods[1].Reason != "/Users/alice/private/diagnostic" || methods[1].ID != "acme_unreleased_fuzzer" {
		t.Fatalf("managed review sanitization mutated local evidence: %#v", methods[1])
	}
}

func TestManagedRiskAllowlistCoversNormalizedPackVocabulary(t *testing.T) {
	risks := []string{
		"imports_fs", "imports_node_fs", "imports_http2", "imports_node_http2", "imports_timers", "imports_node_timers",
		"imports_net_http", "imports_asyncio", "concurrency", "async", "call_readFile", "call_open",
	}
	if managed := managedRisks(risks); !slices.Equal(managed, risks) {
		t.Fatalf("managed review dropped normalized pack risks: got=%#v want=%#v", managed, risks)
	}
}

func TestManagedEvidencePackRemovesRawContentWithoutMutatingLocalEvidence(t *testing.T) {
	posixPath := "/Users/alice/private/source.go"
	windowsPath := `C:\Users\alice\private\source.go`
	uncPath := `\\server\share\private\source.go`
	raw := "raw-command-output"
	pack := domain.EvidencePack{
		SchemaVersion: raw,
		RunID:         raw,
		Provenance: domain.Provenance{
			CoreVersion: raw, Repository: posixPath, BaseRevision: raw, TrustClass: raw,
			LockfileDigests: map[string]string{raw: raw}, ToolVersions: map[string]string{raw: posixPath},
		},
		Approval: domain.ApprovalContext{Source: raw, RequiredOwner: raw, Reason: raw + " " + posixPath},
		Capabilities: []domain.PackCapability{{
			Language: raw, PackVersion: raw, ProtocolVersion: raw, Methods: map[string]bool{raw: true},
			Requirements: map[string]string{raw: posixPath}, TrustClasses: []string{posixPath},
		}},
		Targets: []domain.VerificationTarget{{
			ID: raw, Language: raw, Symbol: raw, Kind: raw, ScopeType: raw,
			File: "src/value.go", BaselineArtifact: posixPath, CandidateArtifact: windowsPath,
			Dependencies:          []string{"src/dependency.go", uncPath},
			ObservationCandidates: []domain.ObservationBoundary{{Kind: raw, Symbol: raw, Path: uncPath}},
			Applicability:         domain.RunApplicability{Reason: raw, Risks: []string{posixPath}},
		}},
		Methods: []domain.MethodResult{{
			ID: raw, Language: raw, Status: domain.MethodStatus(raw), Budget: raw,
			Reason:        raw + " " + posixPath,
			Findings:      []domain.Finding{{ID: raw, Category: domain.FindingCategory(raw), Title: raw, Detail: windowsPath, Severity: raw, TargetID: raw, MethodID: raw}},
			Applicability: &domain.RunApplicability{Reason: raw, Risks: []string{uncPath}},
		}},
		Divergences: []domain.ObservedDivergence{{
			ID: raw, TargetID: raw, ObservationSpecID: raw, Status: domain.ObservationStatus(raw), Comparator: raw,
			EnvironmentDigest: raw, BaselineArtifactDigest: raw, CandidateArtifactDigest: raw, ReplayCapsuleDigest: raw,
			FixtureOrInput: map[string]any{posixPath: raw}, BeforeObservation: windowsPath, AfterObservation: uncPath,
			PreconditionEvidence: []string{raw},
		}},
		Witnesses: []domain.BehavioralWitness{{
			ID: raw, Divergence: domain.ObservedDivergence{ID: raw, BeforeObservation: posixPath},
			DomainValidityEvidence: []string{raw}, ContractRelevanceStatus: raw, ApprovedContractDigest: raw,
		}},
		ReplayCapsules: []domain.ReplayCapsule{{
			Digest: raw, ObjectDigests: []string{raw}, Environment: map[string]string{"repository": posixPath, "workspace": windowsPath},
			ReplayCommand: []string{raw, uncPath}, Seed: raw, Comparator: raw,
		}},
		Scoping: domain.ScopingPack{SchemaVersion: raw, RunID: raw, Opportunities: []domain.Opportunity{{
			ID: raw, Language: raw, Category: raw, Region: "src/value.go:Value", Evidence: []string{raw, posixPath}, AllowedFiles: []string{"src/value.go", windowsPath},
		}}},
		AdvisoryReview: domain.AdvisoryReview{Status: domain.MethodStatus(raw), ModelID: raw, PromptID: raw, Explanation: raw, Suspicion: raw, Reason: posixPath},
		Telemetry:      domain.TelemetryOutcome{Status: raw, Reason: raw, RemoteID: raw},
		Blockers:       []domain.Finding{{Title: raw, Detail: uncPath}},
	}
	before, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	repositoryPaths := map[string]struct{}{"src/value.go": {}, "src/dependency.go": {}}
	managedPack := managedEvidencePack(pack, "acme/repo", repositoryPaths)
	after, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("managed sanitization mutated the local evidence pack")
	}
	managedPayload, err := json.Marshal(managedPack)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{raw, posixPath, windowsPath, uncPath} {
		if strings.Contains(string(managedPayload), forbidden) {
			t.Fatalf("managed evidence retained sensitive content %q: %s", forbidden, managedPayload)
		}
	}
	if managedPack.Provenance.Repository != "acme/repo" || managedPack.Targets[0].File != "src/value.go" ||
		len(managedPack.Targets[0].Dependencies) != 1 || managedPack.Targets[0].Dependencies[0] != "src/dependency.go" ||
		managedPack.Scoping.Opportunities[0].Region != "" || len(managedPack.Scoping.Opportunities[0].AllowedFiles) != 1 {
		t.Fatalf("managed sanitization removed safe structured evidence: %#v", managedPack)
	}
	if managedPath(posixPath, nil) != "" {
		t.Fatalf("absolute path was not rejected: %q", posixPath)
	}
	for _, value := range []string{"file:///Users/alice/private/source.go", "file://localhost/C:/private/source.go"} {
		if managedPath(value, nil) != "" {
			t.Fatalf("file URI was not rejected: %q", value)
		}
	}
	if runtime.GOOS == "windows" {
		if managedPath("C:/src/value.go", nil) != "" {
			t.Fatal("Windows absolute path was not rejected")
		}
	} else {
		for _, value := range []string{"C:/src/value.go", `src\value.go`} {
			if got := managedPath(value, nil); got != value {
				t.Fatalf("valid POSIX Git path was changed: got %q, want %q", got, value)
			}
		}
	}
	if managedPath("https://example.invalid/path", nil) == "" {
		t.Fatal("URL was mistaken for a local absolute path")
	}
	if managedPath("not-in-repository.go", repositoryPaths) != "" {
		t.Fatal("pack-supplied path outside the repository tree was accepted")
	}
}

func testRepository(t *testing.T, before, after string) (string, string, string) {
	t.Helper()
	repository := t.TempDir()
	runGit(t, repository, "init", "-q")
	runGit(t, repository, "config", "user.email", "simpleton@example.invalid")
	runGit(t, repository, "config", "user.name", "Simpleton Test")
	path := filepath.Join(repository, "README.md")
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "README.md")
	runGit(t, repository, "commit", "-qm", "base")
	base := runGit(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "commit", "-qam", "head")
	head := runGit(t, repository, "rev-parse", "HEAD")
	return repository, base, head
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
