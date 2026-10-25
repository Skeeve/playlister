package main

// Minimal reader for iTunes metadata in MP4 containers (.m4a, .m4p, .m4b).
// It walks moov/udta/meta/ilst and reads only the items the playlister needs,
// including the sort names (soar/soaa) that the generic tag library ignores.
// Audio data is never read, so this also works for protected .m4p files.

import (
	"encoding/binary"
	"io"
	"os"
	"strconv"
)

const maxIlst = 32 << 20 // guard against broken files (ilst may contain cover art)

type box struct {
	typ        string
	start, end int64 // payload, without header
}

// nextBox reads the box header at pos (within [pos, limit)).
func nextBox(r io.ReaderAt, pos, limit int64) (box, bool) {
	var h [16]byte
	if limit-pos < 8 {
		return box{}, false
	}
	if _, err := r.ReadAt(h[:8], pos); err != nil {
		return box{}, false
	}
	size := int64(binary.BigEndian.Uint32(h[:4]))
	typ := string(h[4:8])
	hdr := int64(8)
	switch size {
	case 0: // extends to the end of the enclosing container
		size = limit - pos
	case 1: // 64-bit size
		if _, err := r.ReadAt(h[8:16], pos+8); err != nil {
			return box{}, false
		}
		size = int64(binary.BigEndian.Uint64(h[8:16]))
		hdr = 16
	}
	if size < hdr || pos+size > limit {
		return box{}, false
	}
	return box{typ: typ, start: pos + hdr, end: pos + size}, true
}

// findPath descends e.g. moov → udta → meta → ilst.
func findPath(r io.ReaderAt, start, end int64, path []string) (box, bool) {
	for pos := start; pos < end; {
		b, ok := nextBox(r, pos, end)
		if !ok {
			return box{}, false
		}
		if b.typ == path[0] {
			if b.typ == "meta" {
				// ISO "meta" is a FullBox (4 bytes version/flags), the QuickTime
				// variant is not. Detect it by checking for 4 zero bytes.
				var v [4]byte
				if _, err := r.ReadAt(v[:], b.start); err == nil && binary.BigEndian.Uint32(v[:]) == 0 {
					b.start += 4
				}
			}
			if len(path) == 1 {
				return b, true
			}
			return findPath(r, b.start, b.end, path[1:])
		}
		pos = b.end
	}
	return box{}, false
}

func readMP4Tags(f *os.File) (ti tagInfo, ok bool) {
	st, err := f.Stat()
	if err != nil {
		return ti, false
	}
	ilst, found := findPath(f, 0, st.Size(), []string{"moov", "udta", "meta", "ilst"})
	if !found || ilst.end-ilst.start > maxIlst {
		return ti, false
	}
	buf := make([]byte, ilst.end-ilst.start)
	if _, err := f.ReadAt(buf, ilst.start); err != nil {
		return ti, false
	}
	r := bytesReaderAt(buf)
	n := int64(len(buf))

	for pos := int64(0); pos < n; {
		item, ok := nextBox(r, pos, n)
		if !ok {
			break
		}
		pos = item.end
		// "data" box inside the item: 4 bytes type, 4 bytes locale, then the value
		data, ok := findPath(r, item.start, item.end, []string{"data"})
		if !ok || data.end-data.start < 8 {
			continue
		}
		val := buf[data.start+8 : data.end]
		text := func() string { return cleanTag(string(val)) }
		pair := func() int { // trkn/disk: 2 reserved, 2 number, 2 total
			if len(val) >= 4 {
				return int(binary.BigEndian.Uint16(val[2:4]))
			}
			return 0
		}
		switch item.typ {
		case "\xa9nam":
			ti.title = text()
		case "\xa9ART":
			ti.artist = text()
		case "aART":
			ti.albumArtist = text()
		case "soar":
			ti.artistSort = text()
		case "soaa":
			ti.albumArtistSort = text()
		case "\xa9day":
			if m := yearRe.FindStringSubmatch(text()); m != nil {
				ti.year, _ = strconv.Atoi(m[1])
			}
		case "trkn":
			ti.track = pair()
		case "disk":
			ti.disc = pair()
		}
	}
	return ti, true
}

type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
