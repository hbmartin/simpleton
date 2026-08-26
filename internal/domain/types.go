package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	SchemaVersion   = "1"
	ProtocolVersion = "1"
)

type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

type ChangeIntent struct {
	SchemaVersion       string              `json:"schema_version" yaml:"schema_version"`
	ID                  string              `json:"id" yaml:"id"`
	Rationale           string              `json:"rationale" yaml:"rationale"`
	Classification      string              `json:"classification" yaml:"classification"`
	Scope               []IntentScope       `json:"scope" yaml:"scope"`
	AllowedChanges      []string            `json:"allowed_changes,omitempty" yaml:"allowed_changes,omitempty"`
	EquivalenceContract EquivalenceContract `json:"equivalence_contract" yaml:"equivalence_contract"`
}

type IntentScope struct {
	Package string `json:"package,omitempty" yaml:"package,omitempty"`
	Path    string `json:"path,omitempty" yaml:"path,omitempty"`
	Symbol  string `json:"symbol,omitempty" yaml:"symbol,omitempty"`
}

type EquivalenceContract struct {
	APIMappings  []APIMapping      `json:"api_mappings" yaml:"api_mappings"`
	Observations []ObservationSpec `json:"observations" yaml:"observations"`
}

type APIMapping struct {
	Before string `json:"before" yaml:"before"`
	After  string `json:"after" yaml:"after"`
}

