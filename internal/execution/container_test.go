package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
)

func TestContainerArgsEnforceInitialTrustBoundary(t *testing.T) {
	args, err := (ContainerRunner{}).BuildArgs(t.TempDir(), "example@sha256:abc", []string{"go", "test", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"none", "--read-only", "ALL", "no-new-privileges", "65532:65532", "HOME=/tmp/simpleton-home", "LANG=C.UTF-8", "TZ=UTC"} {
		if !slices.Contains(args, required) {
			t.Fatalf("container args omit %q: %#v", required, args)
		}
	}
	for _, value := range args {
		if value == "--privileged" || value == "host" || value == "--env-file" {
			t.Fatalf("unsafe container argument %q", value)
		}
	}
}

func TestContainerDeadlineIsNotReportedAsCommandFailure(t *testing.T) {
	runtimePath := filepath.Join(t.TempDir(), "slow-runtime")
	//nolint:gosec // Executable test fixture requires owner execute permission.
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	result, err := (ContainerRunner{}).Run(ctx, t.TempDir(), domain.ExecutionPolicy{
		TrustClass: "trusted_branch", Runtime: runtimePath, Network: "disabled", Secrets: "none",
	}, "example@sha256:abc", []string{"test"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline must be returned directly, result=%#v err=%v", result, err)
	}
}

func TestCompletedNonzeroExitWinsOverConcurrentContextExpiry(t *testing.T) {
	runErr := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 7").Run()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := classifyRunError(ctx, CommandResult{}, runErr, false, "failed")
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("real exit was masked by concurrent cancellation: result=%#v err=%v", result, err)
	}
}

func TestCompletedNonzeroExitWinsEvenWhenCancelKillRaces(t *testing.T) {
	runErr := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 7").Run()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := classifyRunError(ctx, CommandResult{}, runErr, true, "")
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("real exit was masked by a successful but losing kill: result=%#v err=%v", result, err)
	}
}

func TestCancellationThatSignalsProcessWinsOverSignaledExit(t *testing.T) {
	runErr := exec.CommandContext(t.Context(), "/bin/sh", "-c", "kill -KILL $$").Run()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := classifyRunError(ctx, CommandResult{}, runErr, true, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("winning cancellation was not preserved: %v", err)
	}
}
