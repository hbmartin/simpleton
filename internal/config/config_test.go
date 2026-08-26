package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApprovalMustMatchCurrentHead(t *testing.T) {
	t.Setenv("SIMPLETON_CONTRACT_APPROVED", "true")
	t.Setenv("SIMPLETON_APPROVED_HEAD", "old")
	t.Setenv("SIMPLETON_REQUIRED_OWNER", "@maintainer")
	t.Setenv("SIMPLETON_APPROVAL_SOURCE", "github_codeowners")
	t.Setenv("SIMPLETON_REPOSITORY", "acme/repo")
	approval := ApprovalFromEnvironment("new")
	if approval.Approved || approval.Reason == "" {
		t.Fatalf("stale approval must remain advisory: %#v", approval)
	}
	t.Setenv("SIMPLETON_APPROVED_HEAD", "new")
	if approval = ApprovalFromEnvironment("new"); !approval.Approved {
		t.Fatalf("current-head approval rejected: %#v", approval)
	}
}

func TestApprovalRequiresCodeOwnerAndSupportedAdapter(t *testing.T) {
	t.Setenv("SIMPLETON_CONTRACT_APPROVED", "true")
	t.Setenv("SIMPLETON_APPROVED_HEAD", "head")
	t.Setenv("SIMPLETON_REPOSITORY", "acme/repo")
	if approval := ApprovalFromEnvironment("head"); approval.Approved {
		t.Fatalf("ownerless approval must be rejected: %#v", approval)
	}
	t.Setenv("SIMPLETON_REQUIRED_OWNER", "@maintainer")
	t.Setenv("SIMPLETON_APPROVAL_SOURCE", "handwritten")
	if approval := ApprovalFromEnvironment("head"); approval.Approved {
		t.Fatalf("unsupported adapter must be rejected: %#v", approval)
	}
}

func TestApprovalAttestationMustBindRepoHeadAndEveryOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approval.json")
	contents := []byte(`{
  "schema_version":"1", "provider":"github", "repository":"acme/repo", "pull_request":"42", "head":"head",
  "required_owners":["@alice","@bob"], "approving_owners":["@alice"], "codeowners_checked":true
}`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIMPLETON_APPROVAL_ATTESTATION", path)
	t.Setenv("SIMPLETON_REPOSITORY", "acme/repo")
	if approval := ApprovalFromEnvironment("head"); approval.Approved {
		t.Fatalf("partial owner approval must be rejected: %#v", approval)
	}
	contents = []byte(strings.Replace(string(contents), `"approving_owners":["@alice"]`, `"approving_owners":["@alice","@bob"]`, 1))
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if approval := ApprovalFromEnvironment("head"); !approval.Approved {
		t.Fatalf("complete current-head approval rejected: %#v", approval)
	}
	if approval := ApprovalFromEnvironment("different"); approval.Approved {
		t.Fatalf("stale attestation approved: %#v", approval)
	}
}

func TestStrictPolicyRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	contents := []byte(`schema_version: "1"
mode: shadow
unknown: true
budgets: {total: 30m, hard_ceiling: 60m}
blocking: {allow: []}
execution: {trust_class: trusted_branch, network: disabled, secrets: none}
telemetry: {enabled: false, org_admin_opt_in: false, retention_days: 30, training_permitted: false}
`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(path); err == nil {
		t.Fatal("unknown policy field must be rejected")
	}
}

func TestTelemetryRequiresAllowlistedRepository(t *testing.T) {
	policy := DefaultPolicy().Telemetry
	policy.Enabled = true
	policy.OrgAdminOptIn = true
	policy.Organization = "org"
	policy.Repository = "org/repo"
	if err := TelemetryAllowed(policy); err == nil {
		t.Fatal("non-allowlisted repository must be rejected")
	}
	policy.AllowedRepos = []string{"org/repo"}
	if err := TelemetryAllowed(policy); err != nil {
		t.Fatalf("allowlisted repository rejected: %v", err)
	}
}

func TestTelemetryRejectsUnsafeManagedIdentities(t *testing.T) {
	base := DefaultPolicy().Telemetry
	base.Enabled = true
	base.OrgAdminOptIn = true
	base.Organization = "org"
	base.Repository = "org/repo"
	base.AllowedRepos = []string{"org/repo"}
	if err := TelemetryAllowed(base); err != nil {
		t.Fatalf("valid managed identities rejected: %v", err)
	}

	for _, test := range []struct {
		name         string
		organization string
		repository   string
	}{
		{name: "missing organization", organization: "", repository: "org/repo"},
		{name: "organization path", organization: "/private/org", repository: "org/repo"},
		{name: "organization control character", organization: "org\nprivate", repository: "org/repo"},
		{name: "absolute repository", organization: "org", repository: "/private/repo"},
		{name: "file URI repository", organization: "org", repository: "file:///private/repo"},
		{name: "Windows repository path", organization: "org", repository: `C:\private\repo`},
		{name: "repository traversal", organization: "org", repository: "org/../repo"},
		{name: "repository control character", organization: "org", repository: "org/repo\nprivate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := base
			policy.Organization = test.organization
			policy.Repository = test.repository
			policy.AllowedRepos = []string{test.repository}
			if err := TelemetryAllowed(policy); err == nil {
				t.Fatal("unsafe managed identity was accepted")
			}
		})
	}
}
