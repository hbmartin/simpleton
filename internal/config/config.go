package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
	"gopkg.in/yaml.v3"
)

func DefaultPolicy() domain.Policy {
	return domain.Policy{
		SchemaVersion: domain.SchemaVersion,
		Mode:          "shadow",
		Budgets: domain.BudgetPolicy{
			Total:       domain.Duration(30 * time.Minute),
			HardCeiling: domain.Duration(60 * time.Minute),
			Methods:     map[string]domain.Duration{},
		},
		Blocking: domain.BlockingPolicy{Allow: []domain.FindingCategory{
			domain.CategoryBuildRegression,
			domain.CategoryTypecheckRegression,
			domain.CategoryTestRegression,
			domain.CategoryStaticRegression,
			domain.CategoryBehavioralWitness,
		}},
		Execution: domain.ExecutionPolicy{
			TrustClass: "trusted_branch",
			Runtime:    "auto",
			Network:    "disabled",
			Secrets:    "none",
		},
		Packs: map[string]domain.PackCommand{},
		Telemetry: domain.TelemetryPolicy{
			RetentionDays:     30,
			TrainingPermitted: false,
		},
	}
}

func LoadPolicy(path string) (domain.Policy, error) {
	if path == "" {
		p := DefaultPolicy()
		return p, p.Validate()
	}
	var p domain.Policy
	if err := decodeStrictFile(path, &p); err != nil {
		return domain.Policy{}, fmt.Errorf("load policy: %w", err)
	}
	if p.Budgets.Total == 0 {
		p.Budgets.Total = domain.Duration(30 * time.Minute)
	}
	if p.Budgets.HardCeiling == 0 {
		p.Budgets.HardCeiling = domain.Duration(60 * time.Minute)
	}
	if p.Budgets.Methods == nil {
		p.Budgets.Methods = map[string]domain.Duration{}
	}
	if p.Packs == nil {
		p.Packs = map[string]domain.PackCommand{}
	}
	if p.Telemetry.RetentionDays == 0 {
		p.Telemetry.RetentionDays = 30
	}
	if err := p.Validate(); err != nil {
		return domain.Policy{}, fmt.Errorf("validate policy: %w", err)
	}
	return p, nil
}

func LoadIntent(path string) (domain.ChangeIntent, error) {
	var intent domain.ChangeIntent
	if err := decodeStrictFile(path, &intent); err != nil {
		return domain.ChangeIntent{}, fmt.Errorf("load intent: %w", err)
	}
	if err := intent.Validate(); err != nil {
		return domain.ChangeIntent{}, fmt.Errorf("validate intent: %w", err)
	}
	return intent, nil
}

