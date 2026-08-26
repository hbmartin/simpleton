package planner

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/haroldmartin/simpleton/internal/cache"
	"github.com/haroldmartin/simpleton/internal/config"
	"github.com/haroldmartin/simpleton/internal/domain"
	"github.com/haroldmartin/simpleton/internal/execution"
	"github.com/haroldmartin/simpleton/internal/gitx"
	"github.com/haroldmartin/simpleton/internal/managed"
	"github.com/haroldmartin/simpleton/internal/numeric"
	"github.com/haroldmartin/simpleton/internal/packrpc"
	"github.com/haroldmartin/simpleton/internal/report"
	"github.com/haroldmartin/simpleton/internal/sense"
	"github.com/haroldmartin/simpleton/internal/store"
	"github.com/haroldmartin/simpleton/internal/witness"
)

type Request struct {
	Repository  string
	Base        string
	Head        string
	Output      string
	IntentPath  string
	PolicyPath  string
	PackDir     string
	CoreVersion string
}

type Result struct {
	Pack           domain.EvidencePack
	EvidenceDigest string
	ExitCode       int
}

type Planner struct {
	Runner          execution.Runner
	FMClient        managed.FMClient
	TelemetryClient managed.TelemetryClient
	Now             func() time.Time
	Executable      func() (string, error)
}