type ObservationSpec struct {
	ID            string         `json:"id" yaml:"id"`
	Target        TargetRef      `json:"target" yaml:"target"`
	Setup         []HookRef      `json:"setup,omitempty" yaml:"setup,omitempty"`
	Inputs        []any          `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Generator     *HookRef       `json:"generator,omitempty" yaml:"generator,omitempty"`
	Preconditions []HookRef      `json:"preconditions" yaml:"preconditions"`
	CallsOrEvents []string       `json:"calls_or_events" yaml:"calls_or_events"`
	Observables   []string       `json:"observables" yaml:"observables"`
	Normalizers   []HookRef      `json:"normalizers,omitempty" yaml:"normalizers,omitempty"`
	Comparator    ComparatorSpec `json:"comparator" yaml:"comparator"`
	Tolerances    *Tolerances    `json:"tolerances,omitempty" yaml:"tolerances,omitempty"`
}

type TargetRef struct {
	Language  string `json:"language" yaml:"language"`
	ScopeType string `json:"scope_type" yaml:"scope_type"`
	Path      string `json:"path,omitempty" yaml:"path,omitempty"`
	Symbol    string `json:"symbol,omitempty" yaml:"symbol,omitempty"`
}

type HookRef struct {
	BuiltIn string `json:"built_in,omitempty" yaml:"built_in,omitempty"`
	Symbol  string `json:"symbol,omitempty" yaml:"symbol,omitempty"`
}

type ComparatorSpec struct {
	BuiltIn string `json:"built_in,omitempty" yaml:"built_in,omitempty"`
	Symbol  string `json:"symbol,omitempty" yaml:"symbol,omitempty"`
}

type Tolerances struct {
	Absolute *float64 `json:"absolute,omitempty" yaml:"absolute,omitempty"`
	Relative *float64 `json:"relative,omitempty" yaml:"relative,omitempty"`
}

type Policy struct {
	SchemaVersion string                 `json:"schema_version" yaml:"schema_version"`
	Mode          string                 `json:"mode" yaml:"mode"`
	Budgets       BudgetPolicy           `json:"budgets" yaml:"budgets"`
	Blocking      BlockingPolicy         `json:"blocking" yaml:"blocking"`
	Execution     ExecutionPolicy        `json:"execution" yaml:"execution"`
	Commands      []CommandSpec          `json:"commands,omitempty" yaml:"commands,omitempty"`
	Packs         map[string]PackCommand `json:"packs,omitempty" yaml:"packs,omitempty"`
	Telemetry     TelemetryPolicy        `json:"telemetry" yaml:"telemetry"`
}

type BudgetPolicy struct {
	Total       Duration            `json:"total" yaml:"total"`
	HardCeiling Duration            `json:"hard_ceiling" yaml:"hard_ceiling"`
	Methods     map[string]Duration `json:"methods,omitempty" yaml:"methods,omitempty"`
}

type BlockingPolicy struct {
	Allow []FindingCategory `json:"allow" yaml:"allow"`
}

type ExecutionPolicy struct {
	TrustClass string `json:"trust_class" yaml:"trust_class"`
	Runtime    string `json:"runtime,omitempty" yaml:"runtime,omitempty"`
	Image      string `json:"image,omitempty" yaml:"image,omitempty"`
	Network    string `json:"network" yaml:"network"`
	Secrets    string `json:"secrets" yaml:"secrets"`
}

type CommandSpec struct {
	ID         string          `json:"id" yaml:"id"`
	Category   FindingCategory `json:"category" yaml:"category"`
	Language   string          `json:"language,omitempty" yaml:"language,omitempty"`
	Command    []string        `json:"command" yaml:"command"`
	Image      string          `json:"image,omitempty" yaml:"image,omitempty"`
	Confidence string          `json:"confidence,omitempty" yaml:"confidence,omitempty"`
}

type PackCommand struct {
	Command []string `json:"command" yaml:"command"`
}

type TelemetryPolicy struct {
	Enabled           bool     `json:"enabled" yaml:"enabled"`
	OrgAdminOptIn     bool     `json:"org_admin_opt_in" yaml:"org_admin_opt_in"`
	Organization      string   `json:"organization,omitempty" yaml:"organization,omitempty"`
	Repository        string   `json:"repository,omitempty" yaml:"repository,omitempty"`
	AllowedRepos      []string `json:"allowed_repositories,omitempty" yaml:"allowed_repositories,omitempty"`
	RetentionDays     int      `json:"retention_days" yaml:"retention_days"`
	TrainingPermitted bool     `json:"training_permitted" yaml:"training_permitted"`
}

type FindingCategory string

const (
	CategoryBuildRegression     FindingCategory = "build_regression"
	CategoryTypecheckRegression FindingCategory = "typecheck_regression"
	CategoryTestRegression      FindingCategory = "test_regression"
	CategoryStaticRegression    FindingCategory = "static_security_regression"
	CategoryBehavioralWitness   FindingCategory = "behavioral_witness"
	CategoryObservedDivergence  FindingCategory = "observed_divergence"
	CategoryFMAdvisory          FindingCategory = "fm_advisory"
)

var blockEligible = map[FindingCategory]bool{
	CategoryBuildRegression:     true,
	CategoryTypecheckRegression: true,
	CategoryTestRegression:      true,
	CategoryStaticRegression:    true,
	CategoryBehavioralWitness:   true,
}

type VerificationTarget struct {
	ID                    string                `json:"id"`
	Language              string                `json:"language"`
	File                  string                `json:"file"`
	Symbol                string                `json:"symbol,omitempty"`
	Kind                  string                `json:"kind"`
	ScopeType             string                `json:"scope_type"`
	BaselineArtifact      string                `json:"baseline_artifact,omitempty"`
	CandidateArtifact     string                `json:"candidate_artifact,omitempty"`
	Dependencies          []string              `json:"dependencies,omitempty"`
	ObservationCandidates []ObservationBoundary `json:"observation_candidates,omitempty"`
	Applicability         RunApplicability      `json:"applicability"`
}

type ObservationBoundary struct {
	Kind       string  `json:"kind"`
	Symbol     string  `json:"symbol,omitempty"`
	Path       string  `json:"path,omitempty"`
	Stable     bool    `json:"stable"`
	Generated  bool    `json:"generated"`
	Confidence float64 `json:"confidence"`
}

type PackCapability struct {
	Language        string            `json:"language"`
	PackVersion     string            `json:"pack_version"`
	ProtocolVersion string            `json:"protocol_version"`
	Methods         map[string]bool   `json:"methods"`
	Requirements    map[string]string `json:"requirements,omitempty"`
	TrustClasses    []string          `json:"trust_classes,omitempty"`
}

type RunApplicability struct {
	Applicable bool     `json:"applicable"`
	Reason     string   `json:"reason,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
	Risks      []string `json:"risks,omitempty"`
}

