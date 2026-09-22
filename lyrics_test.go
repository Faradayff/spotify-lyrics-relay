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

func TestTrackFromState_ItemAndTrack(t *testing.T) {
	want := map[string]any{"id": "4uLU64mcE15tl0lwSjmOqz", "name": "Bohemian Rhapsody"}

	// Modern responses carry the track under "item".
	latest := map[string]any{"item": want, "is_playing": true, "progress_ms": float64(42000)}
	if got := trackFromState(latest); got["id"] != "4uLU64mcE15tl0lwSjmOqz" {
		t.Fatalf("item not picked: %+v", got)
	}

	// Older snapshots used "track"; fallback must still work.
	legacy := map[string]any{"track": want, "is_playing": true}
	if got := trackFromState(legacy); got["id"] != "4uLU64mcE15tl0lwSjmOqz" {
		t.Fatalf("track fallback failed: %+v", got)
	}

	// Nothing playing, item explicitly null.
	if got := trackFromState(map[string]any{"item": nil, "progress_ms": float64(0)}); got != nil {
		t.Fatalf("expected nil track, got: %+v", got)
	}
	if got := trackFromState(nil); got != nil {
		t.Fatalf("nil state must yield nil track")
	}
}

func TestLyricPayload(t *testing.T) {
	lines := []lyricLine{
		{T: 40000, Text: "one"}, {T: 45000, Text: "two"}, {T: 50000, Text: "three"},
		{T: 55000, Text: "four"}, {T: 60000, Text: "five"}, {T: 65000, Text: "six"},
	}
	synced := &lyricsData{Synced: true, Lines: lines, LinesCount: 6}

	// Synced data: active line + next three + full list.
	p := lyricPayload(synced, 2)
	if p["line"] != 2 || p["lineText"] != "three" {
		t.Fatalf("active line wrong: %+v", p)
	}
	all, ok := p["lines"].([]lyricLine)
	if !ok || len(all) != 6 || all[5].Text != "six" {
		t.Fatalf("full lines missing: %+v", p)
	}
	next, ok := p["nextLines"].([]lyricLine)
	if !ok || len(next) != 3 || next[0].Text != "four" || next[2].Text != "six" {
		t.Fatalf("nextLines wrong (want four/five/six): %+v", p)
	}

	// At the last line: window must be empty, not padded or panicking.
	last := lyricPayload(synced, 5)
	next, _ = last["nextLines"].([]lyricLine)
	if len(next) != 0 {
		t.Fatalf("expected empty nextLines at end, got: %+v", next)
	}

	// Out-of-range index: nothing leaked, no crash.
	if p := lyricPayload(synced, 99); len(p) != 0 {
		t.Fatalf("out-of-range idx should yield empty payload: %+v", p)
	}

	// Unsynced: only the plain text is exposed.
	plain := &lyricsData{Synced: false, Plain: "full lyrics, no stamps"}
	p2 := lyricPayload(plain, -1)
	if p2["plain"] != "full lyrics, no stamps" {
		t.Fatalf("plain missing: %+v", p2)
	}
	if _, ok := p2["line"]; ok {
		t.Fatalf("no line fields expected for plain-only lyrics")
	}

	// No data at all: empty payload.
	if p := lyricPayload(nil, -1); len(p) != 0 {
		t.Fatalf("nil data should yield empty payload: %+v", p)
	}
	if p := lyricPayload(&lyricsData{}, -1); len(p) != 0 {
		t.Fatalf("empty data should yield empty payload: %+v", p)
	}
}

func TestLyricsStatus(t *testing.T) {
	synced := &lyricsData{Synced: true, Lines: []lyricLine{{T: 0, Text: "a"}}, LinesCount: 1}
	plain := &lyricsData{Synced: false, Plain: "hello"}

	if got := lyricsStatus(synced, 0); got != "synced" {
		t.Fatalf("want synced, got %q", got)
	}
	// Synced data wins even when plain text also exists for the same track.
	if got := lyricsStatus(&lyricsData{Synced: true, Lines: []lyricLine{{T: 0, Text: "a"}}, Plain: "hello"}, 0); got != "synced" {
		t.Fatalf("want synced (plain present), got %q", got)
	}
	if got := lyricsStatus(plain, -1); got != "plain" {
		t.Fatalf("want plain, got %q", got)
	}
	if got := lyricsStatus(nil, -1); got != "none" {
		t.Fatalf("want none, got %q", got)
	}
	if got := lyricsStatus(&lyricsData{}, -1); got != "none" {
		t.Fatalf("want none for empty data, got %q", got)
	}
}
