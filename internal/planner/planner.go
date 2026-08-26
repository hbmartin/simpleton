package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/haroldmartin/simpleton/internal/cache"
	"github.com/haroldmartin/simpleton/internal/config"
	"github.com/haroldmartin/simpleton/internal/domain"
	"github.com/haroldmartin/simpleton/internal/execution"
	"github.com/haroldmartin/simpleton/internal/gitx"
	"github.com/haroldmartin/simpleton/internal/managed"
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
	cacheKey, err := cache.Key(cache.Inputs{
		BaseTree: baseTree, HeadTree: headTree, Worktree: worktreeDigest, Lockfiles: lockfiles,
		Toolchains: map[string]string{"go": runtime.Version()}, Environment: environmentDigest,
		Contract: intentDigest, Policy: policyDigest, Comparator: comparatorDigest,
		Seed: domain.StableID(base, head), Budgets: map[string]string{"total": policy.Budgets.Total.Duration().String()},
		CoreVersion: request.CoreVersion, PackVersions: map[string]string{"protocol": domain.ProtocolVersion},
	})
	if err != nil {
		return Result{}, err
	}
	pack := domain.EvidencePack{
		SchemaVersion: domain.SchemaVersion,
		RunID:         runID,
		CreatedAt:     createdAt,
		Provenance: domain.Provenance{
			CoreVersion: request.CoreVersion, SchemaVersion: domain.SchemaVersion, ProtocolVersion: domain.ProtocolVersion,
			Repository: repo.Path, BaseRevision: base, HeadRevision: head, BaseTreeDigest: baseTree, HeadTreeDigest: headTree,
			IntentDigest: intentDigest, PolicyDigest: policyDigest, EnvironmentDigest: environmentDigest,
			CacheKey: cacheKey, LockfileDigests: lockfiles,
			TrustClass:   policy.Execution.TrustClass,
			ToolVersions: map[string]string{"go": runtime.Version()},
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
	executionRepository := repo.Path
	cleanupWorktree := func() error { return nil }
	if len(policy.Commands) > 0 {
		executionRepository, cleanupWorktree, err = repo.DetachedWorktree(runCtx, head)
		if err != nil {
			return Result{}, fmt.Errorf("prepare detached execution worktree: %w", err)
		}
		defer cleanupWorktree()
	}
	commandMethods := p.runCommands(runCtx, executionRepository, policy)
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
	pack.Scoping.Opportunities = sense.Rank(pack.Scoping.Opportunities)
	probeRan := p.runContractProbes(runCtx, request, policy, packDir, intent, intentDigest, &pack)
	if len(intent.EquivalenceContract.Observations) > 0 && !probeRan {
		pack.SemanticAdvisory = true
	}

	if err := config.TelemetryAllowed(policy.Telemetry); err == nil {
		pack.AdvisoryReview = p.FMClient.Review(runCtx, managed.FMRequest{
			Repository: policy.Telemetry.Repository, BaseRevision: base, HeadRevision: head,
			Targets: pack.Targets, Methods: pack.Methods,
			Instruction: "Return advisory semantic-risk suspicion and explanation only. Do not claim proof, safety, or a Behavioral Witness.",
		})
	} else {
		pack.AdvisoryReview = domain.AdvisoryReview{Status: domain.StatusUnsupported, Reason: err.Error()}
	}

	pack.Blockers = policy.SelectBlockers(pack)
	if err := report.WriteAll(output, pack); err != nil {
		return Result{}, err
	}
	digest, err := state.SaveEvidence(ctx, pack)
	if err != nil {
		return Result{}, err
	}
	pack.Telemetry = p.upload(runCtx, policy, pack)
	if err := report.WriteAll(output, pack); err != nil {
		return Result{}, err
	}
	digest, err = state.SaveEvidence(ctx, pack)
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
		_, probed, err := (&packrpc.Client{Command: command}).InitializeAndProbe(probeCtx, request.CoreVersion, packrpc.ProbeParams{
			ProtocolVersion: domain.ProtocolVersion, Target: target, Observation: observation,
			BudgetMS: budget.Milliseconds(), Seed: domain.StableID(pack.RunID, target.ID, observation.ID),
		})
		cancel()
		wrapper := domain.MethodResult{ID: methodID, Language: target.Language, Budget: budget.String(), DurationMS: time.Since(started).Milliseconds()}
		switch {
		case errors.Is(probeCtx.Err(), context.DeadlineExceeded):
			wrapper.Status = domain.StatusBudgetExhausted
			wrapper.Reason = "probe exceeded its policy budget"
		case err != nil:
			wrapper.Status = domain.StatusExecutionFailed
			wrapper.Reason = err.Error()
		default:
			wrapper.Status = domain.StatusRan
		}
		pack.Methods = append(pack.Methods, wrapper)
		if err != nil {
			continue
		}
		pack.Methods = append(pack.Methods, probed.Method)
		if probed.Method.Status == domain.StatusRan {
			anyRan = true
		}
		pack.ReplayCapsules = append(pack.ReplayCapsules, probed.Capsules...)
		for _, divergence := range probed.Divergences {
			if divergence.TargetID == "" {
				divergence.TargetID = target.ID
			}
			if divergence.ObservationSpecID == "" {
				divergence.ObservationSpecID = observation.ID
			}
			pack.Divergences = append(pack.Divergences, divergence)
			violated, _, compareErr := witness.Compare(observation.Comparator, observation.Tolerances, divergence.BeforeObservation, divergence.AfterObservation)
			if compareErr != nil || !violated || !capsulePresent(divergence.ReplayCapsuleDigest, probed.Capsules) {
				continue
			}
			validated, promoteErr := witness.Promote(
				divergence, pack.Approval.Approved, contractDigest, divergence.PreconditionEvidence, true,
			)
			if promoteErr == nil {
				pack.Witnesses = append(pack.Witnesses, validated)
			}
		}
	}
	return anyRan
}

