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
	"encoding/json"
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

// openBrowser opens url in the given browser, or in the default browser if
// browser is empty or cannot be started.
//
//	macOS:   application name, e.g. "Firefox", "Google Chrome", "Safari"
//	Windows: program name or path, e.g. "firefox", "msedge"
//	Linux:   command, e.g. "firefox", "chromium"
func openBrowser(url, browser string) {
	var cmd *exec.Cmd
	switch {
	case runtime.GOOS == "darwin" && browser != "":
		cmd = exec.Command("open", "-a", browser, url)
	case runtime.GOOS == "darwin":
		cmd = exec.Command("open", url)
	case runtime.GOOS == "windows" && browser != "":
		cmd = exec.Command("cmd", "/c", "start", "", browser, url)
	case runtime.GOOS == "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case browser != "":
		cmd = exec.Command(browser, url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if browser == "" {
		if err := cmd.Start(); err != nil {
			log.Printf("could not open browser: %v", err)
		}
		return
	}
	// "open -a" and "start" return at once and report a missing browser
	// through their exit status; a Linux browser command keeps running, so
	// only its start can fail.
	var err error
	if runtime.GOOS == "linux" {
		err = cmd.Start()
	} else {
		err = cmd.Run()
	}
	if err != nil {
		log.Printf("could not start browser %q (%v), using the default browser", browser, err)
		openBrowser(url, "")
	}
}

// alreadyRunning reports whether a Playlister answers at addr.
func alreadyRunning(addr string) bool {
	req, _ := http.NewRequest("GET", "http://"+addr+"/api/info", nil)
	req.Header.Set("X-Playlister", "1")
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var info struct {
		Version string `json:"version"`
	}
	return resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&info) == nil && info.Version != ""
}

// savedBrowser reads the browser chosen in the settings dialog.
func savedBrowser(cfgPath string) string {
	var c struct {
		Browser string `json:"browser"`
	}
	if data, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c.Browser
}

func main() {
	const defaultListen = "127.0.0.1:8765"
	listen := flag.String("listen", defaultListen, "address:port of the web server")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser automatically")
	browser := flag.String("browser", "", "browser to open (default: as set in the UI, else the system default)")
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
		writeJSON(w, 200, map[string]any{"version": version, "os": runtime.GOOS})
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

	browserFor := func() string {
		if *browser != "" {
			return *browser
		}
		return savedBrowser(s.cfgPath)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil && *listen == defaultListen && alreadyRunning(defaultListen) {
		// e.g. the app was opened a second time: just show the running instance
		fmt.Println("Playlister is already running at http://" + defaultListen + "/")
		if !*noBrowser {
			openBrowser("http://"+defaultListen+"/", browserFor())
		}
		return
	}
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
		go openBrowser(url, browserFor())
	}
	if err := http.Serve(ln, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
