# Simpleton

Simpleton is a local-first engineering evidence platform. It produces
semantic-regression evidence and replayable Behavioral Witnesses; it does not
transform code or emit an aggregate preservation verdict.

The repository now contains the executable foundation for:

- **WITNESS:** a Go CLI/planner, versioned JSON-RPC language-pack protocol,
  contract/policy validation, isolated deterministic checks, evidence reports,
  replay/comparator validation, content-addressed artifacts, and conservative CI
  blocking;
- **SENSE:** native-pack opportunities ranked into JSON plus local Markdown/HTML
  backlog reports; and
- **LEARN:** SQLite run metadata and outcome ingestion for review, acceptance,
  reverts, review cost, and escaped regressions.

## Build and verify

Requirements are Go 1.24, Node.js with the checked TypeScript package available,
Python 3, and Git. Schema verification additionally uses the locally available
`jsonschema` and PyYAML packages.

```sh
make verify
```

The TypeScript dependency is pinned by `packs/typescript/package-lock.json`.
Repository commands require Podman or Docker plus digest-pinned images; absence
of a rootless runtime is recorded as unsupported.

## Analyze

```sh
bin/simpleton analyze \
  --repo /path/to/repository \
  --base <commit> \
  --head <commit> \
  --output /path/to/run-output \
  [--intent /path/to/.simpleton/intents/<stable-id>.yaml] \
  [--policy /path/to/.simpleton/policy.yaml]
```

Without an intent, the run writes `proposed-change-intent.yaml` and all semantic
output is advisory. Outputs include `evidence-pack.json`, `scoping-pack.json`,
`backlog.md`, `backlog.html`, `simpleton.db`, and SHA-256-addressed objects.

Approved semantic use also requires a current-head code-owner attestation from a
supported Git-host adapter. See [.simpleton/README.md](.simpleton/README.md).

Record an outcome after review:

```sh
bin/simpleton learn record --state /path/to/run-output --run-id <id> \
  --reviewer-action accepted --accepted true --review-cycles 1
```

Apply the documented pilot rubric or harness-promotion rule with `simpleton
evaluate score` and `simpleton evaluate promotion`.

## Current capability boundary

Go, TypeScript, and Python packs perform native structural, caller/import,
effect, and available type analysis. Their executable probe capability remains
declared unsupported, so the planner records abstention and cannot fabricate a
Behavioral Witness. The Swift package is an explicit later-phase scaffold.
Pilot and partner results are intentionally absent until real repositories,
independent labels, and human scores exist; see [evaluation/README.md](evaluation/README.md).

Product terms live in [CONTEXT.md](CONTEXT.md), the roadmap in [PLAN.md](PLAN.md),
and the technical design in [IMPLEMENTATION.md](IMPLEMENTATION.md).