func matchingTarget(reference domain.TargetRef, targets []domain.VerificationTarget) (domain.VerificationTarget, bool) {
	for _, target := range targets {
		if target.Language != reference.Language || target.ScopeType != reference.ScopeType {
			continue
		}
		if reference.Path != "" && target.File != reference.Path {
			continue
		}
		if reference.Symbol != "" && target.Symbol != reference.Symbol {
			continue
		}
		return target, true
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

func capsulePresent(digest string, capsules []domain.ReplayCapsule) bool {
	if digest == "" {
		return false
	}
	for _, capsule := range capsules {
		if capsule.Digest == digest {
			return true
		}
	}
	return false
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

func (p Planner) runCommands(ctx context.Context, repo string, policy domain.Policy) []domain.MethodResult {
	results := make([]domain.MethodResult, 0, len(policy.Commands))
	for _, spec := range policy.Commands {
		budget := policy.Budgets.Total.Duration()
		if configured, ok := policy.Budgets.Methods[spec.ID]; ok {
			budget = configured.Duration()
		}
		methodCtx, cancel := context.WithTimeout(ctx, budget)
		started := time.Now()
		image := spec.Image
		if image == "" {
			image = policy.Execution.Image
		}
		result, err := p.Runner.Run(methodCtx, repo, policy.Execution, image, spec.Command)
		cancel()
		method := domain.MethodResult{ID: spec.ID, Language: spec.Language, Budget: budget.String(), DurationMS: time.Since(started).Milliseconds(), Findings: []domain.Finding{}}
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			method.Status = domain.StatusBudgetExhausted
			method.Reason = "method exceeded its policy budget"
		case err != nil:
			method.Status = domain.StatusExecutionFailed
			method.Reason = err.Error()
		case result.UnsupportedReason != "":
			method.Status = domain.StatusUnsupported
			method.Reason = result.UnsupportedReason
		case result.ExitCode != 0:
			method.Status = domain.StatusRan
			method.Findings = append(method.Findings, domain.Finding{
				ID: domain.StableID(spec.ID, result.Stdout, result.Stderr), Category: spec.Category,
				Title: spec.ID + " reported a regression", Detail: truncate(result.Stdout+"\n"+result.Stderr, 16<<10),
				Validated: true, Advisory: false, MethodID: spec.ID,
			})
		default:
			method.Status = domain.StatusRan
		}
		results = append(results, method)
	}
	return results
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

func (p Planner) upload(ctx context.Context, policy domain.Policy, pack domain.EvidencePack) domain.TelemetryOutcome {
	if err := config.TelemetryAllowed(policy.Telemetry); err != nil {
		return domain.TelemetryOutcome{Status: "not_uploaded", Reason: err.Error(), RetentionDays: 30}
	}
	payload, redactions, err := managed.NewRedactor().Evidence(pack)
	if err != nil {
		return domain.TelemetryOutcome{Status: "upload_failed", Reason: err.Error(), RetentionDays: 30}
	}
	remoteID, err := p.TelemetryClient.Upload(ctx, payload, policy.Telemetry.Organization, policy.Telemetry.Repository)
	if err != nil {
		return domain.TelemetryOutcome{Status: "upload_failed", Reason: err.Error(), Redactions: redactions, RetentionDays: 30}
	}
	return domain.TelemetryOutcome{Status: "uploaded", Redactions: redactions, RemoteID: remoteID, RetentionDays: 30}
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
