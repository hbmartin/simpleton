package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/haroldmartin/simpleton/internal/domain"
)

func TestAnalyzeCLIEndToEndWithEmbeddedGoPack(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "simpleton")
	build := exec.Command("go", "build", "-o", binary, "./cmd/simpleton")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, output)
	}
	repository := t.TempDir()
	command(t, repository, "git", "init", "-q")
	command(t, repository, "git", "config", "user.email", "simpleton@example.invalid")
	command(t, repository, "git", "config", "user.name", "Simpleton Test")
	writeFixture(t, filepath.Join(repository, "go.mod"), "module example.invalid/e2e\n\ngo 1.24\n")
	writeFixture(t, filepath.Join(repository, "calc.go"), "package e2e\n\nfunc Add(a, b int) int { return a + b }\n")
	writeFixture(t, filepath.Join(repository, "caller.go"), "package e2e\n\nfunc Public(x int) int { return Add(x, 1) }\n")
	command(t, repository, "git", "add", ".")
	command(t, repository, "git", "commit", "-qm", "base")
	base := command(t, repository, "git", "rev-parse", "HEAD")
	writeFixture(t, filepath.Join(repository, "calc.go"), "package e2e\n\nfunc Add(a, b int) int { return a - b }\n")
	command(t, repository, "git", "commit", "-qam", "head")
	head := command(t, repository, "git", "rev-parse", "HEAD")
	output := filepath.Join(t.TempDir(), "run")

	analyze := exec.Command(binary, "analyze", "--repo", repository, "--base", base, "--head", head, "--output", output)
	analyze.Dir = root
	analyze.Env = append(os.Environ(),
		"SIMPLETON_CONTRACT_APPROVED=", "SIMPLETON_APPROVED_HEAD=", "SIMPLETON_REQUIRED_OWNER=", "SIMPLETON_APPROVAL_SOURCE=",
		"SIMPLETON_REPOSITORY=", "SIMPLETON_APPROVAL_ATTESTATION=",
	)
	stdout, err := analyze.CombinedOutput()
	if err != nil {
		t.Fatalf("analyze: %v: %s", err, stdout)
	}
	if !strings.Contains(string(stdout), "semantic_advisory=true") {
		t.Fatalf("unexpected CLI output: %s", stdout)
	}
	encoded, err := os.ReadFile(filepath.Join(output, "evidence-pack.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pack domain.EvidencePack
	if err := json.Unmarshal(encoded, &pack); err != nil {
		t.Fatal(err)
	}
	if len(pack.Capabilities) != 1 || pack.Capabilities[0].Language != "go" {
		t.Fatalf("embedded Go pack did not handshake: %#v", pack.Capabilities)
	}
	if len(pack.Targets) != 1 || pack.Targets[0].Symbol != "Add" || pack.Targets[0].ObservationCandidates[0].Kind != "unchanged_caller" {
		t.Fatalf("unexpected evidence targets: %#v", pack.Targets)
	}
	if pack.ProposedIntentPath == "" || pack.Provenance.CacheKey == "" {
		t.Fatalf("proposal or cache provenance missing: %#v", pack.Provenance)
	}
}

func command(t *testing.T, directory, name string, arguments ...string) string {
	t.Helper()
	cmd := exec.Command(name, arguments...)
	cmd.Dir = directory
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
