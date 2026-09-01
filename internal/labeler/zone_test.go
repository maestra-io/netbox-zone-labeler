package labeler

import (
	"strings"
	"testing"
)

func TestZoneFromRack(t *testing.T) {
	cases := map[string]string{
		"Rack 42":          "rack-42",
		"UPPER":            "upper",
		"already-good":     "already-good",
		"Multiple  Spaces": "multiple-spaces",
		" L130-B14 ":       "l130-b14",
		"tab\tseparated":   "tab-separated",
		"":                 "",
	}
	for in, want := range cases {
		if got := ZoneFromRack(in); got != want {
			t.Errorf("ZoneFromRack(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateZone(t *testing.T) {
	valid := []string{"rack-42", "l130-b14", "a", "a.b", "a_b", strings.Repeat("a", 63)}
	for _, z := range valid {
		if err := ValidateZone(z); err != nil {
			t.Errorf("ValidateZone(%q) = %v, want nil", z, err)
		}
	}
	invalid := []string{"", "-start-dash", "end-dash-", "has space", "rack#3", "a/b", "стойка-1", strings.Repeat("a", 64)}
	for _, z := range invalid {
		if err := ValidateZone(z); err == nil {
			t.Errorf("ValidateZone(%q) = nil, want error", z)
		}
	}
}
