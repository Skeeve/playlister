package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Track is a single audio file of an album.
type Track struct {
	Path, Rel, Title, Artist string // Rel: relative to the album folder
	TrackNo, DiscNo          int
}

// Album is a folder <root>/<artist>/<album>. Its tags are read lazily, the
// first time the album is selected in the UI.
type Album struct {
	ID       int    `json:"id"`
	ArtistID int    `json:"artist"`
	Name     string `json:"name"`
	Loaded   bool   `json:"loaded"`
	Year     int    `json:"year"` // 0 = unknown
	NTracks  int    `json:"tracks"`

	path   string
	tracks []Track
	names  map[string]int // frequency of artist names found in the tags
	sorts  map[string]int // frequency of artist sort names found in the tags
	mu     sync.Mutex     // serializes loading of this album
}

// Artist is a folder <root>/<artist>.
type Artist struct {
	ID       int    `json:"id"`
	Dir      string `json:"dir"`
	TagName  string `json:"tagName"`  // empty until an album has been loaded
	SortName string `json:"sortName"` // empty if the tags carry no sort name
	Albums   []int  `json:"albums"`
}

// library holds the scanned collection. gen changes with every full scan so
// that stale requests from the browser can be detected.
type library struct {
	mu      sync.Mutex
	gen     int
	root    string
	artists []*Artist
	albums  []*Album
}

type server struct {
	lib      library
	sem      chan struct{} // limits concurrent album loads
	cfgPath  string
	lastPing atomic.Int64 // UnixNano of the last browser heartbeat (see /api/ping)
}

// listDirs returns the visible subfolders of dir in natural order.
func listDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !hidden(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool { return natCompare(names[i], names[j]) < 0 })
	return names, nil
}

// scan lists artist and album folders only – no file is opened.
func (s *server) scan(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	dirs, err := listDirs(root)
	if err != nil {
		return err
	}
	var artists []*Artist
	var albums []*Album
	for _, d := range dirs {
		names, err := listDirs(filepath.Join(root, d))
		if err != nil || len(names) == 0 {
			continue
		}
		ar := &Artist{ID: len(artists), Dir: d}
		for _, n := range names {
			al := &Album{ID: len(albums), ArtistID: ar.ID, Name: n, path: filepath.Join(root, d, n)}
			albums = append(albums, al)
			ar.Albums = append(ar.Albums, al.ID)
		}
		artists = append(artists, ar)
	}

	s.lib.mu.Lock()
	s.lib.gen++
	s.lib.root, s.lib.artists, s.lib.albums = root, artists, albums
	s.lib.mu.Unlock()
	return nil
}

// loadAlbum reads the tags of all tracks of an album. Without force it does
// nothing if the album has been loaded before.
func (s *server) loadAlbum(al *Album, force bool) {
	al.mu.Lock()
	defer al.mu.Unlock()
	s.lib.mu.Lock()
	loaded := al.Loaded
	s.lib.mu.Unlock()
	if loaded && !force {
		return
	}

	s.sem <- struct{}{}
	defer func() { <-s.sem }()

	var tracks []Track
	names, sorts := map[string]int{}, map[string]int{}
	year := 0
	// recursive, so that CD1/CD2 subfolders are included
	_ = filepath.WalkDir(al.path, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != al.path && hidden(de.Name()) {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if de.IsDir() || !audioExt[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		ti := readTags(p)
		rel, _ := filepath.Rel(al.path, p)
		tracks = append(tracks, Track{Path: p, Rel: rel, Title: ti.title, Artist: ti.artist,
			TrackNo: ti.track, DiscNo: ti.disc})
		if year == 0 {
			year = ti.year
		}
		if n := firstNonEmpty(ti.albumArtist, ti.artist); n != "" {
			names[n]++
		}
		if n := firstNonEmpty(ti.albumArtistSort, ti.artistSort); n != "" {
			sorts[n]++
		}
		return nil
	})
	// disc, then track number (missing numbers last), then file name
	sort.SliceStable(tracks, func(i, j int) bool {
		a, b := tracks[i], tracks[j]
		if da, db := max(a.DiscNo, 1), max(b.DiscNo, 1); da != db {
			return da < db
		}
		ta, tb := a.TrackNo, b.TrackNo
		if ta == 0 {
			ta = 1 << 30
		}
		if tb == 0 {
			tb = 1 << 30
		}
		if ta != tb {
			return ta < tb
		}
		return natCompare(a.Rel, b.Rel) < 0
	})

	s.lib.mu.Lock()
	al.tracks, al.NTracks, al.Year = tracks, len(tracks), year
	al.names, al.sorts, al.Loaded = names, sorts, true
	s.lib.mu.Unlock()
}

// loadList loads several albums in parallel (bounded by s.sem).
func (s *server) loadList(list []*Album, force bool) {
	var wg sync.WaitGroup
	for _, al := range list {
		wg.Add(1)
		go func(al *Album) { defer wg.Done(); s.loadAlbum(al, force) }(al)
	}
	wg.Wait()
}

// loadAlbums loads the albums with the given IDs and returns them; nil if
// gen is stale.
func (s *server) loadAlbums(gen int, ids []int) []*Album {
	s.lib.mu.Lock()
	if gen != s.lib.gen {
		s.lib.mu.Unlock()
		return nil
	}
	var list []*Album
	for _, id := range ids {
		if id >= 0 && id < len(s.lib.albums) {
			list = append(list, s.lib.albums[id])
		}
	}
	s.lib.mu.Unlock()
	s.loadList(list, false)
	return list
}

// updateArtist derives name and sort name from all loaded albums of the
// artist. The caller must hold s.lib.mu.
func (s *server) updateArtist(ar *Artist) {
	names, sorts := map[string]int{}, map[string]int{}
	for _, id := range ar.Albums {
		al := s.lib.albums[id]
		if !al.Loaded {
			continue
		}
		for k, v := range al.names {
			names[k] += v
		}
		for k, v := range al.sorts {
			sorts[k] += v
		}
	}
	ar.TagName, ar.SortName = mostCommon(names), mostCommon(sorts)
}

// relistArtist re-reads the album folders of an artist: existing albums keep
// their IDs, new folders are appended, vanished ones are dropped from the
// artist. It returns the already loaded albums, which should be reloaded.
func (s *server) relistArtist(ar *Artist, dir string, names []string) []*Album {
	s.lib.mu.Lock()
	defer s.lib.mu.Unlock()
	existing := map[string]*Album{}
	for _, id := range ar.Albums {
		existing[s.lib.albums[id].Name] = s.lib.albums[id]
	}
	var ids []int
	var reload []*Album
	for _, n := range names {
		if al, ok := existing[n]; ok {
			ids = append(ids, al.ID)
			if al.Loaded {
				reload = append(reload, al)
			}
			continue
		}
		al := &Album{ID: len(s.lib.albums), ArtistID: ar.ID, Name: n, path: filepath.Join(dir, n)}
		s.lib.albums = append(s.lib.albums, al)
		ids = append(ids, al.ID)
	}
	ar.Albums = ids
	return reload
}
