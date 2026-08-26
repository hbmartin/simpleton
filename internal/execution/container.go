package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
)

type CommandResult struct {
	ExitCode          int
	Stdout            string
	Stderr            string
	Duration          time.Duration
	Runtime           string
	Image             string
	UnsupportedReason string
}

type Runner interface {
	Run(context.Context, string, domain.ExecutionPolicy, string, []string) (CommandResult, error)
}

type ContainerRunner struct {
	Runtime string
	Lookup  func(string) (string, error)
}

func (r ContainerRunner) ResolveRuntime(preferred string) (string, error) {
	lookup := r.Lookup
	if lookup == nil {
		lookup = exec.LookPath
	}
	if preferred != "" && preferred != "auto" {
		path, err := lookup(preferred)
		if err != nil {
			return "", fmt.Errorf("container runtime %q is unavailable: %w", preferred, err)
		}
		return path, nil
	}
	for _, candidate := range []string{"podman", "docker"} {
		if path, err := lookup(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("no rootless container runtime found (podman or docker)")
}

func (r ContainerRunner) BuildArgs(repo, image string, command []string) ([]string, error) {
	if image == "" {
		return nil, errors.New("pinned container image is required")
	}
	if !strings.Contains(image, "@sha256:") {
		return nil, errors.New("container image must be pinned by SHA-256 digest")
	}
	if len(command) == 0 {
		return nil, errors.New("container command is required")
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	args := []string{
		"run", "--rm", "--network", "none", "--read-only",
		"--user", "65532:65532",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", "512", "--tmpfs", "/tmp:rw,noexec,nosuid,size=512m",
		"--tmpfs", "/workspace-cache:rw,nosuid,size=1g",
		"--env", "HOME=/tmp/simpleton-home",
		"--env", "XDG_CACHE_HOME=/workspace-cache",
		"--env", "LANG=C.UTF-8",
		"--env", "TZ=UTC",
		"--volume", abs + ":/workspace:ro",
		"--workdir", "/workspace",
		image,
	}
	return append(args, command...), nil
}

func (r ContainerRunner) Run(ctx context.Context, repo string, policy domain.ExecutionPolicy, image string, command []string) (CommandResult, error) {
	if policy.TrustClass != "trusted_branch" || policy.Network != "disabled" || policy.Secrets != "none" {
		return CommandResult{}, errors.New("execution policy violates the initial trust boundary")
	}
	runtimePath, err := r.ResolveRuntime(policy.Runtime)
	if err != nil {
		return CommandResult{UnsupportedReason: err.Error()}, nil
	}
	args, err := r.BuildArgs(repo, image, command)
	if err != nil {
		return CommandResult{}, err
	}
	started := time.Now()
	cmd := exec.CommandContext(ctx, runtimePath, args...)
	var cancellationWon atomic.Bool
	cmd.Cancel = func() error {
		err := cmd.Process.Kill()
		if err == nil {
			cancellationWon.Store(true)
		}
		return err
	}
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	result := CommandResult{
		Stdout: stdout.String(), Stderr: stderr.String(), Duration: time.Since(started),
		Runtime: filepath.Base(runtimePath), Image: image,
	}
	if err == nil {
		return result, nil
	}
	return classifyRunError(ctx, result, err, cancellationWon.Load(), stderr.String())
}

func classifyRunError(ctx context.Context, result CommandResult, runErr error, cancellationWon bool, stderr string) (CommandResult, error) {
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		exitCode := exitErr.ExitCode()
		if cancellationWon && cancellationExitCode(exitCode) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
		}
		result.ExitCode = exitCode
		if containerInfrastructureExitCode(exitCode) {
			return result, fmt.Errorf("container runtime failed before the policy command completed (exit %d): %s", exitCode, strings.TrimSpace(stderr))
		}
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(runErr, ctxErr) {
		return result, ctxErr
	}
	return result, fmt.Errorf("run container: %w: %s", runErr, strings.TrimSpace(stderr))
}

func cancellationExitCode(exitCode int) bool {
	return exitCode < 0 || runtime.GOOS == "windows" && exitCode == 1
}

func containerInfrastructureExitCode(exitCode int) bool {
	return exitCode >= 125 && exitCode <= 127
}
