package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dhowden/tag"
)

// Supported audio formats. .m4p is the extension of (formerly) protected
// iTunes purchases; tags are readable either way.
var audioExt = map[string]bool{".mp3": true, ".m4a": true, ".m4p": true, ".m4b": true}

// Extensions read with the built-in MP4 metadata reader (see mp4.go).
var mp4Ext = map[string]bool{".m4a": true, ".m4p": true, ".m4b": true, ".mp4": true}

var yearRe = regexp.MustCompile(`(?:^|\D)((?:1[89]|20)\d\d)(?:\D|$)`)

type tagInfo struct {
	title, artist, albumArtist  string
	artistSort, albumArtistSort string
	year, track, disc           int
}

// cleanTag returns the first value of a (possibly NUL-separated, ID3v2.4)
// multi-value string, trimmed.
func cleanTag(s string) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// readTags reads the metadata the playlister needs. Errors yield empty values.
func readTags(path string) (ti tagInfo) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	if mp4Ext[strings.ToLower(filepath.Ext(path))] {
		if ti, ok := readMP4Tags(f); ok {
			return ti
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return
		}
	}

	m, err := tag.ReadFrom(f)
	if err != nil {
		return
	}
	raw := m.Raw()
	rawStr := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := raw[k].(string); ok {
				if s = cleanTag(s); s != "" {
					return s
				}
			}
		}
		return ""
	}
	ti.title = cleanTag(m.Title())
	ti.artist = cleanTag(m.Artist())
	ti.albumArtist = cleanTag(m.AlbumArtist())
	ti.track, _ = m.Track()
	ti.disc, _ = m.Disc()
	// ID3v2.3/2.4: TSO2 = album artist sort order (iTunes), TSOP = artist sort order
	ti.albumArtistSort = rawStr("TSO2", "albumartistsort")
	ti.artistSort = rawStr("TSOP", "XSOP", "artistsort")
	// Year: TDRC (v2.4) / TYER (v2.3), falling back to the original release year
	ys := rawStr("TDRC", "TYER", "TYE", "TDOR", "TORY", "TOR", "date", "year")
	if mm := yearRe.FindStringSubmatch(ys); mm != nil {
		ti.year, _ = strconv.Atoi(mm[1])
	} else if y := m.Year(); y >= 1800 && y <= 2999 {
		ti.year = y
	}
	return
}

// hidden reports dot files, including macOS AppleDouble files ("._song.mp3")
// that appear on exFAT/FAT volumes and network shares.
func hidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

// natCompare compares case-insensitively with numbers in numeric order
// ("Track 2" < "Track 10").
func natCompare(a, b string) int {
	a, b = strings.ToLower(a), strings.ToLower(b)
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for a != "" && b != "" {
		if isDigit(a[0]) && isDigit(b[0]) {
			i, j := 0, 0
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na, nb := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
			if len(na) != len(nb) {
				return len(na) - len(nb)
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			a, b = a[i:], b[j:]
			continue
		}
		if a[0] != b[0] {
			return int(a[0]) - int(b[0])
		}
		a, b = a[1:], b[1:]
	}
	return len(a) - len(b)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// mostCommon returns the most frequent value (ties: alphabetically first).
func mostCommon(counts map[string]int) string {
	best, bestN := "", 0
	for s, n := range counts {
		if n > bestN || (n == bestN && s < best) {
			best, bestN = s, n
		}
	}
	return best
}
