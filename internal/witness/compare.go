package witness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"

	"github.com/haroldmartin/simpleton/internal/domain"
	"github.com/haroldmartin/simpleton/internal/numeric"
)

// ObservationPair is one baseline/candidate execution of an Observation Spec.
// LegalInput and DomainEvidence are supplied by reviewed preconditions and
// repository helpers; this package never guesses whether an input is legal.
type ObservationPair struct {
	Before         any
	After          any
	LegalInput     bool
	DomainEvidence []string
}

// ReplayAssessment is deliberately nonbinary. Stable only means the repeated
// observations and comparator outcome agreed; it does not imply equivalence.
type ReplayAssessment struct {
	Status  domain.MethodStatus
	Outcome domain.ObservationStatus
	Stable  bool
	Reason  string
	Replays int
	Before  any
	After   any
}

// Compare reports whether the approved comparator is violated.
func Compare(spec domain.ComparatorSpec, tolerances *domain.Tolerances, before, after any) (bool, string, error) {
	if tolerances != nil {
		if err := tolerances.Validate(); err != nil {
			return false, "", err
		}
	}
	if spec.Symbol != "" {
		return false, "", errors.New("repository comparator symbols must be executed by a language pack")
	}
	switch spec.BuiltIn {
	case "exact":
		equal, err := equalWithTolerance(before, after, tolerances)
		return !equal, "exact observations differ", err
	case "structural_json":
		left, err := asJSONValue(before)
		if err != nil {
			return false, "", fmt.Errorf("baseline observation: %w", err)
		}
		right, err := asJSONValue(after)
		if err != nil {
			return false, "", fmt.Errorf("candidate observation: %w", err)
		}
		equal, err := equalWithTolerance(left, right, tolerances)
		return !equal, "JSON structures differ", err
	case "unordered_collection":
		left, err := canonicalCollection(before)
		if err != nil {
			return false, "", fmt.Errorf("baseline observation: %w", err)
		}
		right, err := canonicalCollection(after)
		if err != nil {
			return false, "", fmt.Errorf("candidate observation: %w", err)
		}
		return !reflect.DeepEqual(left, right), "collection members differ", nil
	case "exception_type":
		left, err := exceptionField(before, "type")
		if err != nil {
			return false, "", err
		}
		right, err := exceptionField(after, "type")
		if err != nil {
			return false, "", err
		}
		return left != right, "exception types differ", nil
	case "exception":
		leftType, err := exceptionField(before, "type")
		if err != nil {
			return false, "", err
		}
		rightType, err := exceptionField(after, "type")
		if err != nil {
			return false, "", err
		}
		leftMessage, err := exceptionField(before, "message")
		if err != nil {
			return false, "", err
		}
		rightMessage, err := exceptionField(after, "message")
		if err != nil {
			return false, "", err
		}
		return leftType != rightType || leftMessage != rightMessage, "exception type or message differs", nil
	default:
		return false, "", fmt.Errorf("unsupported built-in comparator %q", spec.BuiltIn)
	}
}

