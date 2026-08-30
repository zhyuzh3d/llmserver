package pricing

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const decimalScale int64 = 1_000_000_000

// Decimal is a non-negative fixed-point currency value with nine fractional
// digits. It is intentionally not a float so settlements are reproducible.
type Decimal struct {
	nanos int64
}

func ParseDecimal(value string) (Decimal, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Decimal{}, errors.New("amount is empty")
	}
	if strings.HasPrefix(value, "-") {
		return Decimal{}, errors.New("negative amounts are not supported")
	}
	if strings.HasPrefix(value, "+") {
		value = strings.TrimPrefix(value, "+")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return Decimal{}, fmt.Errorf("invalid decimal %q", value)
	}
	wholeText := parts[0]
	if wholeText == "" {
		wholeText = "0"
	}
	whole, err := strconv.ParseInt(wholeText, 10, 64)
	if err != nil {
		return Decimal{}, fmt.Errorf("invalid decimal %q: %w", value, err)
	}
	fractionText := ""
	if len(parts) == 2 {
		fractionText = parts[1]
	}
	if len(fractionText) > 9 {
		return Decimal{}, fmt.Errorf("decimal %q has more than 9 fractional digits", value)
	}
	for _, r := range fractionText {
		if r < '0' || r > '9' {
			return Decimal{}, fmt.Errorf("invalid decimal %q", value)
		}
	}
	fractionText += strings.Repeat("0", 9-len(fractionText))
	fraction := int64(0)
	if fractionText != "" {
		fraction, err = strconv.ParseInt(fractionText, 10, 64)
		if err != nil {
			return Decimal{}, fmt.Errorf("invalid decimal %q: %w", value, err)
		}
	}
	if whole > (int64(^uint64(0)>>1)-fraction)/decimalScale {
		return Decimal{}, fmt.Errorf("decimal %q overflows", value)
	}
	return Decimal{nanos: whole*decimalScale + fraction}, nil
}

func DecimalFromNanos(nanos int64) (Decimal, error) {
	if nanos < 0 {
		return Decimal{}, errors.New("negative amounts are not supported")
	}
	return Decimal{nanos: nanos}, nil
}

func (d Decimal) Nanos() int64 { return d.nanos }

func (d Decimal) IsZero() bool { return d.nanos == 0 }

func (d Decimal) String() string {
	return fmt.Sprintf("%d.%09d", d.nanos/decimalScale, d.nanos%decimalScale)
}

func (d Decimal) Add(other Decimal) (Decimal, error) {
	if other.nanos > 0 && d.nanos > int64(^uint64(0)>>1)-other.nanos {
		return Decimal{}, errors.New("amount addition overflows")
	}
	return Decimal{nanos: d.nanos + other.nanos}, nil
}
