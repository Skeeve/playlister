// Playlister creates M3U playlists from a music collection organized as
//
//	<root>/<artist>/<album>/<tracks>
//
// It starts a small web server on 127.0.0.1 and opens the user interface in
// the default browser. Scanning only lists folders; the tags of an album are
// read when it is selected.
package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

// version is set at build time: -ldflags "-X main.version=v1.2.3"
var version = "dev"

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("could not open browser: %v", err)
	}
}

func main() {
	const defaultListen = "127.0.0.1:8765"
	listen := flag.String("listen", defaultListen, "address:port of the web server")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser automatically")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("playlister", version)
		return
	}

	cfgDir, err := os.UserConfigDir()
	if err != nil {
		cfgDir, _ = os.UserHomeDir()
	}
	s := &server{sem: make(chan struct{}, 4), cfgPath: filepath.Join(cfgDir, "playlister", "config.json")}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/scan", apiGuard(s.handleScan, "POST"))
	mux.HandleFunc("/api/load", apiGuard(s.handleLoad, "POST"))
	mux.HandleFunc("/api/rescan", apiGuard(s.handleRescan, "POST"))
	mux.HandleFunc("/api/playlist", apiGuard(s.handlePlaylist, "POST"))
	mux.HandleFunc("/api/dirs", apiGuard(s.handleDirs, "GET"))
	mux.HandleFunc("/api/config", apiGuard(s.handleConfig, "GET", "POST"))
	mux.HandleFunc("/api/info", apiGuard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"version": version})
	}, "GET"))
	mux.HandleFunc("/api/quit", apiGuard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
		go func() { time.Sleep(300 * time.Millisecond); os.Exit(0) }()
	}, "POST"))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})

	ln, err := net.Listen("tcp", *listen)
	if err != nil && *listen == defaultListen {
		ln, err = net.Listen("tcp", "127.0.0.1:0") // port taken – use a free one
	}
	if err != nil {
		log.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	host := addr.IP.String()
	if addr.IP.IsUnspecified() {
		host = "localhost"
	}
	url := fmt.Sprintf("http://%s/", net.JoinHostPort(host, strconv.Itoa(addr.Port)))
	fmt.Printf("Playlister %s running at %s  (quit: Ctrl+C or the button in the UI)\n", version, url)
	if !*noBrowser {
		openBrowser(url)
	}
	if err := http.Serve(ln, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
