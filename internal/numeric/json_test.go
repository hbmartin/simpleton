package numeric

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseJSONNumberBoundsExpansion(t *testing.T) {
	if _, err := ParseJSONNumber(json.Number("1e10001")); !errors.Is(err, ErrJSONNumberTooLarge) {
		t.Fatalf("large exponent was not rejected: %v", err)
	}
	if _, err := ParseJSONNumber(json.Number(strings.Repeat("9", maxJSONNumberBytes+1))); !errors.Is(err, ErrJSONNumberTooLarge) {
		t.Fatalf("large mantissa was not rejected: %v", err)
	}
}

func TestParseJSONNumberPreservesOrdinaryPrecision(t *testing.T) {
	value, err := ParseJSONNumber(json.Number("9007199254740993.125"))
	if err != nil || value.RatString() != "72057594037927945/8" {
		t.Fatalf("ordinary precise number was not preserved: value=%v err=%v", value, err)
	}
}
