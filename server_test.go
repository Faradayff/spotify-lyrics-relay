package main

import (
	"testing"
)

// TestParseDelayMs tables the validation rules for the client delayMs
// query parameter: absent/invalid -> 0, negative -> 0, over the ceiling -> clamp.
func TestParseDelayMs(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"", 0}, // absent -> no delay
		{"0", 0},
		{"100", 100},
		{"1200", 1200},
		{"29999", 29999},
		{"30000", 30000},  // exactly the ceiling
		{"30001", 30000},  // clamped down
		{"100000", 30000}, // clamped down
		{"-5", 0},         // negative rejected -> 0
		{"abc", 0},        // non-numeric -> 0
		{"1.5", 0},        // not an integer -> 0
	}
	for _, c := range cases {
		if got := parseDelayMs(c.raw); got != c.want {
			t.Errorf("parseDelayMs(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// TestPlainStatusEchoesDelayMs: a client that sends delayMs gets it echoed
// back in the plain /status answer, which is the app's feature detection for
// a relay that applies the delay server-side.
func TestPlainStatusEchoesDelayMs(t *testing.T) {
	srv := newUnauthServer(t)
	_, body := statusRequest(srv, "delayMs=1500", nil)
	v, ok := body["delayMs"]
	if !ok {
		t.Fatal("plain /status must echo the delayMs the client sent")
	}
	i, ok := v.(float64)
	if !ok || i != 1500 {
		t.Fatalf("delayMs = %v, want 1500", v)
	}
}

// TestPlainStatusEchoesZeroDelayMs: even with no delay the field must be
// present (its presence is the contract, the value is the amount).
func TestPlainStatusEchoesZeroDelayMs(t *testing.T) {
	srv := newUnauthServer(t)
	_, body := statusRequest(srv, "delayMs=0", nil)
	v, ok := body["delayMs"]
	if !ok {
		t.Fatal("delayMs=0 must still be echoed")
	}
	i, ok := v.(float64)
	if !ok || i != 0 {
		t.Fatalf("delayMs = %v, want 0", v)
	}
}

// TestFindLineDelayShiftPicksEarlierLine proves the delay mapping: resolving
// at position - delay selects the line the listener is hearing now (earlier
// in the list), matching the app's local LyricIndex re-derivation semantics.
func TestFindLineDelayShiftPicksEarlierLine(t *testing.T) {
	lines := parseLRC("[00:00.00]a\n[00:10.00]b\n[00:20.00]c")
	// No delay, at 12 s the active line is "b" (index 1).
	if i := findLine(lines, 12000); i != 1 {
		t.Fatalf("12 s no-delay: expected 1 got %d", i)
	}
	// A 5 s car-audio delay shifts the heard position to 7 s: line "a".
	if i := findLine(lines, 12000-5000); i != 0 {
		t.Fatalf("12 s - 5 s delay: expected 0 got %d", i)
	}
	// A large delay that lands before the first line stays on the first line.
	if i := findLine(lines, 5000-30000); i != 0 {
		t.Fatalf("delay before first line: expected 0 got %d", i)
	}
}

// TestLyricPayloadPrevLine: prevLine is the text of the line above the active
// one and is the empty string (still present) at the first line.
func TestLyricPayloadPrevLine(t *testing.T) {
	lines := []lyricLine{{T: 0, Text: "a"}, {T: 1000, Text: "b"}, {T: 2000, Text: "c"}}
	data := &lyricsData{Synced: true, Lines: lines, LinesCount: 3}

	p := lyricPayload(data, 1)
	if v, _ := p["prevLine"].(string); v != "a" {
		t.Fatalf("prevLine = %v, want a", p["prevLine"])
	}

	p0 := lyricPayload(data, 0)
	v, ok := p0["prevLine"].(string)
	if !ok || v != "" {
		t.Fatalf("prevLine at first line should be empty string, got %v (present=%v)", v, ok)
	}
}