func (p Planner) Analyze(ctx context.Context, request Request) (Result, error) {
	if request.Repository == "" || request.Base == "" || request.Head == "" || request.Output == "" {
		return Result{}, errors.New("repo, base, head, and output are required")
	}
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.Executable == nil {
		p.Executable = os.Executable
	}
	if p.Runner == nil {
		p.Runner = execution.ContainerRunner{}
	}
	policyPath := request.PolicyPath
	if policyPath == "" {
		candidate := filepath.Join(request.Repository, ".simpleton", "policy.yaml")
		if _, statErr := os.Stat(candidate); statErr == nil {
			policyPath = candidate
		}
	}
	policy, err := config.LoadPolicy(policyPath)
	if err != nil {
		return Result{}, err
	}
	requestCtx := ctx
	hardCtx, hardCancel := context.WithTimeout(requestCtx, policy.Budgets.HardCeiling.Duration())
	defer hardCancel()
	ctx = hardCtx
	repo, err := gitx.Open(request.Repository)
	if err != nil {
		return Result{}, err
	}
	base, err := repo.Resolve(ctx, request.Base)
	if err != nil {
		return Result{}, err
	}
	head, err := repo.Resolve(ctx, request.Head)
	if err != nil {
		return Result{}, err
	}
	baseTree, err := repo.TreeDigest(ctx, base)
	if err != nil {
		return Result{}, err
	}
	headTree, err := repo.TreeDigest(ctx, head)
	if err != nil {
		return Result{}, err
	}
	changed, err := repo.ChangedFiles(ctx, base, head)
	if err != nil {
		return Result{}, err
	}
	managedRepositoryPaths := map[string]struct{}{}
	if config.TelemetryAllowed(policy.Telemetry) == nil {
		if paths, listErr := repo.ListFiles(ctx, head); listErr == nil {
			for _, path := range paths {
				managedRepositoryPaths[path] = struct{}{}
			}
		}
	}
	patch, err := repo.Patch(ctx, base, head)
	if err != nil {
		return Result{}, err
	}
	worktreeDigest, err := repo.WorktreeDigest(ctx)
	if err != nil {
		return Result{}, err
	}
	policyDigest, err := domain.DigestJSON(policy)
	if err != nil {
		return Result{}, err
	}
	createdAt := p.Now().UTC()
	runID := domain.StableID(repo.Path, base, head, createdAt.Format(time.RFC3339Nano))
	output, err := filepath.Abs(request.Output)
	if err != nil {
		return Result{}, err
	}
	state, err := store.Open(output)
	if err != nil {
		return Result{}, err
	}
	defer state.Close()
	patchDigest, err := state.PutObject(patch)
	if err != nil {
		return Result{}, err
	}

	intent, intentDigest, approval, semanticAdvisory, proposedPath, err := p.resolveIntent(request, changed, base, head, output)
	if err != nil {
		return Result{}, err
	}
	environmentDigest, err := domain.DigestJSON(map[string]string{
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"trust_class": policy.Execution.TrustClass, "network": policy.Execution.Network,
	})
	if err != nil {
		return Result{}, err
	}
	lockfiles, err := cache.LockfileDigests(ctx, repo, head)
	if err != nil {
		return Result{}, err
	}
	comparatorDigest, err := domain.DigestJSON(intent.EquivalenceContract.Observations)
	if err != nil {
		return Result{}, err
	}
	cacheInputs := cache.Inputs{
		BaseTree: baseTree, HeadTree: headTree, Worktree: worktreeDigest, Lockfiles: lockfiles,
		Toolchains: map[string]string{"go": runtime.Version()}, Environment: environmentDigest,
		Contract: intentDigest, Policy: policyDigest, Comparator: comparatorDigest,
		Seed: domain.StableID(base, head), Budgets: map[string]string{"total": policy.Budgets.Total.Duration().String()},
		CoreVersion: request.CoreVersion, PackVersions: map[string]string{"protocol": domain.ProtocolVersion},
	}
	pack := domain.EvidencePack{
		SchemaVersion: domain.SchemaVersion,
		RunID:         runID,
		CreatedAt:     createdAt,
		Provenance: domain.Provenance{
			CoreVersion: request.CoreVersion, SchemaVersion: domain.SchemaVersion, ProtocolVersion: domain.ProtocolVersion,
			Repository: repo.Path, BaseRevision: base, HeadRevision: head, BaseTreeDigest: baseTree, HeadTreeDigest: headTree,
			IntentDigest: intentDigest, PolicyDigest: policyDigest, EnvironmentDigest: environmentDigest,
			LockfileDigests: lockfiles,
			TrustClass:      policy.Execution.TrustClass,
			ToolVersions:    map[string]string{"go": runtime.Version()},
		},
		Approval:         approval,
		SemanticAdvisory: semanticAdvisory,
		Capabilities:     []domain.PackCapability{}, Targets: []domain.VerificationTarget{}, Methods: []domain.MethodResult{},
		Divergences: []domain.ObservedDivergence{}, Witnesses: []domain.BehavioralWitness{}, ReplayCapsules: []domain.ReplayCapsule{},
		Scoping:            domain.ScopingPack{SchemaVersion: domain.SchemaVersion, RunID: runID, Opportunities: []domain.Opportunity{}},
		AdvisoryReview:     domain.AdvisoryReview{Status: domain.StatusUnsupported, Reason: "managed FM not attempted"},
		Telemetry:          domain.TelemetryOutcome{Status: "not_uploaded", RetentionDays: 30},
		ProposedIntentPath: proposedPath,
		Blockers:           []domain.Finding{},
	}
	pack.Methods = append(pack.Methods, domain.MethodResult{
		ID: "structural_diff", Status: domain.StatusRan,
		Coverage: &domain.Coverage{TargetsTotal: len(changed), TargetsObserved: len(changed), Ratio: ratio(len(changed), len(changed))},
		Findings: []domain.Finding{},
	})
	pack.ReplayCapsules = append(pack.ReplayCapsules, domain.ReplayCapsule{
		Digest: patchDigest, ObjectDigests: []string{patchDigest},
		Environment:   map[string]string{"base": base, "head": head, "repository": repo.Path},
		ReplayCommand: []string{"git", "apply", "--check", "<object:" + patchDigest + ">"},
	})

	runCtx, cancel := context.WithTimeout(ctx, policy.Budgets.Total.Duration())
	defer cancel()
	baselineRepository := repo.Path
	candidateRepository := repo.Path
	if len(policy.Commands) > 0 {
		var cleanupBaseline func() error
		baselineRepository, cleanupBaseline, err = repo.DetachedWorktree(runCtx, base)
		if err != nil {
			return Result{}, fmt.Errorf("prepare detached baseline worktree: %w", err)
		}
		defer func() { _ = cleanupBaseline() }()
		var cleanupCandidate func() error
		candidateRepository, cleanupCandidate, err = repo.DetachedWorktree(runCtx, head)
		if err != nil {
			return Result{}, fmt.Errorf("prepare detached candidate worktree: %w", err)
		}
		defer func() { _ = cleanupCandidate() }()
	}
	commandMethods := p.runCommands(runCtx, baselineRepository, candidateRepository, policy)
	pack.Methods = append(pack.Methods, commandMethods...)

	packDir := request.PackDir
	if packDir == "" {
		packDir = "packs"
	}
	if !filepath.IsAbs(packDir) {
		if abs, absErr := filepath.Abs(packDir); absErr == nil {
			packDir = abs
		}
	}
	groups := groupFiles(changed)
	for _, language := range sortedLanguages(groups) {
		capability, analyzed, method := p.runPack(runCtx, request, policy, packDir, language, groups[language], base, head, intentDigest, environmentDigest)
		if capability.Language != "" {
			pack.Capabilities = append(pack.Capabilities, capability)
		}
		pack.Methods = append(pack.Methods, method)
		pack.Methods = append(pack.Methods, analyzed.Methods...)
		pack.Targets = append(pack.Targets, analyzed.Targets...)
		pack.Scoping.Opportunities = append(pack.Scoping.Opportunities, analyzed.Opportunities...)
	}
	cacheInputs.PackVersions = cachePackVersions(pack.Capabilities)
	pack.Provenance.CacheKey, err = cache.Key(cacheInputs)
	if err != nil {
		return Result{}, err
	}
	pack.Scoping.Opportunities = sense.Rank(pack.Scoping.Opportunities)
	probeRan := p.runContractProbes(runCtx, request, policy, packDir, intent, intentDigest, &pack)
	if len(intent.EquivalenceContract.Observations) > 0 && !probeRan {
		pack.SemanticAdvisory = true
	}

	hardCeilingExhausted := errors.Is(hardCtx.Err(), context.DeadlineExceeded)
	if err := config.TelemetryAllowed(policy.Telemetry); err == nil && !hardCeilingExhausted {
		fmCtx, fmCancel := context.WithTimeout(requestCtx, managedTimeout(policy, "managed_fm", 30*time.Second))
		managedReviewPack := managedEvidencePack(pack, policy.Telemetry.Repository, managedRepositoryPaths)
		pack.AdvisoryReview = p.FMClient.Review(fmCtx, managed.FMRequest{
			Repository: policy.Telemetry.Repository, BaseRevision: base, HeadRevision: head,
			Targets: managedReviewPack.Targets, Methods: managedReviewPack.Methods,
			Instruction: "Return advisory semantic-risk suspicion and explanation only. Do not claim proof, safety, or a Behavioral Witness.",
		})
		fmCancel()
	} else if err != nil {
		pack.AdvisoryReview = domain.AdvisoryReview{Status: domain.StatusUnsupported, Reason: err.Error()}
	} else {
		pack.AdvisoryReview = domain.AdvisoryReview{Status: domain.StatusBudgetExhausted, Reason: "hard ceiling exhausted before managed review"}
	}

	pack.Blockers = policy.SelectBlockers(pack)
	if err := report.WriteAll(output, pack); err != nil {
		return Result{}, err
	}
	saveCtx, saveCancel := context.WithTimeout(requestCtx, 30*time.Second)
	_, err = state.SaveEvidence(saveCtx, pack)
	saveCancel()
	if err != nil {
		return Result{}, err
	}
	if hardCeilingExhausted {
		pack.Telemetry = domain.TelemetryOutcome{Status: "not_uploaded", Reason: "hard ceiling exhausted before managed upload", RetentionDays: 30}
	} else {
		uploadCtx, uploadCancel := context.WithTimeout(requestCtx, managedTimeout(policy, "telemetry_upload", 15*time.Second))
		pack.Telemetry = p.upload(uploadCtx, policy, pack, managedRepositoryPaths)
		uploadCancel()
	}
	if err := report.WriteAll(output, pack); err != nil {
		return Result{}, err
	}
	saveCtx, saveCancel = context.WithTimeout(requestCtx, 30*time.Second)
	digest, err := state.SaveEvidence(saveCtx, pack)
	saveCancel()
	if err != nil {
		return Result{}, err
	}
	exitCode := 0
	if len(pack.Blockers) > 0 {
		exitCode = 1
	}
	return Result{Pack: pack, EvidenceDigest: digest, ExitCode: exitCode}, nil
}

