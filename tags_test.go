package main

import (
	"sort"
	"testing"
)

func TestNatCompare(t *testing.T) {
	in := []string{"Track 10", "track 2", "Track 1", "Track 02b", "Album", "album 9"}
	want := []string{"Album", "album 9", "Track 1", "track 2", "Track 02b", "Track 10"}
	sort.SliceStable(in, func(i, j int) bool { return natCompare(in[i], in[j]) < 0 })
	for i := range want {
		if in[i] != want[i] {
			t.Fatalf("got %q, want %q", in, want)
		}
	}
}

func TestYearRe(t *testing.T) {
	cases := map[string]string{
		"1987-07-01T07:00:00Z": "1987",
		"2004":                 "2004",
		"12345":                "",
		"no year":              "",
	}
	for in, want := range cases {
		got := ""
		if m := yearRe.FindStringSubmatch(in); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestCleanTag(t *testing.T) {
	if got := cleanTag(" Pat Metheny\x00Lyle Mays "); got != "Pat Metheny" {
		t.Errorf("got %q", got)
	}
}