type MethodStatus string

const (
	StatusRan             MethodStatus = "ran"
	StatusUnsupported     MethodStatus = "unsupported"
	StatusInconclusive    MethodStatus = "inconclusive"
	StatusBudgetExhausted MethodStatus = "budget_exhausted"
	StatusFlaky           MethodStatus = "flaky"
	StatusExecutionFailed MethodStatus = "execution_failed"
)

type MethodResult struct {
	ID            string            `json:"id"`
	Language      string            `json:"language,omitempty"`
	Status        MethodStatus      `json:"status"`
	Reason        string            `json:"reason,omitempty"`
	Budget        string            `json:"budget,omitempty"`
	DurationMS    int64             `json:"duration_ms,omitempty"`
	CostUSD       *float64          `json:"cost_usd,omitempty"`
	Coverage      *Coverage         `json:"coverage,omitempty"`
	Findings      []Finding         `json:"findings,omitempty"`
	Applicability *RunApplicability `json:"applicability,omitempty"`
}

type Coverage struct {
	TargetsTotal    int     `json:"targets_total,omitempty"`
	TargetsObserved int     `json:"targets_observed,omitempty"`
	Ratio           float64 `json:"ratio,omitempty"`
}

type Finding struct {
	ID        string          `json:"id"`
	Category  FindingCategory `json:"category"`
	Title     string          `json:"title"`
	Detail    string          `json:"detail,omitempty"`
	Severity  string          `json:"severity,omitempty"`
	TargetID  string          `json:"target_id,omitempty"`
	MethodID  string          `json:"method_id,omitempty"`
	Validated bool            `json:"validated"`
	Advisory  bool            `json:"advisory"`
}

type ObservationStatus string

const (
	ObservationDivergenceConfirmed ObservationStatus = "divergence_confirmed"
	ObservationNoDivergence        ObservationStatus = "no_divergence_observed"
	ObservationNotObserved         ObservationStatus = "not_observed"
)

type ObservedDivergence struct {
	ID                      string            `json:"id"`
	TargetID                string            `json:"target_id"`
	ObservationSpecID       string            `json:"observation_spec_id,omitempty"`
	Status                  ObservationStatus `json:"status"`
	FixtureOrInput          any               `json:"fixture_or_input,omitempty"`
	BeforeObservation       any               `json:"before_observation,omitempty"`
	AfterObservation        any               `json:"after_observation,omitempty"`
	Comparator              string            `json:"comparator,omitempty"`
	EnvironmentDigest       string            `json:"environment_digest,omitempty"`
	BaselineArtifactDigest  string            `json:"baseline_artifact_digest,omitempty"`
	CandidateArtifactDigest string            `json:"candidate_artifact_digest,omitempty"`
	ReplayCapsuleDigest     string            `json:"replay_capsule_digest,omitempty"`
	ReplayCount             int               `json:"replay_count"`
	Stable                  bool              `json:"stable"`
	PreconditionEvidence    []string          `json:"precondition_evidence,omitempty"`
}

type BehavioralWitness struct {
	ID                      string             `json:"id"`
	Divergence              ObservedDivergence `json:"divergence"`
	DomainValidityEvidence  []string           `json:"domain_validity_evidence"`
	ContractRelevanceStatus string             `json:"contract_relevance_status"`
	ApprovedContractDigest  string             `json:"approved_contract_digest"`
}

type ReplayCapsule struct {
	Digest        string            `json:"digest"`
	ObjectDigests []string          `json:"object_digests"`
	Environment   map[string]string `json:"environment"`
	ReplayCommand []string          `json:"replay_command"`
	Seed          string            `json:"seed,omitempty"`
	Comparator    string            `json:"comparator,omitempty"`
}

type Opportunity struct {
	ID                      string   `json:"id"`
	Language                string   `json:"language"`
	Category                string   `json:"category"`
	Region                  string   `json:"region"`
	Evidence                []string `json:"evidence"`
	AllowedFiles            []string `json:"allowed_files"`
	Benefit                 float64  `json:"benefit"`
	ApplicabilityConfidence float64  `json:"applicability_confidence"`
	Risk                    float64  `json:"risk"`
	ReviewEffort            float64  `json:"review_effort"`
	Rank                    float64  `json:"rank"`
}