func (p Planner) runContractProbes(ctx context.Context, request Request, policy domain.Policy, packDir string, intent domain.ChangeIntent, contractDigest string, pack *domain.EvidencePack) bool {
	anyRan := false
	for _, observation := range intent.EquivalenceContract.Observations {
		target, found := matchingTarget(observation.Target, pack.Targets)
		methodID := observation.Target.Language + "_probe_" + observation.ID
		if !found {
			pack.Methods = append(pack.Methods, domain.MethodResult{
				ID: methodID, Language: observation.Target.Language, Status: domain.StatusUnsupported,
				Reason: "approved Observation Spec has no matching Verification Target",
			})
			continue
		}
		if !target.Applicability.Applicable {
			pack.Methods = append(pack.Methods, domain.MethodResult{
				ID: methodID, Language: target.Language, Status: domain.StatusInconclusive,
				Reason: target.Applicability.Reason,
			})
			continue
		}
		capability, ok := capabilityFor(target.Language, pack.Capabilities)
		if !ok || !capability.Methods["probe"] {
			pack.Methods = append(pack.Methods, domain.MethodResult{
				ID: methodID, Language: target.Language, Status: domain.StatusUnsupported,
				Reason: "language pack does not currently provide native or generated probes for this target",
			})
			continue
		}
		command, err := p.packCommand(policy, packDir, target.Language)
		if err != nil {
			pack.Methods = append(pack.Methods, domain.MethodResult{ID: methodID, Language: target.Language, Status: domain.StatusUnsupported, Reason: err.Error()})
			continue
		}
		budget := policy.Budgets.Total.Duration()
		if configured, ok := policy.Budgets.Methods[target.Language+"_probe"]; ok {
			budget = configured.Duration()
		}
		probeCtx, cancel := context.WithTimeout(ctx, budget)
		started := time.Now()
		replayBudget := budget / 2
		if replayBudget <= 0 {
			replayBudget = budget
		}
		params := packrpc.ProbeParams{
			ProtocolVersion: domain.ProtocolVersion, Target: target, Observation: observation,
			BudgetMS: replayBudget.Milliseconds(), Seed: domain.StableID(pack.RunID, target.ID, observation.ID),
		}
		_, first, firstErr := (&packrpc.Client{Command: command}).InitializeAndProbe(probeCtx, request.CoreVersion, params)
		var second packrpc.ProbeResult
		var secondErr error
		if firstErr == nil && first.Method.Status == domain.StatusRan {
			_, second, secondErr = (&packrpc.Client{Command: command}).InitializeAndProbe(probeCtx, request.CoreVersion, params)
		}
		cancel()
		wrapper := domain.MethodResult{ID: methodID, Language: target.Language, Budget: budget.String(), DurationMS: time.Since(started).Milliseconds()}
		switch {
		case errors.Is(probeCtx.Err(), context.DeadlineExceeded):
			wrapper.Status = domain.StatusBudgetExhausted
			wrapper.Reason = "probe exceeded its policy budget"
		case firstErr != nil:
			wrapper.Status = domain.StatusExecutionFailed
			wrapper.Reason = firstErr.Error()
		case secondErr != nil:
			wrapper.Status = domain.StatusExecutionFailed
			wrapper.Reason = "independent replay failed: " + secondErr.Error()
		default:
			wrapper.Status = domain.StatusRan
		}
		pack.Methods = append(pack.Methods, wrapper)
		if firstErr != nil {
			continue
		}
		pack.Methods = append(pack.Methods, first.Method)
		if first.Method.Status != domain.StatusRan {
			continue
		}
		validation := domain.MethodResult{ID: methodID + "_validation", Language: target.Language, Budget: budget.String(), Status: domain.StatusRan}
		if secondErr != nil || second.Method.Status != domain.StatusRan {
			validation.Status = domain.StatusInconclusive
			validation.Reason = "two successful independent probe runs are required"
			pack.Methods = append(pack.Methods, validation)
			pack.Divergences = append(pack.Divergences, sanitizeReportedDivergences(first.Divergences, target, observation)...)
			continue
		}
		anyRan = true
		if matched, reason := matchingDivergenceSet(first.Divergences, second.Divergences); !matched {
			validation.Status = domain.StatusFlaky
			validation.Reason = reason
			pack.Methods = append(pack.Methods, validation)
			pack.Divergences = append(pack.Divergences, sanitizeReportedDivergences(first.Divergences, target, observation)...)
			continue
		}
		pack.ReplayCapsules = append(pack.ReplayCapsules, first.Capsules...)
		validationReasons := []string{}
		validationStatus := domain.StatusRan
		for _, reported := range first.Divergences {
			validated, witnessValue, status, reason := validateIndependentReplay(
				observation, target, params.Seed, contractDigest, pack, reported, first.Capsules, second,
			)
			pack.Divergences = append(pack.Divergences, validated)
			if witnessValue != nil {
				pack.Witnesses = append(pack.Witnesses, *witnessValue)
			}
			if reason != "" {
				validationReasons = append(validationReasons, reported.ID+": "+reason)
			}
			if status == domain.StatusFlaky {
				validationStatus = domain.StatusFlaky
			} else if status != domain.StatusRan && validationStatus == domain.StatusRan {
				validationStatus = status
			}
		}
		validation.Status = validationStatus
		validation.Reason = strings.Join(validationReasons, "; ")
		pack.Methods = append(pack.Methods, validation)
	}
	return anyRan
}

