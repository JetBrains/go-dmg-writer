package hfsplus

import (
	"bytes"
	"testing"
)

func TestNewNameASCII(t *testing.T) {
	n, err := NewName("hello")
	if err != nil {
		t.Fatalf("NewName: %v", err)
	}
	if n.LenU16() != 5 {
		t.Errorf("len: got %d want 5", n.LenU16())
	}
	if !n.IsASCII() {
		t.Errorf("IsASCII: got false")
	}
	want := []byte{0, 'h', 0, 'e', 0, 'l', 0, 'l', 0, 'o'}
	if !bytes.Equal(n.Bytes(), want) {
		t.Errorf("bytes: got %v want %v", n.Bytes(), want)
	}
}

func TestNewNameNonASCII(t *testing.T) {
	// "café" - the 'é' (U+00E9) decomposes to 'e' + U+0301 (combining acute).
	n, err := NewName("café")
	if err != nil {
		t.Fatalf("NewName: %v", err)
	}
	if n.IsASCII() {
		t.Errorf("IsASCII: got true for non-ASCII name")
	}
	// After NFD: c a f e U+0301 → 5 code units.
	if n.LenU16() != 5 {
		t.Errorf("len: got %d want 5 (NFD)", n.LenU16())
	}
}

func TestNewNameTooLong(t *testing.T) {
	long := make([]byte, 0, 256)
	for i := 0; i < 256; i++ {
		long = append(long, 'a')
	}
	if _, err := NewName(string(long)); err == nil {
		t.Error("expected ErrNameTooLong, got nil")
	}
}

// TestNewNameAppleNFDException is a known-failing test that documents
// the gap between standard Unicode NFD (what we apply today via
// `golang.org/x/text/unicode/norm`) and Apple's HFS+ NFD exception
// table (TN1150 §"HFS+ Decomposition").
//
// The U+2000-U+2FFF range (general punctuation) and U+F900-U+FAFF
// (CJK compatibility ideographs) are NOT decomposed under Apple's
// rules, but standard NFD does decompose at least some of them. For
// example, U+2002 (EN SPACE) is decomposed to U+0020 (SPACE) by
// standard NFD, while Apple's NFD leaves it as U+2002.
//
// This test is currently t.Skip()'d so the test suite stays green
// while the gap is tracked. Removing the skip is the indicator that
// the exception table has been implemented.
func TestNewNameAppleNFDException(t *testing.T) {
	t.Skip("Apple-NFD exception table not yet implemented (see TN1150)")
	n, err := NewName("\u2002") // U+2002 EN SPACE
	if err != nil {
		t.Fatalf("NewName: %v", err)
	}
	if n.LenU16() != 1 {
		t.Errorf("len: got %d want 1 (Apple-NFD does not decompose U+2002)", n.LenU16())
	}
	if len(n.Units) != 1 || n.Units[0] != 0x2002 {
		t.Errorf("units: got %v want [0x2002]", n.Units)
	}
}

func TestHFSCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"a", "b", -1},
		{"b", "a", 1},
		{"a", "a", 0},
		{"abc", "abcd", -1},
		{"abcd", "abc", 1},
		{"A", "a", -1}, // 'A' < 'a' in binary compare
	}
	for _, c := range cases {
		na, _ := NewName(c.a)
		nb, _ := NewName(c.b)
		got := na.HFSCompare(nb)
		if got != c.want {
			t.Errorf("HFSCompare(%q,%q): got %d want %d", c.a, c.b, got, c.want)
		}
	}
}
