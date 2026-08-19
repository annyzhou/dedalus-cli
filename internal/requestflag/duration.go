package requestflag

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"
)

// expires_in is a signed 32-bit integer on the wire.
const maxDurationSeconds int64 = 1<<31 - 1

const durationSecondsUsage = `CLI input accepts fixed duration units like 30s, 30m, 2h, 7d3h4s, or 1w3d, and raw seconds ("1800").`

// DurationSecondsFlag accepts human-readable durations while preserving the
// integer-seconds value required by the outgoing request.
type DurationSecondsFlag struct {
	*Flag[int64]
}

var _ cli.Flag = (*DurationSecondsFlag)(nil)

// NewDurationSecondsFlag lets a generated integer-seconds flag accept duration
// input without changing where the value appears in the outgoing request.
func NewDurationSecondsFlag(flag *Flag[int64]) *DurationSecondsFlag {
	return &DurationSecondsFlag{Flag: flag}
}

func (f *DurationSecondsFlag) PreParse() error {
	f.value = &durationSecondsValue{seconds: f.Default}
	f.applied = true

	if f.Default == 0 {
		return nil
	}
	if f.Default < 0 || f.Default > maxDurationSeconds {
		return fmt.Errorf("default duration must be between 1 and %d seconds", maxDurationSeconds)
	}
	if f.Validator == nil {
		return nil
	}
	return f.Validator(f.Default)
}

func (f *DurationSecondsFlag) PostParse() error {
	if f.hasBeenSet {
		return nil
	}

	value, source, found := f.Sources.LookupWithSource()
	if !found || value == "" {
		return nil
	}
	if err := f.Set(f.Name, value); err != nil {
		return fmt.Errorf(
			"could not parse %q as duration value from %s for flag %s: %s",
			value, source, f.Name, err,
		)
	}
	return nil
}

func (f *DurationSecondsFlag) Set(_ string, value string) error {
	if !f.applied {
		if err := f.PreParse(); err != nil {
			return err
		}
	}

	seconds, err := parseDurationSeconds(value)
	if err != nil {
		return err
	}
	if f.Validator != nil {
		if err := f.Validator(seconds); err != nil {
			return err
		}
	}

	f.count++
	f.value = &durationSecondsValue{raw: value, seconds: seconds}
	f.hasBeenSet = true
	return nil
}

func (f *DurationSecondsFlag) TypeName() string {
	return "duration"
}

func (f *DurationSecondsFlag) GetUsage() string {
	usage := strings.TrimSpace(f.Usage)
	if usage == "" {
		return durationSecondsUsage
	}
	return strings.TrimSuffix(usage, ".") + ". " + durationSecondsUsage
}

func (f *DurationSecondsFlag) String() string {
	return cli.FlagStringer(f)
}

type durationSecondsValue struct {
	raw     string
	seconds int64
}

func (v *durationSecondsValue) Set(value string) error {
	seconds, err := parseDurationSeconds(value)
	if err != nil {
		return err
	}
	v.raw = value
	v.seconds = seconds
	return nil
}

func (v *durationSecondsValue) String() string {
	if v.raw != "" {
		return v.raw
	}
	return strconv.FormatInt(v.seconds, 10)
}

func (v *durationSecondsValue) Get() any {
	return v.seconds
}

func parseDurationSeconds(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, fmt.Errorf("%q is not a valid duration", value)
	}

	if allDecimalDigits(value) {
		seconds, err := parseDurationQuantity(value)
		if err != nil {
			return 0, err
		}
		if seconds == 0 {
			return 0, fmt.Errorf("duration must be greater than zero")
		}
		return seconds, nil
	}

	var total int64
	for offset := 0; offset < len(value); {
		quantityStart := offset
		for offset < len(value) && value[offset] >= '0' && value[offset] <= '9' {
			offset++
		}
		if quantityStart == offset || offset == len(value) {
			return 0, fmt.Errorf("%q is not a valid duration", value)
		}

		quantity, err := parseDurationQuantity(value[quantityStart:offset])
		if err != nil {
			return 0, err
		}
		unit := value[offset]
		if !isASCIILetter(unit) {
			return 0, fmt.Errorf("%q is not a valid duration", value)
		}
		multiplier, err := durationUnitSeconds(unit)
		if err != nil {
			return 0, err
		}
		offset++

		if quantity > (maxDurationSeconds-total)/multiplier {
			return 0, fmt.Errorf("duration %q exceeds the maximum of %d seconds", value, maxDurationSeconds)
		}
		total += quantity * multiplier
	}

	if total == 0 {
		return 0, fmt.Errorf("duration must be greater than zero")
	}
	return total, nil
}

func isASCIILetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func allDecimalDigits(value string) bool {
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func parseDurationQuantity(value string) (int64, error) {
	quantity, err := strconv.ParseUint(value, 10, 64)
	if err != nil || quantity > uint64(maxDurationSeconds) {
		return 0, fmt.Errorf("duration %q exceeds the maximum of %d seconds", value, maxDurationSeconds)
	}
	return int64(quantity), nil
}

func durationUnitSeconds(unit byte) (int64, error) {
	switch unit {
	case 's':
		return 1, nil
	case 'm':
		return 60, nil
	case 'h':
		return 60 * 60, nil
	case 'd':
		return 24 * 60 * 60, nil
	case 'w':
		return 7 * 24 * 60 * 60, nil
	default:
		return 0, fmt.Errorf("unsupported unit %q; use s, m, h, d, or w", unit)
	}
}