func validateIndependentReplay(observation domain.ObservationSpec, target domain.VerificationTarget, seed, contractDigest string, pack *domain.EvidencePack, first domain.ObservedDivergence, firstCapsules []domain.ReplayCapsule, secondResult packrpc.ProbeResult) (domain.ObservedDivergence, *domain.BehavioralWitness, domain.MethodStatus, string) {
	recorded := sanitizeReportedDivergence(first, target, observation)
	if first.ID == "" {
		return recorded, nil, domain.StatusInconclusive, "probe divergence has no stable ID"
	}
	if first.TargetID != "" && first.TargetID != target.ID {
		return recorded, nil, domain.StatusInconclusive, "probe divergence target does not match the approved Observation Spec"
	}
	if first.ObservationSpecID != "" && first.ObservationSpecID != observation.ID {
		return recorded, nil, domain.StatusInconclusive, "probe divergence observation ID does not match the approved Observation Spec"
	}
	second, found := divergenceByID(first.ID, secondResult.Divergences)
	if !found {
		return recorded, nil, domain.StatusFlaky, "independent replay did not reproduce the divergence"
	}
	if second.TargetID != "" && second.TargetID != target.ID || second.ObservationSpecID != "" && second.ObservationSpecID != observation.ID {
		return recorded, nil, domain.StatusFlaky, "independent replay changed the target or Observation Spec identity"
	}
	firstFixtureDigest, firstFixtureErr := domain.DigestJSON(first.FixtureOrInput)
	secondFixtureDigest, secondFixtureErr := domain.DigestJSON(second.FixtureOrInput)
	if firstFixtureErr != nil || secondFixtureErr != nil || firstFixtureDigest != secondFixtureDigest {
		return recorded, nil, domain.StatusFlaky, "independent replay did not use the same fixture"
	}
	domainEvidence, legal, domainReason := approvedInputEvidence(observation, first.FixtureOrInput)
	if !legal {
		return recorded, nil, domain.StatusInconclusive, domainReason
	}
	secondEvidence, secondLegal, secondDomainReason := approvedInputEvidence(observation, second.FixtureOrInput)
	if !secondLegal {
		return recorded, nil, domain.StatusInconclusive, "independent replay input: " + secondDomainReason
	}
	if !reflect.DeepEqual(domainEvidence, secondEvidence) {
		return recorded, nil, domain.StatusFlaky, "independent replay did not establish the same domain evidence"
	}
	assessment, err := witness.AssessReplays(observation.Comparator, observation.Tolerances, []witness.ObservationPair{
		{Before: first.BeforeObservation, After: first.AfterObservation, LegalInput: true, DomainEvidence: domainEvidence},
		{Before: second.BeforeObservation, After: second.AfterObservation, LegalInput: true, DomainEvidence: secondEvidence},
	})
	if err != nil {
		return recorded, nil, domain.StatusInconclusive, "core comparator validation failed: " + err.Error()
	}
	if assessment.Status != domain.StatusRan || !assessment.Stable {
		return recorded, nil, assessment.Status, assessment.Reason
	}
	recorded.Status = assessment.Outcome
	recorded.Stable = assessment.Stable
	recorded.ReplayCount = assessment.Replays
	recorded.BeforeObservation = assessment.Before
	recorded.AfterObservation = assessment.After
	recorded.PreconditionEvidence = domainEvidence
	recorded.Comparator = observation.Comparator.BuiltIn
	if recorded.Comparator == "" {
		recorded.Comparator = observation.Comparator.Symbol
	}
	if assessment.Outcome != domain.ObservationDivergenceConfirmed {
		return recorded, nil, domain.StatusRan, "approved comparator was not violated"
	}
	firstCapsule, found := matchingCapsule(first.ReplayCapsuleDigest, firstCapsules)
	if !found {
		return recorded, nil, domain.StatusInconclusive, "reported Replay Capsule was not returned by the first probe"
	}
	secondCapsule, found := matchingCapsule(second.ReplayCapsuleDigest, secondResult.Capsules)
	if !found || firstCapsule.Digest != secondCapsule.Digest || !reflect.DeepEqual(firstCapsule, secondCapsule) {
		return recorded, nil, domain.StatusFlaky, "independent replay did not reproduce the same Replay Capsule"
	}
	if reason := validateReplayCapsule(firstCapsule, seed, recorded.Comparator); reason != "" {
		return recorded, nil, domain.StatusInconclusive, reason
	}
	recorded.ReplayCapsuleDigest = firstCapsule.Digest
	recorded.EnvironmentDigest = pack.Provenance.EnvironmentDigest
	baselineDigest, err := domain.DigestJSON(assessment.Before)
	if err != nil {
		return recorded, nil, domain.StatusInconclusive, "digest baseline observation: " + err.Error()
	}
	candidateDigest, err := domain.DigestJSON(assessment.After)
	if err != nil {
		return recorded, nil, domain.StatusInconclusive, "digest candidate observation: " + err.Error()
	}
	recorded.BaselineArtifactDigest = baselineDigest
	recorded.CandidateArtifactDigest = candidateDigest
	validatedWitness, err := witness.Promote(recorded, pack.Approval.Approved, contractDigest, domainEvidence, true)
	if err != nil {
		return recorded, nil, domain.StatusInconclusive, "witness promotion rejected: " + err.Error()
	}
	return recorded, &validatedWitness, domain.StatusRan, ""
}

func sanitizeReportedDivergences(reported []domain.ObservedDivergence, target domain.VerificationTarget, observation domain.ObservationSpec) []domain.ObservedDivergence {
	sanitized := make([]domain.ObservedDivergence, 0, len(reported))
	for _, divergence := range reported {
		sanitized = append(sanitized, sanitizeReportedDivergence(divergence, target, observation))
	}
	return sanitized
}

func sanitizeReportedDivergence(reported domain.ObservedDivergence, target domain.VerificationTarget, observation domain.ObservationSpec) domain.ObservedDivergence {
	return domain.ObservedDivergence{
		ID: reported.ID, TargetID: target.ID, ObservationSpecID: observation.ID,
		Status: domain.ObservationNotObserved, FixtureOrInput: reported.FixtureOrInput,
		BeforeObservation: reported.BeforeObservation, AfterObservation: reported.AfterObservation,
	}
}

func divergenceByID(id string, divergences []domain.ObservedDivergence) (domain.ObservedDivergence, bool) {
	for _, divergence := range divergences {
		if divergence.ID == id {
			return divergence, true
		}
	}
	return domain.ObservedDivergence{}, false
}

func matchingDivergenceSet(first, second []domain.ObservedDivergence) (bool, string) {
	if len(first) != len(second) {
		return false, "independent replay changed the set of reported divergences"
	}
	identifiers := map[string]bool{}
	for _, divergence := range first {
		if divergence.ID == "" || identifiers[divergence.ID] {
			return false, "probe returned missing or duplicate divergence IDs"
		}
		identifiers[divergence.ID] = true
	}
	for _, divergence := range second {
		if !identifiers[divergence.ID] {
			return false, "independent replay changed the set of reported divergences"
		}
		delete(identifiers, divergence.ID)
	}
	return len(identifiers) == 0, ""
}

func approvedInputEvidence(observation domain.ObservationSpec, fixture any) ([]string, bool, string) {
	if observation.Inputs == nil {
		return nil, false, "generated inputs require an independent domain validator"
	}
	fixtureDigest, err := domain.DigestJSON(fixture)
	if err != nil {
		return nil, false, "fixture cannot be canonically encoded: " + err.Error()
	}
	matched := false
	for _, approved := range observation.Inputs {
		digest, err := domain.DigestJSON(approved)
		if err == nil && digest == fixtureDigest {
			matched = true
			break
		}
	}
	if !matched {
		return nil, false, "fixture was not one of the approved explicit inputs"
	}
	evidence := []string{"fixture matched an approved explicit input"}
	for _, precondition := range observation.Preconditions {
		if precondition.Symbol != "" {
			return nil, false, "repository precondition symbols require an independent core validator"
		}
		if ok, reason := evaluateBuiltInPrecondition(precondition.BuiltIn, fixture); !ok {
			return nil, false, reason
		}
		evidence = append(evidence, "core validated precondition "+precondition.BuiltIn)
	}
	return evidence, true, ""
}

func evaluateBuiltInPrecondition(name string, fixture any) (bool, string) {
	switch name {
	case "always", "seeded_values":
		return true, ""
	case "non_null":
		if fixture != nil {
			return true, ""
		}
	case "non_negative":
		if value, ok := numericFixture(fixture); ok && value.Sign() >= 0 {
			return true, ""
		}
	case "finite":
		if finiteFixture(fixture) {
			return true, ""
		}
	case "valid_utf8":
		if value, ok := fixture.(string); ok && utf8.ValidString(value) {
			return true, ""
		}
	default:
		return false, fmt.Sprintf("built-in precondition %q has no independent core validator", name)
	}
	return false, fmt.Sprintf("fixture failed built-in precondition %q", name)
}

