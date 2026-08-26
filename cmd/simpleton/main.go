package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/haroldmartin/simpleton/internal/evaluation"
	"github.com/haroldmartin/simpleton/internal/gopack"
	"github.com/haroldmartin/simpleton/internal/managed"
	"github.com/haroldmartin/simpleton/internal/packrpc"
	"github.com/haroldmartin/simpleton/internal/planner"
	"github.com/haroldmartin/simpleton/internal/store"
)

const version = "0.1.0"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "analyze":
		return runAnalyze(ctx, args[1:], stdout, stderr)
	case "pack":
		return runPack(ctx, args[1:], stderr)
	case "learn":
		return runLearn(ctx, args[1:], stdout, stderr)
	case "evaluate":
		return runEvaluate(args[1:], stdout, stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func runEvaluate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: simpleton evaluate <score|promotion> [flags]")
		return 2
	}
	var result any
	var err error
	switch args[0] {
	case "score":
		flags := flag.NewFlagSet("evaluate score", flag.ContinueOnError)
		flags.SetOutput(stderr)
		boundary := flags.Int("boundary-constructibility", 0, "score from 1 to 5")
		legal := flags.Int("legal-input-confidence", 0, "score from 1 to 5")
		relevance := flags.Int("contract-relevance", 0, "score from 1 to 5")
		replay := flags.Int("replay-stability", 0, "score from 1 to 5")
		infrastructure := flags.Int("infrastructure-effort", 0, "score from 1 to 5")
		usefulness := flags.Int("reviewer-usefulness", 0, "score from 1 to 5")
		if parseErr := flags.Parse(args[1:]); parseErr != nil {
			return 2
		}
		result, err = evaluation.Decide(evaluation.QualitativeScore{
			BoundaryConstructibility: *boundary, LegalInputConfidence: *legal, ContractRelevance: *relevance,
			ReplayStability: *replay, InfrastructureEffort: *infrastructure, ReviewerUsefulness: *usefulness,
		})
	case "promotion":
		flags := flag.NewFlagSet("evaluate promotion", flag.ContinueOnError)
		flags.SetOutput(stderr)
		targets := flags.Int("targets", 0, "targets in the language/change class")
		native := flags.Int("native-witnesses", 0, "native validated-witness count")
		generated := flags.Int("generated-witnesses", 0, "generated validated-witness count")
		if parseErr := flags.Parse(args[1:]); parseErr != nil {
			return 2
		}
		result, err = evaluation.AssessPromotion(evaluation.PromotionSample{Targets: *targets, NativeHits: *native, GeneratedHits: *generated})
	default:
		fmt.Fprintf(stderr, "unknown evaluation %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "evaluate: %v\n", err)
		return 2
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "evaluate: %v\n", err)
		return 2
	}
	fmt.Fprintln(stdout, string(encoded))
	return 0
}

func runAnalyze(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("analyze", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "path to the Git repository")
	base := flags.String("base", "", "baseline Git revision")
	head := flags.String("head", "", "candidate Git revision")
	output := flags.String("output", "", "output directory")
	intent := flags.String("intent", "", "checked-in Change Intent path")
	policy := flags.String("policy", "", "repository policy path (defaults to shadow policy)")
	packDir := flags.String("pack-dir", "packs", "directory containing native language packs")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	service := planner.Planner{
		FMClient: managed.FMClient{
			Endpoint: os.Getenv("SIMPLETON_FM_ENDPOINT"), Token: os.Getenv("SIMPLETON_MANAGED_TOKEN"),
			ModelID: envOr("SIMPLETON_FM_MODEL", "managed"), PromptID: "semantic-risk-v1",
		},
		TelemetryClient: managed.TelemetryClient{
			Endpoint: os.Getenv("SIMPLETON_TELEMETRY_ENDPOINT"), Token: os.Getenv("SIMPLETON_MANAGED_TOKEN"),
		},
	}
	result, err := service.Analyze(ctx, planner.Request{
		Repository: *repo, Base: *base, Head: *head, Output: *output,
		IntentPath: *intent, PolicyPath: *policy, PackDir: *packDir, CoreVersion: version,
	})
	if err != nil {
		fmt.Fprintf(stderr, "simpleton analyze: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "run_id=%s evidence_digest=%s blockers=%d semantic_advisory=%t\n",
		result.Pack.RunID, result.EvidenceDigest, len(result.Pack.Blockers), result.Pack.SemanticAdvisory)
	return result.ExitCode
}

func runPack(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: simpleton pack go")
		return 2
	}
	switch args[0] {
	case "go":
		if err := packrpc.Serve(ctx, os.Stdin, os.Stdout, gopack.Handler{}); err != nil {
			fmt.Fprintf(stderr, "Go pack: %v\n", err)
			return 2
		}
		return 0
	default:
		fmt.Fprintf(stderr, "pack %q is not embedded\n", args[0])
		return 2
	}
}

func runLearn(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "record" {
		fmt.Fprintln(stderr, "usage: simpleton learn record --state <dir> --run-id <id> [outcome flags]")
		return 2
	}
	flags := flag.NewFlagSet("learn record", flag.ContinueOnError)
	flags.SetOutput(stderr)
	statePath := flags.String("state", "", "directory containing simpleton.db")
	runID := flags.String("run-id", "", "Evidence Pack run ID")
	action := flags.String("reviewer-action", "", "reviewer action")
	accepted := flags.String("accepted", "", "true or false")
	reverted := flags.String("reverted", "", "true or false")
	escaped := flags.String("escaped-regression", "", "true or false")
	cycles := flags.Int("review-cycles", -1, "review cycle count")
	minutes := flags.Int("review-minutes", -1, "review minutes")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *statePath == "" || *runID == "" {
		fmt.Fprintln(stderr, "--state and --run-id are required")
		return 2
	}
	outcome := store.Outcome{RunID: *runID, ReviewerAction: *action, RecordedAt: time.Now().UTC()}
	var err error
	if outcome.Accepted, err = optionalBool(*accepted); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if outcome.Reverted, err = optionalBool(*reverted); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if outcome.EscapedRegression, err = optionalBool(*escaped); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *cycles >= 0 {
		outcome.ReviewCycles = cycles
	}
	if *minutes >= 0 {
		outcome.ReviewMinutes = minutes
	}
	state, err := store.Open(*statePath)
	if err != nil {
		fmt.Fprintf(stderr, "open LEARN store: %v\n", err)
		return 2
	}
	defer state.Close()
	if err := state.RecordOutcome(ctx, outcome); err != nil {
		fmt.Fprintf(stderr, "record outcome: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "recorded outcome for run %s\n", *runID)
	return 0
}

func optionalBool(value string) (*bool, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return nil, errors.New("boolean outcome values must be true or false")
	}
	return &parsed, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `Simpleton produces semantic-regression evidence without safety verdicts.

Usage:
  simpleton analyze --repo <path> --base <sha> --head <sha> --output <dir> [--intent <path>] [--policy <path>]
  simpleton learn record --state <dir> --run-id <id> [outcome flags]
  simpleton evaluate score [six dimension flags]
  simpleton evaluate promotion --targets <n> --native-witnesses <n> --generated-witnesses <n>
  simpleton version`)
}
