package manifest

import (
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	ok := []struct {
		in   string
		want Size
	}{
		{"512M", 512 * Megabyte},
		{"20G", 20 * Gigabyte},
		{"1T", Terabyte},
		{"256K", 256 * Kilobyte},
		{"1GB", Gigabyte},  // the common spelling
		{"1GiB", Gigabyte}, // the pedantic spelling
		{"1g", Gigabyte},   // case does not matter
		{"1.5G", Size(1.5 * float64(Gigabyte))},
		{" 512M ", 512 * Megabyte},
	}
	for _, tc := range ok {
		got, err := ParseSize(tc.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A bare number is the mistake this type exists to prevent, so the error has to
// say what to write instead.
func TestParseSizeBareNumberExplainsItself(t *testing.T) {
	_, err := ParseSize("512")
	if err == nil {
		t.Fatal("expected an error for a bare number")
	}
	for _, want := range []string{"512M", "512G", "no unit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestParseSizeErrors(t *testing.T) {
	for _, in := range []string{"", "  ", "G", "abc", "-5G", "5X", "M5"} {
		if got, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", in, got)
		}
	}
}

func TestSizeRoundTrip(t *testing.T) {
	// A size read from a manifest must render back to the text it was written
	// as, or re-serializing a manifest would look like an edit.
	for _, in := range []string{"512M", "20G", "1T", "256K"} {
		s, err := ParseSize(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.String(); got != in {
			t.Errorf("%q round-tripped to %q", in, got)
		}
	}
}

func TestSizeMegabytes(t *testing.T) {
	s, _ := ParseSize("2G")
	if got := s.Megabytes(); got != 2048 {
		t.Errorf("2G in MB = %d, want 2048", got)
	}
}
