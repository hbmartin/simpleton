package numeric

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const (
	maxJSONNumberBytes       = 4096
	maxJSONExponentMagnitude = 10_000
)

var ErrJSONNumberTooLarge = errors.New("JSON number exceeds comparison resource limits")

// ParseJSONNumber converts a JSON number without allowing a short exponent to
// expand into an attacker-sized big integer in the core process.
func ParseJSONNumber(value json.Number) (*big.Rat, error) {
	text := string(value)
	if len(text) == 0 {
		return nil, errors.New("JSON number is empty")
	}
	if len(text) > maxJSONNumberBytes {
		return nil, ErrJSONNumberTooLarge
	}
	if !json.Valid([]byte(text)) {
		return nil, errors.New("invalid JSON number")
	}
	if exponentAt := strings.IndexAny(text, "eE"); exponentAt >= 0 {
		exponent, err := strconv.ParseInt(text[exponentAt+1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid JSON number exponent: %w", err)
		}
		if exponent < -maxJSONExponentMagnitude || exponent > maxJSONExponentMagnitude {
			return nil, ErrJSONNumberTooLarge
		}
	}
	parsed, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, errors.New("invalid JSON number")
	}
	return parsed, nil
}
