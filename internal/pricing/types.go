package pricing

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const usdPerMillionScale int64 = 1_000_000

var (
	ErrPersistenceDisabled = errors.New("pricing persistence is disabled")
	ErrRevisionConflict    = errors.New("pricing revision conflict")
	ErrModelNotFound       = errors.New("pricing model not found")
	ErrInvalidModel        = errors.New("invalid pricing model")
)

// USDPerMillion stores a USD-per-million-token price as integer micro-USD.
type USDPerMillion int64

// Micros returns the fixed-point price value.
func (p USDPerMillion) Micros() int64 { return int64(p) }

// UnmarshalJSON accepts a non-negative JSON decimal with at most six fractional digits.
func (p *USDPerMillion) UnmarshalJSON(data []byte) error {
	if p == nil {
		return errors.New("pricing value target is nil")
	}
	text := strings.TrimSpace(string(data))
	if text == "" || strings.ContainsAny(text, `"eE+-`) {
		return errors.New("price must be a non-negative decimal number")
	}
	parts := strings.Split(text, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
		return errors.New("price must be a non-negative decimal number")
	}
	if !decimalDigits(parts[0]) || (len(parts) == 2 && !decimalDigits(parts[1])) {
		return errors.New("price must be a non-negative decimal number")
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > 6 {
		return errors.New("price supports at most six decimal places")
	}
	whole, errWhole := strconv.ParseInt(parts[0], 10, 64)
	if errWhole != nil || whole > math.MaxInt64/usdPerMillionScale {
		return errors.New("price is out of range")
	}
	fraction += strings.Repeat("0", 6-len(fraction))
	fractionValue := int64(0)
	if fraction != "" {
		var errFraction error
		fractionValue, errFraction = strconv.ParseInt(fraction, 10, 64)
		if errFraction != nil {
			return errors.New("price is out of range")
		}
	}
	value := whole*usdPerMillionScale + fractionValue
	if value < 0 {
		return errors.New("price is out of range")
	}
	*p = USDPerMillion(value)
	return nil
}

// MarshalJSON emits an exact JSON decimal without exponent notation.
func (p USDPerMillion) MarshalJSON() ([]byte, error) {
	if p < 0 {
		return nil, errors.New("price cannot be negative")
	}
	whole := int64(p) / usdPerMillionScale
	fraction := int64(p) % usdPerMillionScale
	if fraction == 0 {
		return []byte(strconv.FormatInt(whole, 10)), nil
	}
	text := fmt.Sprintf("%d.%06d", whole, fraction)
	return []byte(strings.TrimRight(text, "0")), nil
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// BasePrices is the required USD-per-million-token price for each canonical token bucket.
type BasePrices struct {
	Input      USDPerMillion `json:"input"`
	Output     USDPerMillion `json:"output"`
	CacheRead  USDPerMillion `json:"cache_read"`
	CacheWrite USDPerMillion `json:"cache_write"`
}

// ModelConfig defines one provider/model base-price entry.
type ModelConfig struct {
	Provider   string     `json:"provider"`
	Model      string     `json:"model"`
	BasePrices BasePrices `json:"base_prices"`
}

// State is one immutable pricing catalog revision.
type State struct {
	Revision int64         `json:"revision"`
	Models   []ModelConfig `json:"models"`
}

// Subject contains the normalized dimensions needed for base-price resolution.
type Subject struct {
	Provider        string
	Model           string
	Alias           string
	ResponseModel   string
	UncachedInput   int64
	CacheReadInput  int64
	CacheWriteInput int64
	Output          int64
}

// Quote is a fixed-point catalog estimate for one request.
type Quote struct {
	Available bool
	USDMicros int64
	Revision  int64
	Model     string
}
