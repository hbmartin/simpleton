# Pilot evidence inputs

This directory defines data expectations for the public pilots; it contains no
fabricated outcomes.

- Pilot A requires 10 historical pull requests from each of two repositories
  per core language (60 total) and records changed-unit versus stable-boundary
  reach.
- Pilot B requires five independently established test-missed breaking changes
  and five preserving controls per language. A curated mutant is accepted only
  after independent non-equivalence evidence is linked.
- The lead records six 1–5 qualitative scores. `internal/evaluation` applies the
  continue/specialize/defer rule exactly.
- Generated-harness promotion records raw native and generated validated-witness
  counts for at least 30 targets in one language/change class. The calculation
  and its assumptions remain with the result.

Repository identities, pull requests, labels, and decisions should be committed
only when the corresponding pilot is actually run.
