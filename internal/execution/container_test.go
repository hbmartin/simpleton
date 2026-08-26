package execution

import (
	"context"
	"errors"
	"os"
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