// AssessReplays accepts only repeatable, legal-input comparisons. A mixture of
// outcomes or observations is flaky, and illegal inputs are inconclusive.
func AssessReplays(spec domain.ComparatorSpec, tolerances *domain.Tolerances, pairs []ObservationPair) (ReplayAssessment, error) {
	assessment := ReplayAssessment{Status: domain.StatusInconclusive, Outcome: domain.ObservationNotObserved, Replays: len(pairs)}
	if len(pairs) < 2 {
		assessment.Reason = "at least two replays are required"
		return assessment, nil
	}
	var referenceBefore, referenceAfter []byte
	var referenceViolated bool
	for index, pair := range pairs {
		if !pair.LegalInput || len(pair.DomainEvidence) == 0 {
			assessment.Reason = "reviewed preconditions did not establish a legal input"
			return assessment, nil
		}
		violated, _, err := Compare(spec, tolerances, pair.Before, pair.After)
		if err != nil {
			return assessment, err
		}
		before, err := canonicalJSON(pair.Before)
		if err != nil {
			return assessment, err
		}
		after, err := canonicalJSON(pair.After)
		if err != nil {
			return assessment, err
		}
		if index == 0 {
			referenceBefore, referenceAfter, referenceViolated = before, after, violated
			assessment.Before, assessment.After = pair.Before, pair.After
			continue
		}
		if violated != referenceViolated || !bytes.Equal(before, referenceBefore) || !bytes.Equal(after, referenceAfter) {
			assessment.Status = domain.StatusFlaky
			assessment.Reason = "replayed observations or comparator outcomes were not repeatable"
			return assessment, nil
		}
	}
	assessment.Status = domain.StatusRan
	assessment.Stable = true
	if referenceViolated {
		assessment.Outcome = domain.ObservationDivergenceConfirmed
		assessment.Reason = "approved comparator was repeatably violated"
	} else {
		assessment.Outcome = domain.ObservationNoDivergence
		assessment.Reason = "no approved-comparator violation was observed"
	}
	return assessment, nil
}

// Promote creates a Behavioral Witness only after stability, domain validity,
// contract relevance, approval-at-head, and replay provenance are established.
func Promote(divergence domain.ObservedDivergence, approvedAtHead bool, contractDigest string, domainEvidence []string, contractViolated bool) (domain.BehavioralWitness, error) {
	switch {
	case !approvedAtHead:
		return domain.BehavioralWitness{}, errors.New("current-head code-owner approval is required")
	case contractDigest == "":
		return domain.BehavioralWitness{}, errors.New("approved contract digest is required")
	case divergence.Status != domain.ObservationDivergenceConfirmed:
		return domain.BehavioralWitness{}, errors.New("no comparator-governed divergence was confirmed")
	case !divergence.Stable || divergence.ReplayCount < 2:
		return domain.BehavioralWitness{}, errors.New("divergence was not stably replayed")
	case divergence.ReplayCapsuleDigest == "":
		return domain.BehavioralWitness{}, errors.New("Replay Capsule is required")
	case divergence.EnvironmentDigest == "" || divergence.BaselineArtifactDigest == "" || divergence.CandidateArtifactDigest == "":
		return domain.BehavioralWitness{}, errors.New("baseline, candidate, and environment provenance are required")
	case len(domainEvidence) == 0:
		return domain.BehavioralWitness{}, errors.New("domain-validity evidence is required")
	case !contractViolated:
		return domain.BehavioralWitness{}, errors.New("divergence is not relevant to the approved contract")
	}
	return domain.BehavioralWitness{
		ID:                      domain.StableID("witness", divergence.ID, contractDigest),
		Divergence:              divergence,
		DomainValidityEvidence:  append([]string(nil), domainEvidence...),
		ContractRelevanceStatus: "violated",
		ApprovedContractDigest:  contractDigest,
	}, nil
}

