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
	// "café" — the 'é' (U+00E9) decomposes to 'e' + U+0301 (combining acute).
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

func TestHFSCompare(t *testing.T) {
	cases := []struct{ a, b string; want int }{
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