func WriteYAML(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

func ProposeIntent(changedPaths []string, base, head string) domain.ChangeIntent {
	paths := slices.Clone(changedPaths)
	slices.Sort(paths)
	scope := make([]domain.IntentScope, 0, len(paths))
	for _, path := range paths {
		scope = append(scope, domain.IntentScope{Path: path})
	}
	return domain.ChangeIntent{
		SchemaVersion:  domain.SchemaVersion,
		ID:             "change-" + domain.StableID(base, head, strings.Join(paths, "\x00")),
		Rationale:      "Review and replace this agent-proposed rationale before approval",
		Classification: "behavior_preserving",
		Scope:          scope,
		AllowedChanges: []string{},
		EquivalenceContract: domain.EquivalenceContract{
			APIMappings:  []domain.APIMapping{},
			Observations: []domain.ObservationSpec{},
		},
	}
}

func ApprovalFromEnvironment(head string) domain.ApprovalContext {
	if path := strings.TrimSpace(os.Getenv("SIMPLETON_APPROVAL_ATTESTATION")); path != "" {
		return approvalFromAttestation(path, head, strings.TrimSpace(os.Getenv("SIMPLETON_REPOSITORY")))
	}
	approved := strings.EqualFold(os.Getenv("SIMPLETON_CONTRACT_APPROVED"), "true")
	approvedHead := strings.TrimSpace(os.Getenv("SIMPLETON_APPROVED_HEAD"))
	owner := strings.TrimSpace(os.Getenv("SIMPLETON_REQUIRED_OWNER"))
	source := strings.TrimSpace(os.Getenv("SIMPLETON_APPROVAL_SOURCE"))
	repository := strings.TrimSpace(os.Getenv("SIMPLETON_REPOSITORY"))
	ctx := domain.ApprovalContext{
		Approved:      approved && approvedHead != "" && approvedHead == head && owner != "" && repository != "" && validApprovalSource(source),
		Source:        source,
		ApprovedHead:  approvedHead,
		RequiredOwner: owner,
	}
	if !approved {
		ctx.Reason = "required code-owner review is absent"
	} else if approvedHead == "" {
		ctx.Reason = "approval is not bound to a pull-request head"
	} else if approvedHead != head {
		ctx.Reason = "approval is stale for the current head"
	} else if owner == "" {
		ctx.Reason = "git-host adapter did not identify the approving code owner"
	} else if repository == "" {
		ctx.Reason = "git-host approval is not bound to a repository identity"
	} else if !validApprovalSource(source) {
		ctx.Reason = "approval source is not a supported code-owner adapter"
	}
	return ctx
}

type GitHostAttestation struct {
	SchemaVersion     string   `json:"schema_version" yaml:"schema_version"`
	Provider          string   `json:"provider" yaml:"provider"`
	Repository        string   `json:"repository" yaml:"repository"`
	PullRequest       string   `json:"pull_request" yaml:"pull_request"`
	Head              string   `json:"head" yaml:"head"`
	RequiredOwners    []string `json:"required_owners" yaml:"required_owners"`
	ApprovingOwners   []string `json:"approving_owners" yaml:"approving_owners"`
	CodeOwnersChecked bool     `json:"codeowners_checked" yaml:"codeowners_checked"`
}

func approvalFromAttestation(path, head, repository string) domain.ApprovalContext {
	var attestation GitHostAttestation
	approval := domain.ApprovalContext{Source: "git_host_attestation"}
	if err := decodeStrictFile(path, &attestation); err != nil {
		approval.Reason = "invalid git-host attestation: " + err.Error()
		return approval
	}
	approval.Source = attestation.Provider + "_codeowners"
	approval.ApprovedHead = attestation.Head
	approval.RequiredOwner = strings.Join(attestation.RequiredOwners, ",")
	switch {
	case attestation.SchemaVersion != domain.SchemaVersion:
		approval.Reason = "git-host attestation schema version mismatch"
	case !validApprovalSource(approval.Source):
		approval.Reason = "git-host attestation provider is unsupported"
	case repository == "" || attestation.Repository != repository:
		approval.Reason = "git-host attestation repository does not match the analyzed repository identity"
	case attestation.PullRequest == "":
		approval.Reason = "git-host attestation is not bound to a pull request"
	case attestation.Head != head:
		approval.Reason = "approval is stale for the current head"
	case !attestation.CodeOwnersChecked:
		approval.Reason = "adapter did not verify CODEOWNERS at the current head"
	case len(attestation.RequiredOwners) == 0:
		approval.Reason = "git-host adapter found no required code owners"
	case !allOwnersApproved(attestation.RequiredOwners, attestation.ApprovingOwners):
		approval.Reason = "one or more required code owners have not approved the current head"
	default:
		approval.Approved = true
	}
	return approval
}

func allOwnersApproved(required, approved []string) bool {
	approvedSet := map[string]bool{}
	for _, owner := range approved {
		approvedSet[owner] = true
	}
	for _, owner := range required {
		if !approvedSet[owner] {
			return false
		}
	}
	return true
}

func validApprovalSource(source string) bool {
	return source == "github_codeowners" || source == "gitlab_codeowners"
}

func TelemetryAllowed(policy domain.TelemetryPolicy) error {
	if !policy.Enabled {
		return errors.New("managed telemetry is disabled")
	}
	if !policy.OrgAdminOptIn {
		return errors.New("organization administrator has not opted in")
	}
	if !validManagedOrganization(policy.Organization) {
		return errors.New("managed organization identity is missing or invalid")
	}
	if !validManagedRepository(policy.Repository) {
		return errors.New("managed repository identity is missing or invalid")
	}
	if !slices.Contains(policy.AllowedRepos, policy.Repository) {
		return errors.New("repository is not allowlisted")
	}
	if policy.RetentionDays != 30 {
		return errors.New("raw managed evidence retention must be 30 days")
	}
	if policy.TrainingPermitted {
		return errors.New("training is not permitted by the base product policy")
	}
	return nil
}

func validManagedOrganization(value string) bool {
	return validManagedIdentitySegment(value)
}

func validManagedRepository(value string) bool {
	segments := strings.Split(value, "/")
	if len(segments) < 2 {
		return false
	}
	for _, segment := range segments {
		if !validManagedIdentitySegment(segment) {
			return false
		}
	}
	return true
}

func validManagedIdentitySegment(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func decodeStrictFile(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	err = dec.Decode(&extra)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err == nil {
		return errors.New("multiple YAML documents are not allowed")
	}
	return nil
}