func numericFixture(value any) (*big.Rat, bool) {
	switch value := value.(type) {
	case json.Number:
		number, err := numeric.ParseJSONNumber(value)
		return number, err == nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(value), true
	case float32:
		return numericFixture(float64(value))
	case int:
		return new(big.Rat).SetInt64(int64(value)), true
	case int8:
		return new(big.Rat).SetInt64(int64(value)), true
	case int16:
		return new(big.Rat).SetInt64(int64(value)), true
	case int32:
		return new(big.Rat).SetInt64(int64(value)), true
	case int64:
		return new(big.Rat).SetInt64(value), true
	case uint:
		return new(big.Rat).SetUint64(uint64(value)), true
	case uint8:
		return new(big.Rat).SetUint64(uint64(value)), true
	case uint16:
		return new(big.Rat).SetUint64(uint64(value)), true
	case uint32:
		return new(big.Rat).SetUint64(uint64(value)), true
	case uint64:
		return new(big.Rat).SetUint64(value), true
	default:
		return nil, false
	}
}

func finiteFixture(value any) bool {
	if number, ok := numericFixture(value); ok {
		return number != nil
	}
	return false
}

func matchingCapsule(digest string, capsules []domain.ReplayCapsule) (domain.ReplayCapsule, bool) {
	if digest == "" {
		return domain.ReplayCapsule{}, false
	}
	for _, capsule := range capsules {
		if capsule.Digest == digest {
			return capsule, true
		}
	}
	return domain.ReplayCapsule{}, false
}

