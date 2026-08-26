# Simpleton repository configuration

`policy.yaml` is the reviewed shadow-mode policy for this repository. Approved per-change manifests belong in `intents/<stable-id>.yaml`; generated proposals in a run output are advisory until moved there and approved by the required code owner for the current PR head.

The repository intentionally has no fabricated approved intent. Run `simpleton analyze` without `--intent` to generate a proposal for a real diff.

Trusted Git-host adapters bind approval to the current PR head using
`SIMPLETON_CONTRACT_APPROVED=true`, `SIMPLETON_APPROVED_HEAD`,
`SIMPLETON_REQUIRED_OWNER`, and `SIMPLETON_APPROVAL_SOURCE`. The source must be
`github_codeowners` or `gitlab_codeowners`; incomplete, ownerless, or stale
attestations leave semantic evidence advisory. The adapter also supplies
`SIMPLETON_REPOSITORY`.

The preferred interface is `SIMPLETON_APPROVAL_ATTESTATION`, pointing to a
strict current-version JSON/YAML file outside the repository worktree. It binds
provider, repository, pull request, head, required owners, approving owners, and
the adapter's CODEOWNERS check. Every required owner must have approved the
current head. Direct environment fields implement the same contract for adapters
that cannot emit a file.
