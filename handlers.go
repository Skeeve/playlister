package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Errors are sent as a code plus arguments; the browser translates them.
// "error" carries an English fallback text.
type apiError struct {
	status int
	code   string
	args   []any
}

var errMessages = map[string]string{
	"badRequest":       "Invalid request.",
	"noRoot":           "No music folder given.",
	"rootUnreadable":   "Cannot read folder: %v",
	"stale":            "The library has been rescanned.",
	"albumNotFound":    "Album not found.",
	"albumGone":        "The album folder no longer exists. Rescan the artist.",
	"artistNotFound":   "Artist not found.",
	"artistGone":       "The artist folder can no longer be read. Please rescan the whole library.",
	"noAlbums":         "No albums selected.",
	"targetMissing":    "Target folder does not exist: %v",
	"badFileName":      "Invalid file name.",
	"writeFailed":      "Could not write playlist: %v",
	"dirUnreadable":    "Cannot read folder: %v",
	"badConfig":        "Invalid settings.",
	"configFailed":     "Could not save settings: %v",
	"forbidden":        "Forbidden.",
	"methodNotAllowed": "Method not allowed.",
}

func fail(w http.ResponseWriter, status int, code string, args ...any) {
	if args == nil {
		args = []any{}
	}
	writeJSON(w, status, map[string]any{
		"error": fmt.Sprintf(errMessages[code], args...),
		"code":  code,
		"args":  args,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v)
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// libraryJSON must be called with s.lib.mu held.
func (s *server) libraryJSON() map[string]any {
	return map[string]any{"gen": s.lib.gen, "root": s.lib.root, "artists": s.lib.artists, "albums": s.lib.albums}
}

// POST /api/scan {root}
func (s *server) handleScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Root string `json:"root"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Root) == "" {
		fail(w, 400, "noRoot")
		return
	}
	if err := s.scan(strings.TrimSpace(req.Root)); err != nil {
		fail(w, 400, "rootUnreadable", err.Error())
		return
	}
	s.lib.mu.Lock()
	defer s.lib.mu.Unlock()
	writeJSON(w, 200, s.libraryJSON())
}

// POST /api/load {gen, ids}
func (s *server) handleLoad(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Gen int   `json:"gen"`
		IDs []int `json:"ids"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, 400, "badRequest")
		return
	}
	list := s.loadAlbums(req.Gen, req.IDs)
	if list == nil {
		fail(w, 409, "stale")
		return
	}
	s.lib.mu.Lock()
	defer s.lib.mu.Unlock()
	seen := map[int]bool{}
	artists := []*Artist{}
	for _, al := range list {
		if !seen[al.ArtistID] {
			seen[al.ArtistID] = true
			ar := s.lib.artists[al.ArtistID]
			s.updateArtist(ar)
			artists = append(artists, ar)
		}
	}
	writeJSON(w, 200, map[string]any{"albums": list, "artists": artists})
}

// POST /api/rescan {gen, album} or {gen, artist}
// Rereads one album's tags, or an artist folder (album list plus tags of the
// albums loaded so far), keeping the selection intact.
func (s *server) handleRescan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Gen    int  `json:"gen"`
		Artist *int `json:"artist"`
		Album  *int `json:"album"`
	}
	if err := readJSON(r, &req); err != nil || (req.Artist == nil) == (req.Album == nil) {
		fail(w, 400, "badRequest")
		return
	}
	s.lib.mu.Lock()
	if req.Gen != s.lib.gen {
		s.lib.mu.Unlock()
		fail(w, 409, "stale")
		return
	}
	var ar *Artist
	var reload []*Album
	if req.Album != nil {
		if *req.Album < 0 || *req.Album >= len(s.lib.albums) {
			s.lib.mu.Unlock()
			fail(w, 404, "albumNotFound")
			return
		}
		al := s.lib.albums[*req.Album]
		ar = s.lib.artists[al.ArtistID]
		reload = []*Album{al}
		s.lib.mu.Unlock()
		if !isDir(al.path) {
			fail(w, 404, "albumGone")
			return
		}
	} else {
		if *req.Artist < 0 || *req.Artist >= len(s.lib.artists) {
			s.lib.mu.Unlock()
			fail(w, 404, "artistNotFound")
			return
		}
		ar = s.lib.artists[*req.Artist]
		dir := filepath.Join(s.lib.root, ar.Dir)
		s.lib.mu.Unlock()
		names, err := listDirs(dir)
		if err != nil {
			fail(w, 404, "artistGone")
			return
		}
		reload = s.relistArtist(ar, dir, names)
	}

	s.loadList(reload, true)

	s.lib.mu.Lock()
	defer s.lib.mu.Unlock()
	s.updateArtist(ar)
	albums := reload
	if req.Artist != nil {
		albums = []*Album{}
		for _, id := range ar.Albums {
			albums = append(albums, s.lib.albums[id])
		}
	}
	writeJSON(w, 200, map[string]any{"artist": ar, "albums": albums})
}

