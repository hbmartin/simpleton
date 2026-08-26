package planner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	var uploadedPayload atomic.Value
	fmServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
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
	payload, _ := uploadedPayload.Load().(string)
	if strings.Contains(payload, repository) || strings.Contains(payload, output) || !strings.Contains(payload, `"repository":"acme/repo"`) {
		t.Fatalf("managed payload exposed local paths or lost repository identity: %s", payload)
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