type ScopingPack struct {
	SchemaVersion string        `json:"schema_version"`
	RunID         string        `json:"run_id"`
	Opportunities []Opportunity `json:"opportunities"`
}

type AdvisoryReview struct {
	Status      MethodStatus `json:"status"`
	ModelID     string       `json:"model_id,omitempty"`
	PromptID    string       `json:"prompt_id,omitempty"`
	Explanation string       `json:"explanation,omitempty"`
	Suspicion   string       `json:"suspicion,omitempty"`
	Reason      string       `json:"reason,omitempty"`
	DurationMS  int64        `json:"duration_ms,omitempty"`
	CostUSD     *float64     `json:"cost_usd,omitempty"`
}

type ApprovalContext struct {
	Approved      bool   `json:"approved"`
	Source        string `json:"source,omitempty"`
	ApprovedHead  string `json:"approved_head,omitempty"`
	RequiredOwner string `json:"required_owner,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type Provenance struct {
	CoreVersion       string            `json:"core_version"`
	SchemaVersion     string            `json:"schema_version"`
	ProtocolVersion   string            `json:"protocol_version"`
	Repository        string            `json:"repository"`
	BaseRevision      string            `json:"base_revision"`
	HeadRevision      string            `json:"head_revision"`
	BaseTreeDigest    string            `json:"base_tree_digest"`
	HeadTreeDigest    string            `json:"head_tree_digest"`
	IntentDigest      string            `json:"intent_digest,omitempty"`
	PolicyDigest      string            `json:"policy_digest"`
	EnvironmentDigest string            `json:"environment_digest"`
	CacheKey          string            `json:"cache_key"`
	LockfileDigests   map[string]string `json:"lockfile_digests,omitempty"`
	TrustClass        string            `json:"trust_class"`
	ToolVersions      map[string]string `json:"tool_versions,omitempty"`
}

type TelemetryOutcome struct {
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	Redactions    int    `json:"redactions,omitempty"`
	RemoteID      string `json:"remote_id,omitempty"`
	RetentionDays int    `json:"retention_days,omitempty"`
}

type EvidencePack struct {
	SchemaVersion      string               `json:"schema_version"`
	RunID              string               `json:"run_id"`
	CreatedAt          time.Time            `json:"created_at"`
	Provenance         Provenance           `json:"provenance"`
	Approval           ApprovalContext      `json:"approval"`
	SemanticAdvisory   bool                 `json:"semantic_advisory"`
	Capabilities       []PackCapability     `json:"capabilities"`
	Targets            []VerificationTarget `json:"targets"`
	Methods            []MethodResult       `json:"methods"`
	Divergences        []ObservedDivergence `json:"divergences"`
	Witnesses          []BehavioralWitness  `json:"witnesses"`
	ReplayCapsules     []ReplayCapsule      `json:"replay_capsules"`
	Scoping            ScopingPack          `json:"scoping"`
	AdvisoryReview     AdvisoryReview       `json:"advisory_review"`
	Telemetry          TelemetryOutcome     `json:"telemetry"`
	ProposedIntentPath string               `json:"proposed_intent_path,omitempty"`
	Blockers           []Finding            `json:"blockers"`
}

func (i ChangeIntent) Validate() error {
	var errs []error
	if i.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("intent schema_version must be %q", SchemaVersion))
	}
	if strings.TrimSpace(i.ID) == "" {
		errs = append(errs, errors.New("intent id is required"))
	}
	if strings.TrimSpace(i.Rationale) == "" {
		errs = append(errs, errors.New("intent rationale is required"))
	}
	if i.Classification != "behavior_preserving" && i.Classification != "behavior_changing" && i.Classification != "mixed" {
		errs = append(errs, errors.New("intent classification must be behavior_preserving, behavior_changing, or mixed"))
	}
	if len(i.Scope) == 0 {
		errs = append(errs, errors.New("intent scope is required"))
	}
	for n, scope := range i.Scope {
		if scope.Package == "" && scope.Path == "" && scope.Symbol == "" {
			errs = append(errs, fmt.Errorf("scope %d must identify a package, path, or symbol", n))
		}
	}
	if i.AllowedChanges == nil {
		errs = append(errs, errors.New("allowed_changes must be explicitly declared (it may be empty)"))
	}
	for n, mapping := range i.EquivalenceContract.APIMappings {
		if strings.TrimSpace(mapping.Before) == "" || strings.TrimSpace(mapping.After) == "" {
			errs = append(errs, fmt.Errorf("api mapping %d requires before and after symbols", n))
		}
	}
	for n, obs := range i.EquivalenceContract.Observations {
		if err := obs.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("observation %d: %w", n, err))
		}
	}
	return errors.Join(errs...)
}

func (o ObservationSpec) Validate() error {
	if o.ID == "" {
		return errors.New("id is required")
	}
	if !slices.Contains([]string{"go", "typescript", "python", "swift"}, o.Target.Language) || !slices.Contains([]string{"api", "symbol", "module", "package"}, o.Target.ScopeType) {
		return errors.New("target language and scope_type are required")
	}
	if o.Target.Path == "" && o.Target.Symbol == "" {
		return errors.New("target path or symbol is required")
	}
	if o.Inputs == nil && o.Generator == nil {
		return errors.New("explicit inputs or a reviewed generator is required")
	}
	if o.Inputs != nil && o.Generator != nil {
		return errors.New("inputs and generator are mutually exclusive")
	}
	if len(o.Preconditions) == 0 {
		return errors.New("at least one machine-checkable precondition is required")
	}
	hooks := append([]HookRef{}, o.Setup...)
	hooks = append(hooks, o.Preconditions...)
	hooks = append(hooks, o.Normalizers...)
	if o.Generator != nil {
		hooks = append(hooks, *o.Generator)
	}
	for _, hook := range hooks {
		if err := validateHook(hook); err != nil {
			return err
		}
	}
	if len(o.CallsOrEvents) == 0 || len(o.Observables) == 0 {
		return errors.New("calls_or_events and observables must be nonempty")
	}
	if (o.Comparator.BuiltIn == "") == (o.Comparator.Symbol == "") {
		return errors.New("comparator must set exactly one of built_in or symbol")
	}
	if o.Comparator.BuiltIn != "" && !slices.Contains([]string{"exact", "structural_json", "unordered_collection", "exception_type", "exception"}, o.Comparator.BuiltIn) {
		return fmt.Errorf("unknown built-in comparator %q", o.Comparator.BuiltIn)
	}
	if o.Comparator.Symbol != "" && !validSymbol(o.Comparator.Symbol) {
		return errors.New("comparator symbol is not a valid repository helper reference")
	}
	if o.Tolerances != nil {
		for _, tolerance := range []*float64{o.Tolerances.Absolute, o.Tolerances.Relative} {
			if tolerance != nil && (*tolerance < 0 || math.IsNaN(*tolerance) || math.IsInf(*tolerance, 0)) {
				return errors.New("tolerances must be finite and nonnegative")
			}
		}
	}
	return nil
}

func validateHook(h HookRef) error {
	if (h.BuiltIn == "") == (h.Symbol == "") {
		return errors.New("hook must set exactly one of built_in or symbol")
	}
	if h.BuiltIn != "" && !slices.Contains([]string{
		"always", "non_null", "non_negative", "finite", "valid_utf8", "json_schema", "seeded_values",
		"structural_json", "sort_collection", "exception_type", "trim_whitespace",
	}, h.BuiltIn) {
		return fmt.Errorf("unknown reviewed built-in hook %q", h.BuiltIn)
	}
	if h.Symbol != "" && !validSymbol(h.Symbol) {
		return errors.New("hook symbol is not a valid repository helper reference")
	}
	return nil
}

var symbolPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_./:#-]*$`)