// POST /api/playlist {gen, ids, extinf, dir, file, overwrite}
// ids are already in playlist order (sorting happens in the browser).
func (s *server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Gen       int    `json:"gen"`
		IDs       []int  `json:"ids"`
		ExtInf    bool   `json:"extinf"`
		Dir       string `json:"dir"`
		File      string `json:"file"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := readJSON(r, &req); err != nil || len(req.IDs) == 0 {
		fail(w, 400, "noAlbums")
		return
	}
	s.lib.mu.Lock()
	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		dir = s.lib.root
	}
	s.lib.mu.Unlock()
	if !isDir(dir) {
		fail(w, 400, "targetMissing", dir)
		return
	}
	file := strings.TrimSpace(req.File)
	if file == "" || file != filepath.Base(file) || strings.ContainsAny(file, `/\`) {
		fail(w, 400, "badFileName")
		return
	}
	if ext := strings.ToLower(filepath.Ext(file)); ext != ".m3u" && ext != ".m3u8" {
		file += ".m3u"
	}
	path := filepath.Join(dir, file)
	if _, err := os.Stat(path); err == nil && !req.Overwrite {
		writeJSON(w, 409, map[string]any{"exists": true, "path": path})
		return
	}

	list := s.loadAlbums(req.Gen, req.IDs) // in case anything is still missing
	if list == nil {
		fail(w, 409, "stale")
		return
	}

	base, _ := filepath.Abs(dir)
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	count, outside := 0, 0
	s.lib.mu.Lock()
	for _, al := range list {
		ar := s.lib.artists[al.ArtistID]
		if req.ExtInf && len(al.tracks) > 0 {
			// #EXTART/#EXTALB are unofficial extended M3U directives; players
			// that don't know them treat them as comments.
			fmt.Fprintf(&sb, "#EXTART:%s\n", firstNonEmpty(ar.TagName, ar.Dir))
			if al.Year > 0 {
				fmt.Fprintf(&sb, "#EXTALB:%s (%d)\n", al.Name, al.Year)
			} else {
				fmt.Fprintf(&sb, "#EXTALB:%s\n", al.Name)
			}
		}
		for _, t := range al.tracks {
			rel, err := filepath.Rel(base, t.Path)
			if err != nil { // Windows: different drive
				rel = t.Path
				outside++
			}
			if req.ExtInf {
				title := firstNonEmpty(t.Title, strings.TrimSuffix(filepath.Base(t.Path), filepath.Ext(t.Path)))
				fmt.Fprintf(&sb, "#EXTINF:-1,%s - %s\n", firstNonEmpty(t.Artist, ar.TagName, ar.Dir), title)
			}
			sb.WriteString(filepath.ToSlash(rel))
			sb.WriteByte('\n')
			count++
		}
	}
	s.lib.mu.Unlock()

	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		fail(w, 500, "writeFailed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"count": count, "outside": outside, "path": path})
}

// GET /api/dirs?path=… – folder listing for the folder picker in the browser.
func (s *server) handleDirs(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	home, _ := os.UserHomeDir()
	if p == "" {
		p = home
	}
	p, _ = filepath.Abs(p)
	entries, err := os.ReadDir(p)
	if err != nil {
		fail(w, 400, "dirUnreadable", err.Error())
		return
	}
	type entry struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Kind string `json:"kind,omitempty"` // "home" for the user's home folder
	}
	dirs := []entry{}
	for _, e := range entries {
		if hidden(e.Name()) {
			continue
		}
		full := filepath.Join(p, e.Name())
		if e.IsDir() || (e.Type()&fs.ModeSymlink != 0 && isDir(full)) {
			dirs = append(dirs, entry{Name: e.Name(), Path: full})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return natCompare(dirs[i].Name, dirs[j].Name) < 0 })
	places := []entry{}
	if home != "" {
		places = append(places, entry{Name: filepath.Base(home), Path: home, Kind: "home"})
	}
	if runtime.GOOS != "windows" {
		for _, c := range []string{"/Volumes", "/media", "/mnt"} {
			if isDir(c) {
				places = append(places, entry{Name: c, Path: c})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"path": p, "parent": filepath.Dir(p), "dirs": dirs, "places": places})
}

// GET/POST /api/config – settings are stored as an opaque JSON blob.
func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		data, err := os.ReadFile(s.cfgPath)
		if err != nil || !json.Valid(data) {
			data = []byte("{}")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(data)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || !json.Valid(data) {
		fail(w, 400, "badConfig")
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.cfgPath), 0o755)
	if err := os.WriteFile(s.cfgPath, data, 0o644); err != nil {
		fail(w, 500, "configFailed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// apiGuard protects against other web pages posting to localhost: the custom
// header forces a CORS preflight, which this server never answers.
func apiGuard(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Playlister") != "1" {
			fail(w, 403, "forbidden")
			return
		}
		for _, m := range methods {
			if r.Method == m {
				next(w, r)
				return
			}
		}
		fail(w, 405, "methodNotAllowed")
	}
}
