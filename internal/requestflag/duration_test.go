package requestflag

import (
	"strings"
	"testing"
)

func TestParseDurationSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  int64
	}{
		{name: "seconds", input: "1s", want: 1},
		{name: "minutes", input: "1m", want: 60},
		{name: "hours", input: "1h", want: 3_600},
		{name: "days", input: "1d", want: 86_400},
		{name: "weeks", input: "1w", want: 604_800},
		{name: "compound hours", input: "1h30m", want: 5_400},
		{name: "compound days", input: "2d12h", want: 216_000},
		{name: "all units", input: "1w2d3h4m5s", want: 788_645},
		{name: "trimmed uppercase units", input: " 2H ", want: 7_200},
		{name: "bare seconds", input: "3600", want: 3_600},
		{name: "wire maximum", input: "2147483647", want: 2_147_483_647},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseDurationSeconds(test.input)
			if err != nil {
				t.Fatalf("parseDurationSeconds(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Errorf("parseDurationSeconds(%q) = %d, want %d", test.input, got, test.want)
			}
		})
	}
}

func TestParseDurationSecondsRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{name: "empty", input: "", wantErr: "valid duration"},
		{name: "zero seconds", input: "0", wantErr: "greater than zero"},
		{name: "zero duration", input: "0s", wantErr: "greater than zero"},
		{name: "negative seconds", input: "-1", wantErr: "valid duration"},
		{name: "negative duration", input: "-1s", wantErr: "valid duration"},
		{name: "unknown unit", input: "1x", wantErr: "unsupported unit"},
		{name: "missing quantity", input: "h", wantErr: "valid duration"},
		{name: "missing final unit", input: "1h30", wantErr: "valid duration"},
		{name: "fractional quantity", input: "1.5h", wantErr: "valid duration"},
		{name: "embedded whitespace", input: "1h 30m", wantErr: "valid duration"},
		{name: "never", input: "never", wantErr: "valid duration"},
		{name: "wire overflow", input: "2147483648", wantErr: "exceeds"},
		{name: "numeric overflow", input: "999999999999999999999999", wantErr: "exceeds"},
		{name: "compound overflow", input: "999999999999999999999999w", wantErr: "exceeds"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseDurationSeconds(test.input)
			if err == nil {
				t.Fatalf("parseDurationSeconds(%q) succeeded", test.input)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("parseDurationSeconds(%q) error = %q, want substring %q", test.input, err, test.wantErr)
			}
		})
	}
}