func validSymbol(value string) bool { return symbolPattern.MatchString(value) }

func (p Policy) Validate() error {
	var errs []error
	if p.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("policy schema_version must be %q", SchemaVersion))
	}
	if p.Mode != "shadow" && p.Mode != "blocking" {
		errs = append(errs, errors.New("policy mode must be shadow or blocking"))
	}
	if p.Budgets.Total.Duration() <= 0 || p.Budgets.HardCeiling.Duration() <= 0 {
		errs = append(errs, errors.New("policy budgets must be positive"))
	}
	if p.Budgets.Total.Duration() > p.Budgets.HardCeiling.Duration() {
		errs = append(errs, errors.New("policy total budget exceeds hard ceiling"))
	}
	if p.Budgets.HardCeiling.Duration() > 60*time.Minute {
		errs = append(errs, errors.New("policy hard ceiling cannot exceed 60m"))
	}
	if p.Execution.Network != "disabled" || p.Execution.Secrets != "none" {
		errs = append(errs, errors.New("initial execution policy requires network=disabled and secrets=none"))
	}
	if p.Execution.TrustClass != "trusted_branch" {
		errs = append(errs, errors.New("initial execution accepts trusted_branch only"))
	}
	for id, budget := range p.Budgets.Methods {
		if budget.Duration() <= 0 || budget.Duration() > p.Budgets.Total.Duration() {
			errs = append(errs, fmt.Errorf("method budget %q must be positive and cannot exceed total budget", id))
		}
	}
	seenCommands := map[string]bool{}
	for _, command := range p.Commands {
		if command.ID == "" || len(command.Command) == 0 {
			errs = append(errs, errors.New("policy commands require id and command"))
		}
		if seenCommands[command.ID] {
			errs = append(errs, fmt.Errorf("duplicate command id %q", command.ID))
		}
		seenCommands[command.ID] = true
		if !blockEligible[command.Category] || command.Category == CategoryBehavioralWitness {
			errs = append(errs, fmt.Errorf("command %q has invalid deterministic category %q", command.ID, command.Category))
		}
		if command.Category == CategoryStaticRegression && command.Confidence != "high" {
			errs = append(errs, fmt.Errorf("static/security command %q must declare confidence=high", command.ID))
		}
		image := command.Image
		if image == "" {
			image = p.Execution.Image
		}
		if !strings.Contains(image, "@sha256:") {
			errs = append(errs, fmt.Errorf("command %q requires a digest-pinned container image", command.ID))
		}
	}
	for _, category := range p.Blocking.Allow {
		if !blockEligible[category] {
			errs = append(errs, fmt.Errorf("category %q can never block", category))
		}
	}
	if p.Telemetry.TrainingPermitted {
		errs = append(errs, errors.New("training_permitted must be false without separate consent"))
	}
	if p.Telemetry.Enabled && p.Telemetry.RetentionDays != 30 {
		errs = append(errs, errors.New("managed raw evidence retention must be 30 days"))
	}
	return errors.Join(errs...)
}