func equalWithTolerance(left, right any, tolerances *domain.Tolerances) (bool, error) {
	leftNumber, leftIsNumber, err := number(left)
	if err != nil {
		return false, err
	}
	rightNumber, rightIsNumber, err := number(right)
	if err != nil {
		return false, err
	}
	if leftIsNumber || rightIsNumber {
		if !leftIsNumber || !rightIsNumber {
			return false, nil
		}
		if tolerances == nil || tolerances.Absolute == nil && tolerances.Relative == nil {
			return leftNumber.Cmp(rightNumber) == 0, nil
		}
		difference := new(big.Rat).Sub(leftNumber, rightNumber)
		difference.Abs(difference)
		if tolerances.Absolute != nil {
			absolute, ok := finiteRat(*tolerances.Absolute)
			if ok && difference.Cmp(absolute) <= 0 {
				return true, nil
			}
		}
		if tolerances.Relative != nil {
			relative, ok := finiteRat(*tolerances.Relative)
			leftMagnitude := new(big.Rat).Abs(leftNumber)
			rightMagnitude := new(big.Rat).Abs(rightNumber)
			scale := leftMagnitude
			if rightMagnitude.Cmp(leftMagnitude) > 0 {
				scale = rightMagnitude
			}
			if ok && difference.Cmp(new(big.Rat).Mul(relative, scale)) <= 0 {
				return true, nil
			}
		}
		return false, nil
	}
	leftMap, leftIsMap := left.(map[string]any)
	rightMap, rightIsMap := right.(map[string]any)
	if leftIsMap || rightIsMap {
		if !leftIsMap || !rightIsMap || len(leftMap) != len(rightMap) {
			return false, nil
		}
		for key, leftValue := range leftMap {
			rightValue, ok := rightMap[key]
			if !ok {
				return false, nil
			}
			equal, err := equalWithTolerance(leftValue, rightValue, tolerances)
			if err != nil || !equal {
				return equal, err
			}
		}
		return true, nil
	}
	leftSlice, leftIsSlice := left.([]any)
	rightSlice, rightIsSlice := right.([]any)
	if leftIsSlice || rightIsSlice {
		if !leftIsSlice || !rightIsSlice || len(leftSlice) != len(rightSlice) {
			return false, nil
		}
		for index := range leftSlice {
			equal, err := equalWithTolerance(leftSlice[index], rightSlice[index], tolerances)
			if err != nil || !equal {
				return equal, err
			}
		}
		return true, nil
	}
	return reflect.DeepEqual(left, right), nil
}

func number(value any) (*big.Rat, bool, error) {
	switch typed := value.(type) {
	case float64:
		number, ok := finiteRat(typed)
		return number, ok, nil
	case float32:
		number, ok := finiteRat(float64(typed))
		return number, ok, nil
	case int:
		return new(big.Rat).SetInt64(int64(typed)), true, nil
	case int8:
		return new(big.Rat).SetInt64(int64(typed)), true, nil
	case int16:
		return new(big.Rat).SetInt64(int64(typed)), true, nil
	case int32:
		return new(big.Rat).SetInt64(int64(typed)), true, nil
	case int64:
		return new(big.Rat).SetInt64(typed), true, nil
	case uint:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(uint64(typed))), true, nil
	case uint8:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(uint64(typed))), true, nil
	case uint16:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(uint64(typed))), true, nil
	case uint32:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(uint64(typed))), true, nil
	case uint64:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(typed)), true, nil
	case json.Number:
		n, err := numeric.ParseJSONNumber(typed)
		return n, err == nil, err
	default:
		return nil, false, nil
	}
}

func finiteRat(value float64) (*big.Rat, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, false
	}
	return new(big.Rat).SetFloat64(value), true
}

func asJSONValue(value any) (any, error) {
	if text, ok := value.(string); ok {
		dec := json.NewDecoder(bytes.NewBufferString(text))
		dec.UseNumber()
		var parsed any
		if err := dec.Decode(&parsed); err != nil {
			return nil, err
		}
		return parsed, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func canonicalCollection(value any) ([]string, error) {
	parsed, err := asJSONValue(value)
	if err != nil {
		return nil, err
	}
	items, ok := parsed.([]any)
	if !ok {
		return nil, errors.New("unordered_collection requires an array observation")
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		encoded, err := canonicalJSON(item)
		if err != nil {
			return nil, err
		}
		result = append(result, string(encoded))
	}
	sort.Strings(result)
	return result, nil
}

func canonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func exceptionField(value any, field string) (string, error) {
	parsed, err := asJSONValue(value)
	if err != nil {
		return "", err
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return "", errors.New("exception comparator requires an object observation")
	}
	result, ok := object[field].(string)
	if !ok {
		return "", fmt.Errorf("exception observation requires string field %q", field)
	}
	return result, nil
}