func validateReplayCapsule(capsule domain.ReplayCapsule, seed, comparator string) string {
	if !validSHA256(capsule.Digest) {
		return "Replay Capsule digest is not a SHA-256 value"
	}
	if len(capsule.ObjectDigests) == 0 || len(capsule.ReplayCommand) == 0 {
		return "Replay Capsule must include content-addressed objects and a replay command"
	}
	for _, digest := range capsule.ObjectDigests {
		if !validSHA256(digest) {
			return "Replay Capsule contains a non-SHA-256 object digest"
		}
	}
	if capsule.Seed == "" {
		return "Replay Capsule must include the core-issued seed"
	}
	if capsule.Seed != seed {
		return "Replay Capsule seed does not match the core-issued seed"
	}
	if capsule.Comparator != comparator {
		return "Replay Capsule comparator does not match the approved comparator"
	}
	return ""
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func matchingTarget(reference domain.TargetRef, targets []domain.VerificationTarget) (domain.VerificationTarget, bool) {
	var legacyMatches []domain.VerificationTarget
	for _, target := range targets {
		if target.Language != reference.Language || target.ScopeType != reference.ScopeType {
			continue
		}
		if reference.Path != "" && target.File != reference.Path {
			continue
		}
		if reference.Symbol != "" {
			if target.Symbol == reference.Symbol {
				return target, true
			}
			if !strings.Contains(reference.Symbol, ".") && strings.HasSuffix(target.Symbol, "."+reference.Symbol) {
				legacyMatches = append(legacyMatches, target)
			}
			continue
		}
		return target, true
	}
	if len(legacyMatches) == 1 {
		return legacyMatches[0], true
	}
	return domain.VerificationTarget{}, false
}

func capabilityFor(language string, capabilities []domain.PackCapability) (domain.PackCapability, bool) {
	for _, capability := range capabilities {
		if capability.Language == language {
			return capability, true
		}
	}
	return domain.PackCapability{}, false
}

func cachePackVersions(capabilities []domain.PackCapability) map[string]string {
	versions := map[string]string{"protocol": domain.ProtocolVersion}
	for _, capability := range capabilities {
		if capability.Language != "" && capability.PackVersion != "" {
			versions[capability.Language] = capability.PackVersion
		}
	}
	return versions
}

func (p Planner) resolveIntent(request Request, changed []gitx.ChangedFile, base, head, output string) (domain.ChangeIntent, string, domain.ApprovalContext, bool, string, error) {
	paths := make([]string, 0, len(changed))
	for _, file := range changed {
		paths = append(paths, file.Path)
	}
	if request.IntentPath == "" {
		proposal := config.ProposeIntent(paths, base, head)
		path := filepath.Join(output, "proposed-change-intent.yaml")
		if err := config.WriteYAML(path, proposal); err != nil {
			return domain.ChangeIntent{}, "", domain.ApprovalContext{}, true, "", err
		}
		return proposal, "", domain.ApprovalContext{Approved: false, Reason: "no checked-in Change Intent was supplied"}, true, path, nil
	}
	intent, err := config.LoadIntent(request.IntentPath)
	if err != nil {
		return domain.ChangeIntent{}, "", domain.ApprovalContext{}, true, "", err
	}
	digest, err := domain.DigestJSON(intent)
	if err != nil {
		return domain.ChangeIntent{}, "", domain.ApprovalContext{}, true, "", err
	}
	approval := config.ApprovalFromEnvironment(head)
	semanticAdvisory := !approval.Approved || intent.Classification != "behavior_preserving" || len(intent.EquivalenceContract.Observations) == 0
	if approval.Approved && len(intent.EquivalenceContract.Observations) == 0 {
		approval.Approved = false
		approval.Reason = "Equivalence Contract has no machine-checkable observations"
	}
	return intent, digest, approval, semanticAdvisory, "", nil
}

func (p Planner) runCommands(ctx context.Context, baselineRepo, candidateRepo string, policy domain.Policy) []domain.MethodResult {
	results := make([]domain.MethodResult, 0, len(policy.Commands))
	for _, spec := range policy.Commands {
		budget := policy.Budgets.Total.Duration()
		if configured, ok := policy.Budgets.Methods[spec.ID]; ok {
			budget = configured.Duration()
		}
		started := time.Now()
		image := spec.Image
		if image == "" {
			image = policy.Execution.Image
		}
		baselineCtx, baselineCancel := context.WithTimeout(ctx, budget)
		baseline, baselineErr := p.Runner.Run(baselineCtx, baselineRepo, policy.Execution, image, spec.Command)
		baselineCancel()
		method := domain.MethodResult{ID: spec.ID, Language: spec.Language, Budget: budget.String(), Findings: []domain.Finding{}}
		switch {
		case errors.Is(baselineCtx.Err(), context.DeadlineExceeded) || errors.Is(baselineErr, context.DeadlineExceeded):
			method.Status = domain.StatusBudgetExhausted
			method.Reason = "baseline command exceeded its policy budget"
		case baselineErr != nil:
			method.Status = domain.StatusExecutionFailed
			method.Reason = "baseline command could not run: " + baselineErr.Error()
		case baseline.UnsupportedReason != "":
			method.Status = domain.StatusUnsupported
			method.Reason = "baseline command is unsupported: " + baseline.UnsupportedReason
		case infrastructureExit(baseline.ExitCode):
			method.Status = domain.StatusExecutionFailed
			method.Reason = fmt.Sprintf("baseline container infrastructure failed with exit code %d", baseline.ExitCode)
		case baseline.ExitCode != 0:
			method.Status = domain.StatusInconclusive
			method.Reason = fmt.Sprintf("baseline command failed with exit code %d; a regression cannot be established", baseline.ExitCode)
		default:
			candidateCtx, candidateCancel := context.WithTimeout(ctx, budget)
			candidate, candidateErr := p.Runner.Run(candidateCtx, candidateRepo, policy.Execution, image, spec.Command)
			candidateCancel()
			switch {
			case errors.Is(candidateCtx.Err(), context.DeadlineExceeded) || errors.Is(candidateErr, context.DeadlineExceeded):
				method.Status = domain.StatusBudgetExhausted
				method.Reason = "candidate command exceeded its policy budget"
			case candidateErr != nil:
				method.Status = domain.StatusExecutionFailed
				method.Reason = "candidate command could not run: " + candidateErr.Error()
			case candidate.UnsupportedReason != "":
				method.Status = domain.StatusUnsupported
				method.Reason = "candidate command is unsupported: " + candidate.UnsupportedReason
			case infrastructureExit(candidate.ExitCode):
				method.Status = domain.StatusExecutionFailed
				method.Reason = fmt.Sprintf("candidate container infrastructure failed with exit code %d", candidate.ExitCode)
			case candidate.ExitCode != 0:
				method.Status = domain.StatusRan
				method.Findings = append(method.Findings, domain.Finding{
					ID: domain.StableID(spec.ID, candidate.Stdout, candidate.Stderr), Category: spec.Category,
					Title: spec.ID + " reported a regression", Detail: truncate(candidate.Stdout+"\n"+candidate.Stderr, 16<<10),
					Validated: true, Advisory: false, MethodID: spec.ID,
				})
			default:
				method.Status = domain.StatusRan
			}
		}
		method.DurationMS = time.Since(started).Milliseconds()
		results = append(results, method)
	}
	return results
}

func infrastructureExit(exitCode int) bool {
	return exitCode >= 125 && exitCode <= 127
}

func (p Planner) runPack(ctx context.Context, request Request, policy domain.Policy, packDir, language string, files []gitx.ChangedFile, base, head, intentDigest, environmentDigest string) (domain.PackCapability, packrpc.AnalyzeResult, domain.MethodResult) {
	command, err := p.packCommand(policy, packDir, language)
	if err != nil {
		return domain.PackCapability{}, packrpc.AnalyzeResult{}, domain.MethodResult{
			ID: language + "_pack", Language: language, Status: domain.StatusUnsupported, Reason: err.Error(),
		}
	}
	budget := policy.Budgets.Total.Duration()
	if configured, ok := policy.Budgets.Methods[language+"_pack"]; ok {
		budget = configured.Duration()
	}
	packCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	started := time.Now()
	client := &packrpc.Client{Command: command}
	capability, analyzed, err := client.InitializeAndAnalyze(packCtx, request.CoreVersion, packrpc.AnalyzeParams{
		ProtocolVersion: domain.ProtocolVersion, Repository: request.Repository, BaseRevision: base, HeadRevision: head,
		ChangedFiles: files, ContractDigest: intentDigest, EnvironmentDigest: environmentDigest,
		Seed: domain.StableID(base, head, language), BudgetMS: budget.Milliseconds(),
	})
	method := domain.MethodResult{ID: language + "_pack", Language: language, Budget: budget.String(), DurationMS: time.Since(started).Milliseconds(), Findings: []domain.Finding{}}
	switch {
	case errors.Is(packCtx.Err(), context.DeadlineExceeded):
		method.Status = domain.StatusBudgetExhausted
		method.Reason = "language pack exceeded its policy budget"
	case err != nil:
		method.Status = domain.StatusExecutionFailed
		method.Reason = err.Error()
	default:
		method.Status = domain.StatusRan
	}
	return capability, analyzed, method
}

func (p Planner) packCommand(policy domain.Policy, packDir, language string) ([]string, error) {
	if configured, ok := policy.Packs[language]; ok && len(configured.Command) > 0 {
		return slices.Clone(configured.Command), nil
	}
	switch language {
	case "go":
		executable, err := p.Executable()
		if err != nil {
			return nil, err
		}
		return []string{executable, "pack", "go"}, nil
	case "typescript":
		return []string{"node", filepath.Join(packDir, "typescript", "index.mjs")}, nil
	case "python":
		return []string{"python3", filepath.Join(packDir, "python", "simpleton_pack.py")}, nil
	case "swift":
		return nil, errors.New("Swift pack is a committed later phase and is not installed")
	default:
		return nil, fmt.Errorf("no pack for language %q", language)
	}
}

func (p Planner) upload(ctx context.Context, policy domain.Policy, pack domain.EvidencePack, repositoryPaths map[string]struct{}) domain.TelemetryOutcome {
	if err := config.TelemetryAllowed(policy.Telemetry); err != nil {
		return domain.TelemetryOutcome{Status: "not_uploaded", Reason: err.Error(), RetentionDays: 30}
	}
	managedPack := managedEvidencePack(pack, policy.Telemetry.Repository, repositoryPaths)
	payload, redactions, err := managed.NewRedactor().Evidence(managedPack)
	if err != nil {
		return domain.TelemetryOutcome{Status: "upload_failed", Reason: err.Error(), RetentionDays: 30}
	}
	remoteID, err := p.TelemetryClient.Upload(ctx, payload, policy.Telemetry.Organization, policy.Telemetry.Repository)
	if err != nil {
		return domain.TelemetryOutcome{Status: "upload_failed", Reason: err.Error(), Redactions: redactions, RetentionDays: 30}
	}
	return domain.TelemetryOutcome{Status: "uploaded", Redactions: redactions, RemoteID: remoteID, RetentionDays: 30}
}

func managedEvidencePack(pack domain.EvidencePack, repositoryIdentity string, repositoryPaths map[string]struct{}) domain.EvidencePack {
	runID := managedID("run", pack.RunID)
	managedPack := domain.EvidencePack{
		SchemaVersion:    domain.SchemaVersion,
		RunID:            runID,
		CreatedAt:        pack.CreatedAt,
		SemanticAdvisory: pack.SemanticAdvisory,
		Provenance:       managedProvenance(pack.Provenance, repositoryIdentity, repositoryPaths),
		Approval: domain.ApprovalContext{
			Approved: pack.Approval.Approved,
			Source:   managedEnum(pack.Approval.Source, "github_codeowners", "gitlab_codeowners"),
		},
		Capabilities:   managedCapabilities(pack.Capabilities),
		Targets:        managedTargets(pack.Targets, repositoryPaths),
		Methods:        managedMethodResults(pack.Methods),
		Divergences:    managedDivergences(pack.Divergences),
		Witnesses:      managedWitnesses(pack.Witnesses),
		ReplayCapsules: managedReplayCapsules(pack.ReplayCapsules, repositoryIdentity),
		Scoping: domain.ScopingPack{
			SchemaVersion: domain.SchemaVersion,
			RunID:         runID,
			Opportunities: managedOpportunities(pack.Scoping.Opportunities, repositoryPaths),
		},
		AdvisoryReview: domain.AdvisoryReview{
			Status:     managedMethodStatus(pack.AdvisoryReview.Status),
			Suspicion:  managedEnum(pack.AdvisoryReview.Suspicion, "low", "medium", "high"),
			DurationMS: pack.AdvisoryReview.DurationMS,
			CostUSD:    pack.AdvisoryReview.CostUSD,
		},
		Telemetry: domain.TelemetryOutcome{
			Status:        managedEnum(pack.Telemetry.Status, "not_uploaded", "uploaded", "upload_failed"),
			RemoteID:      managedID("remote-evidence", pack.Telemetry.RemoteID),
			RetentionDays: pack.Telemetry.RetentionDays,
		},
		Blockers: managedFindings(pack.Blockers),
	}
	return managedPack
}

func managedProvenance(provenance domain.Provenance, repositoryIdentity string, repositoryPaths map[string]struct{}) domain.Provenance {
	lockfiles := map[string]string{}
	for filename, digest := range provenance.LockfileDigests {
		if filename = managedPath(filename, repositoryPaths); filename != "" && validSHA256(digest) {
			lockfiles[filename] = digest
		}
	}
	return domain.Provenance{
		CoreVersion:       managedVersion(provenance.CoreVersion),
		SchemaVersion:     domain.SchemaVersion,
		ProtocolVersion:   domain.ProtocolVersion,
		Repository:        repositoryIdentity,
		BaseRevision:      managedGitObjectID(provenance.BaseRevision),
		HeadRevision:      managedGitObjectID(provenance.HeadRevision),
		BaseTreeDigest:    managedGitObjectID(provenance.BaseTreeDigest),
		HeadTreeDigest:    managedGitObjectID(provenance.HeadTreeDigest),
		IntentDigest:      managedSHA256(provenance.IntentDigest),
		PolicyDigest:      managedSHA256(provenance.PolicyDigest),
		EnvironmentDigest: managedSHA256(provenance.EnvironmentDigest),
		CacheKey:          managedSHA256(provenance.CacheKey),
		LockfileDigests:   lockfiles,
		TrustClass:        managedEnum(provenance.TrustClass, "trusted_branch", "untrusted_pull_request"),
	}
}

func managedCapabilities(capabilities []domain.PackCapability) []domain.PackCapability {
	result := make([]domain.PackCapability, 0, len(capabilities))
	for _, capability := range capabilities {
		methods := map[string]bool{}
		for _, method := range []string{"analyze", "probe", "cancel", "types", "type_hints", "imports", "callers", "effects", "ssa"} {
			if supported, ok := capability.Methods[method]; ok {
				methods[method] = supported
			}
		}
		result = append(result, domain.PackCapability{
			Language: managedLanguage(capability.Language), PackVersion: managedVersion(capability.PackVersion),
			ProtocolVersion: managedEnum(capability.ProtocolVersion, domain.ProtocolVersion), Methods: methods,
		})
	}
	return result
}

func managedTargets(targets []domain.VerificationTarget, repositoryPaths map[string]struct{}) []domain.VerificationTarget {
	result := make([]domain.VerificationTarget, 0, len(targets))
	for _, target := range targets {
		boundaries := make([]domain.ObservationBoundary, 0, len(target.ObservationCandidates))
		for _, boundary := range target.ObservationCandidates {
			boundaries = append(boundaries, domain.ObservationBoundary{
				Kind: managedEnum(boundary.Kind, "unchanged_caller", "public_api", "direct_unit"),
				Path: managedPath(boundary.Path, repositoryPaths), Stable: boundary.Stable, Generated: boundary.Generated,
				Confidence: boundary.Confidence,
			})
		}
		result = append(result, domain.VerificationTarget{
			ID: managedID("target", target.ID), Language: managedLanguage(target.Language),
			File: managedPath(target.File, repositoryPaths), Kind: managedEnum(target.Kind, "function", "method", "type", "module", "package", "file"),
			ScopeType:        managedEnum(target.ScopeType, "symbol", "api", "file", "package", "module"),
			BaselineArtifact: managedPath(target.BaselineArtifact, repositoryPaths), CandidateArtifact: managedPath(target.CandidateArtifact, repositoryPaths),
			Dependencies: managedPaths(target.Dependencies, repositoryPaths), ObservationCandidates: boundaries,
			Applicability: domain.RunApplicability{Applicable: target.Applicability.Applicable, Confidence: target.Applicability.Confidence},
		})
	}
	return result
}

func managedMethodResults(methods []domain.MethodResult) []domain.MethodResult {
	result := make([]domain.MethodResult, 0, len(methods))
	for _, method := range methods {
		managed := domain.MethodResult{
			ID: managedID("method", method.ID), Language: managedLanguage(method.Language), Status: managedMethodStatus(method.Status),
			Budget: managedDuration(method.Budget), DurationMS: method.DurationMS, CostUSD: method.CostUSD,
			Coverage: method.Coverage, Findings: managedFindings(method.Findings),
		}
		if method.Applicability != nil {
			managed.Applicability = &domain.RunApplicability{Applicable: method.Applicability.Applicable, Confidence: method.Applicability.Confidence}
		}
		result = append(result, managed)
	}
	return result
}

func managedFindings(findings []domain.Finding) []domain.Finding {
	result := make([]domain.Finding, 0, len(findings))
	for _, finding := range findings {
		result = append(result, domain.Finding{
			ID: managedID("finding", finding.ID), Category: managedFindingCategory(finding.Category), Title: "managed finding",
			Severity: managedEnum(finding.Severity, "info", "low", "medium", "high", "critical", "warning", "error"),
			TargetID: managedID("target", finding.TargetID), MethodID: managedID("method", finding.MethodID),
			Validated: finding.Validated, Advisory: finding.Advisory,
		})
	}
	return result
}

func managedDivergences(divergences []domain.ObservedDivergence) []domain.ObservedDivergence {
	result := make([]domain.ObservedDivergence, 0, len(divergences))
	for _, divergence := range divergences {
		result = append(result, managedDivergence(divergence))
	}
	return result
}

func managedDivergence(divergence domain.ObservedDivergence) domain.ObservedDivergence {
	return domain.ObservedDivergence{
		ID: managedID("divergence", divergence.ID), TargetID: managedID("target", divergence.TargetID),
		ObservationSpecID: managedID("observation", divergence.ObservationSpecID), Status: managedObservationStatus(divergence.Status),
		EnvironmentDigest: managedSHA256(divergence.EnvironmentDigest), BaselineArtifactDigest: managedSHA256(divergence.BaselineArtifactDigest),
		CandidateArtifactDigest: managedSHA256(divergence.CandidateArtifactDigest), ReplayCapsuleDigest: managedSHA256(divergence.ReplayCapsuleDigest),
		ReplayCount: divergence.ReplayCount, Stable: divergence.Stable,
	}
}

func managedWitnesses(witnesses []domain.BehavioralWitness) []domain.BehavioralWitness {
	result := make([]domain.BehavioralWitness, 0, len(witnesses))
	for _, witness := range witnesses {
		result = append(result, domain.BehavioralWitness{
			ID: managedID("witness", witness.ID), Divergence: managedDivergence(witness.Divergence),
			ApprovedContractDigest: managedSHA256(witness.ApprovedContractDigest),
		})
	}
	return result
}

func managedReplayCapsules(capsules []domain.ReplayCapsule, repositoryIdentity string) []domain.ReplayCapsule {
	result := make([]domain.ReplayCapsule, 0, len(capsules))
	for _, capsule := range capsules {
		digests := make([]string, 0, len(capsule.ObjectDigests))
		for _, digest := range capsule.ObjectDigests {
			if validSHA256(digest) {
				digests = append(digests, digest)
			}
		}
		result = append(result, domain.ReplayCapsule{
			Digest: managedSHA256(capsule.Digest), ObjectDigests: digests,
			Environment: map[string]string{"repository": repositoryIdentity}, Seed: managedID("seed", capsule.Seed),
		})
	}
	return result
}

func managedOpportunities(opportunities []domain.Opportunity, repositoryPaths map[string]struct{}) []domain.Opportunity {
	result := make([]domain.Opportunity, 0, len(opportunities))
	for _, opportunity := range opportunities {
		result = append(result, domain.Opportunity{
			ID: managedID("opportunity", opportunity.ID), Language: managedLanguage(opportunity.Language),
			Category:     managedEnum(opportunity.Category, "oversized_or_complex_unit"),
			AllowedFiles: managedPaths(opportunity.AllowedFiles, repositoryPaths), Benefit: opportunity.Benefit,
			ApplicabilityConfidence: opportunity.ApplicabilityConfidence, Risk: opportunity.Risk,
			ReviewEffort: opportunity.ReviewEffort, Rank: opportunity.Rank,
		})
	}
	return result
}

func managedPaths(paths []string, repositoryPaths map[string]struct{}) []string {
	result := make([]string, 0, len(paths))
	for _, value := range paths {
		if value = managedPath(value, repositoryPaths); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func managedPath(value string, repositoryPaths map[string]struct{}) string {
	if value == "" || filepath.IsAbs(value) || strings.ContainsRune(value, '\x00') {
		return ""
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "file:") {
		return ""
	}
	cleaned := path.Clean(value)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	if filepath.Separator == '\\' {
		native := filepath.Clean(value)
		if native == ".." || strings.HasPrefix(native, ".."+string(filepath.Separator)) {
			return ""
		}
	}
	if repositoryPaths != nil {
		if _, ok := repositoryPaths[value]; !ok {
			return ""
		}
	}
	return value
}

func managedID(namespace, value string) string {
	if value == "" {
		return ""
	}
	return domain.StableID("managed", namespace, value)
}

func managedLanguage(value string) string {
	return managedEnum(value, "go", "python", "typescript", "swift")
}

func managedVersion(value string) string {
	if value == "" || len(value) > 64 {
		return ""
	}
	primary := value
	if suffix := strings.IndexAny(primary, "-+"); suffix >= 0 {
		primary = primary[:suffix]
	}
	segments := strings.Split(primary, ".")
	if len(segments) != 3 {
		return ""
	}
	for _, segment := range segments {
		if segment == "" {
			return ""
		}
		for _, character := range segment {
			if character < '0' || character > '9' {
				return ""
			}
		}
	}
	for _, character := range value[len(primary):] {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '.' || character == '-' || character == '+' {
			continue
		}
		return ""
	}
	return value
}

func managedDuration(value string) string {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return ""
	}
	return duration.String()
}

func managedEnum[T ~string](value T, allowed ...T) T {
	if slices.Contains(allowed, value) {
		return value
	}
	return ""
}

func managedMethodStatus(value domain.MethodStatus) domain.MethodStatus {
	return managedEnum(value, domain.StatusRan, domain.StatusUnsupported, domain.StatusInconclusive, domain.StatusBudgetExhausted, domain.StatusFlaky, domain.StatusExecutionFailed)
}

func managedObservationStatus(value domain.ObservationStatus) domain.ObservationStatus {
	return managedEnum(value, domain.ObservationDivergenceConfirmed, domain.ObservationNoDivergence, domain.ObservationNotObserved)
}

func managedFindingCategory(value domain.FindingCategory) domain.FindingCategory {
	return managedEnum(value, domain.CategoryBuildRegression, domain.CategoryTypecheckRegression, domain.CategoryTestRegression,
		domain.CategoryStaticRegression, domain.CategoryBehavioralWitness, domain.CategoryObservedDivergence, domain.CategoryFMAdvisory)
}

func managedSHA256(value string) string {
	if validSHA256(value) {
		return value
	}
	return ""
}

func managedGitObjectID(value string) string {
	if len(value) != 40 && len(value) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}

func managedTimeout(policy domain.Policy, method string, fallback time.Duration) time.Duration {
	if configured, ok := policy.Budgets.Methods[method]; ok {
		return configured.Duration()
	}
	return fallback
}

func groupFiles(files []gitx.ChangedFile) map[string][]gitx.ChangedFile {
	groups := map[string][]gitx.ChangedFile{}
	for _, file := range files {
		if file.Language != "" {
			groups[file.Language] = append(groups[file.Language], file)
		}
	}
	return groups
}

func sortedLanguages(groups map[string][]gitx.ChangedFile) []string {
	languages := make([]string, 0, len(groups))
	for language := range groups {
		languages = append(languages, language)
	}
	slices.Sort(languages)
	return languages
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func MarshalPack(pack domain.EvidencePack) ([]byte, error) {
	return json.MarshalIndent(pack, "", "  ")
}