func (p Policy) SelectBlockers(pack EvidencePack) []Finding {
	if p.Mode != "blocking" {
		return nil
	}
	allowed := make(map[FindingCategory]bool, len(p.Blocking.Allow))
	for _, category := range p.Blocking.Allow {
		allowed[category] = true
	}
	var blockers []Finding
	for _, method := range pack.Methods {
		if method.Status != StatusRan {
			continue
		}
		for _, finding := range method.Findings {
			if blockEligible[finding.Category] && allowed[finding.Category] && finding.Validated && !finding.Advisory {
				if pack.SemanticAdvisory && finding.Category == CategoryBehavioralWitness {
					continue
				}
				blockers = append(blockers, finding)
			}
		}
	}
	for _, witness := range pack.Witnesses {
		if pack.SemanticAdvisory {
			continue
		}
		if allowed[CategoryBehavioralWitness] && witness.ContractRelevanceStatus == "violated" && witness.ApprovedContractDigest != "" {
			blockers = append(blockers, Finding{
				ID: witness.ID, Category: CategoryBehavioralWitness, Title: "Behavioral Witness", Validated: true,
			})
		}
	}
	sort.Slice(blockers, func(a, b int) bool { return blockers[a].ID < blockers[b].ID })
	return blockers
}

func DigestJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func StableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}
