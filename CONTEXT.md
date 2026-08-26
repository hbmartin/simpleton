# Simpleton

Simpleton is the domain of producing executable evidence about semantic regressions in software changes while remaining explicit about what was not established.

## Change Declaration

**Change Intent**:
A reviewed declaration of why a change exists, what it may alter, and the scope in which it is evaluated.
_Avoid_: Intent manifest, change contract

**Equivalence Contract**:
The behavior-preservation portion of a Change Intent that identifies observations that must remain stable across the baseline and candidate.
_Avoid_: Change Intent, safety contract

## Verification

**Verification Target**:
The semantic subject compared across a baseline and candidate, such as a symbol, module, package, API, or scenario.
_Avoid_: ChangedUnit, fuzz target

**Observation Boundary**:
The stable caller, public API, or scenario through which a Verification Target is exercised and observed.
_Avoid_: Test entrypoint

**Observation Spec**:
The contract definition of legal setup, inputs, observables, normalization, comparison, and tolerated differences at an Observation Boundary.
_Avoid_: Oracle, assertion script

**Probe**:
An executable experiment that exercises the same Observation Boundary against the baseline and candidate.
_Avoid_: Test, proof

**Observed Divergence**:
A repeatable difference between baseline and candidate observations. Replay establishes the difference, not that it is a defect.
_Avoid_: Counterexample, regression

**Behavioral Witness**:
An Observed Divergence whose input satisfies the approved domain preconditions and whose observation violates the approved Equivalence Contract.
_Avoid_: Counterexample, proof

**Semantic Regression**:
The contract violation demonstrated by a Behavioral Witness.
_Avoid_: Any difference, unsafe change

## Evidence

**Replay Capsule**:
The immutable fixtures, artifacts, environment description, and commands needed to reproduce an observation.
_Avoid_: Log bundle

**Evidence Pack**:
The complete per-change artifact containing applicability, method results, divergences, witnesses, provenance, and Replay Capsules.
_Avoid_: Verdict, safety score, audit pack

**Scoping Pack**:
A ranked collection of simplification opportunities and evidence used by humans and external agents to select bounded work.
_Avoid_: Technical-debt verdict

**Pack Capability**:
What a language pack can theoretically analyze or execute when its prerequisites are available.
_Avoid_: Eligibility

**Run Applicability**:
What can actually run for a specific repository, change, Verification Target, and environment.
_Avoid_: Pack Capability, Eligibility
