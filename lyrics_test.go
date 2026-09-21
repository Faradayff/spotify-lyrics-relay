package main

import "testing"

func TestParseLRC_Basic(t *testing.T) {
	lrc := "[00:12.00]Line one\n[00:15.500]Line two\n[00:20]Line three"
	lines := parseLRC(lrc)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0].T != 12000 || lines[0].Text != "Line one" {
		t.Fatalf("line 0 wrong: %+v", lines[0])
	}
	if lines[1].T != 15500 || lines[1].Text != "Line two" {
		t.Fatalf("line 1 wrong: %+v", lines[1])
	}
	if lines[2].T != 20000 || lines[2].Text != "Line three" {
		t.Fatalf("line 2 wrong: %+v", lines[2])
	}
}

func TestParseLRC_MultiStamp(t *testing.T) {
	// A line with two timestamps expands into two synced lines.
	lrc := "[00:01.00][45:01.00]Repeat"
	lines := parseLRC(lrc)
	if len(lines) != 2 || lines[0].T != 1000 || lines[1].T != 45*60000+1000 {
		t.Fatalf("multi-stamp wrong: %+v", lines)
	}
}

func TestParseLRC_Sorting(t *testing.T) {
	lrc := "[00:30.00]later\n[00:10.00]earlier"
	lines := parseLRC(lrc)
	if lines[0].T >= lines[1].T {
		t.Fatalf("not sorted: %+v", lines)
	}
}

func TestFindLine(t *testing.T) {
	lines := parseLRC("[00:00.00]a\n[00:10.00]b\n[00:20.00]c")
	// at 5s -> closest is 0 (a)
	if i := findLine(lines, 5000); i != 0 {
		t.Fatalf("5s expected 0 got %d", i)
	}
	// at 12s -> closest is 10 (b)
	if i := findLine(lines, 12000); i != 1 {
		t.Fatalf("12s expected 1 got %d", i)
	}
	// at 25s -> clamps to last (c)
	if i := findLine(lines, 25000); i != 2 {
		t.Fatalf("25s expected 2 got %d", i)
	}
}

// TestDecodeLyrics_RealAPIShape reproduces the exact field names the
// LRCLIB /get endpoint returns (verified live: "syncedLyrics" /
// "plainLyrics"), so regressions to "synced" / "plain" fail here.
func TestDecodeLyrics_RealAPIShape(t *testing.T) {
	m := map[string]any{
		"id":           "3q2CVvw9bZu6aWn1PnS4jF",
		"trackName":    "Bohemian Rhapsody",
		"albumName":    "A Night at the Opera",
		"duration":     354,
		"plainLyrics":  "Is this the real life?",
		"syncedLyrics": "[00:00.15] Is this the real life?\n[00:07.13] Caught in a landslide",
		"lyricsfile":   "https://lrclib.net/api/lyrics/...",
	}
	out := decodeLyrics(m)
	if !out.Synced {
		t.Fatalf("expected synced lyrics, got %+v", out)
	}
	if out.LinesCount != 2 || len(out.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %+v", out.Lines)
	}
	if out.Lines[0].T != 150 || out.Lines[0].Text != "Is this the real life?" {
		t.Fatalf("line 0 wrong: %+v", out.Lines[0])
	}
	if out.Plain != "Is this the real life?" {
		t.Fatalf("plain wrong: %q", out.Plain)
	}

	// Plain-only response.
	plain := decodeLyrics(map[string]any{"plainLyrics": "hello"})
	if plain.Synced || plain.Plain != "hello" {
		t.Fatalf("plain-only wrong: %+v", plain)
	}

	// Empty / missing fields must yield empty data (no crash).
	if d := decodeLyrics(map[string]any{}); d.Synced || d.Plain != "" {
		t.Fatalf("empty wrong: %+v", d)
	}
	if d := decodeLyrics(nil); d.Synced || d.Plain != "" {
		t.Fatalf("nil wrong: %+v", d)
	}
}
