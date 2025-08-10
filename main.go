// Package main implements gphotosdl
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const (
	program       = "gphotosdl"
	gphotosURL    = "https://photos.google.com/"
	loginURL      = "https://photos.google.com/login"
	gphotoURLReal = "https://photos.google.com/photo/"
	gphotoURL     = "https://photos.google.com/lr/photo/" // redirects to gphotosURLReal which uses a different ID
	photoID       = "AF1QipNJVLe7d5mOh-b4CzFAob1UW-6EpFd0HnCBT3c6"
)

// Flags
var (
	debug   = flag.Bool("debug", false, "set to see debug messages")
	login   = flag.Bool("login", false, "set to launch a visible browser for login, then start the server")
	show    = flag.Bool("show", false, "set to show the browser (not headless)")
	addr    = flag.String("addr", "localhost:8282", "address for the web server")
	useJSON = flag.Bool("json", false, "log in JSON format")
)

// Global variables
var (
	configRoot    string      // top level config dir, typically ~/.config/gphotodl
	browserConfig string      // work directory for browser instance
	browserPath   string      // path to the browser binary
	downloadDir   string      // temporary directory for downloads
	browserPrefs  string      // JSON config for the browser
	version       = "DEV"     // set by goreleaser
	commit        = "NONE"    // set by goreleaser
	date          = "UNKNOWN" // set by goreleaser
	exitSignals   []os.Signal // Signals to exit on
)

// Remove the download directory and contents
func removeDownloadDirectory() {
	if downloadDir == "" {
		return
	}
	err := os.RemoveAll(downloadDir)
	if err == nil {
		slog.Debug("Removed download directory")
	} else {
		slog.Error("Failed to remove download directory", "err", err)
	}
}

// Set up the global variables from the flags
func config() (err error) {
	version := fmt.Sprintf("%s version %s, commit %s, built at %s", program, version, commit, date)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n%s\n", version)
	}
	flag.Parse()

	// Set up the logger
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	if *useJSON {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
		slog.SetDefault(logger)
	} else {
		slog.SetLogLoggerLevel(level) // set log level of Default Handler
	}
	slog.Debug(version)

	configRoot, err = os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("didn't find config directory: %w", err)
	}
	configRoot = filepath.Join(configRoot, program)
	browserConfig = filepath.Join(configRoot, "browser")
	err = os.MkdirAll(browserConfig, 0700)
	if err != nil {
		return fmt.Errorf("config directory creation: %w", err)
	}
	slog.Debug("Configured config", "config_root", configRoot, "browser_config", browserConfig)

	downloadDir, err = os.MkdirTemp("", program)
	if err != nil {
		log.Fatal(err)
	}
	slog.Debug("Created download directory", "download_directory", downloadDir)

	// Find the browser
	var ok bool
	browserPath, ok = launcher.LookPath()
	if !ok {
		return errors.New("browser not found")
	}
	slog.Debug("Found browser", "browser_path", browserPath)

	// Browser preferences
	pref := map[string]any{
		"download": map[string]any{
			"default_directory": "/tmp/gphotos", // FIXME
		},
	}
	prefJSON, err := json.Marshal(pref)
	if err != nil {
		return fmt.Errorf("failed to make preferences: %w", err)
	}
	browserPrefs = string(prefJSON)
	slog.Debug("made browser preferences", "prefs", browserPrefs)

	return nil
}

// logger makes an io.Writer from slog.Debug
type logger struct{}

// Write writes len(p) bytes from p to the underlying data stream.
func (logger) Write(p []byte) (n int, err error) {
	s := string(p)
	s = strings.TrimSpace(s)
	slog.Debug(s)
	return len(p), nil
}

// Println is called to log text
func (logger) Println(vs ...any) {
	s := fmt.Sprint(vs...)
	s = strings.TrimSpace(s)
	slog.Debug(s)
}

// Gphotos is a single page browser for Google Photos
type Gphotos struct {
	browser *rod.Browser
	page    *rod.Page
	mu      sync.Mutex // only one download at once is allowed
}

// New creates a new browser on the gphotos main page to check we are logged in
func New() (*Gphotos, error) {
	g := &Gphotos{}
	err := g.startBrowser()
	if err != nil {
		return nil, err
	}
	err = g.startServer()
	if err != nil {
		return nil, err
	}
	return g, nil
}

// start the browser off and check it is authenticated
func (g *Gphotos) startBrowser() error {
	// The -login flag implies showing the browser for the user to interact with.
	isHeadless := !*show && !*login

	// We use the default profile in our new data directory
	l := launcher.New().
		Bin(browserPath).
		Headless(isHeadless).
		UserDataDir(browserConfig).
		Preferences(browserPrefs).
		Set("disable-gpu").
		Set("disable-audio-output").
		Logger(logger{})

	url, err := l.Launch()
	if err != nil {
		return fmt.Errorf("browser launch: %w", err)
	}

	g.browser = rod.New().
		ControlURL(url).
		NoDefaultDevice().
		Trace(true).
		SlowMotion(100 * time.Millisecond).
		Logger(logger{})

	err = g.browser.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to browser: %w", err)
	}

	// If -login is passed, start at the login URL. Otherwise, go to photos.
	startURL := gphotosURL
	if *login {
		startURL = loginURL
	}

	g.page, err = g.browser.Page(proto.TargetCreateTarget{URL: startURL})
	if err != nil {
		return fmt.Errorf("couldn't open initial URL: %w", err)
	}

	err = g.page.WaitLoad()
	if err != nil {
		return fmt.Errorf("initial page load: %w", err)
	}

	authenticated := false
	if *login {
		slog.Info("A browser window is open. Please log in to your Google account. The server will start automatically once login is complete.")
	}

	// Loop indefinitely if login flag is set (waiting for user), otherwise try for 60 seconds.
	for try := 0; *login || try < 60; try++ {
		time.Sleep(1 * time.Second)
		info, err := g.page.Info()
		if err != nil {
			slog.Warn("Could not get page info, retrying...", "err", err)
			continue
		}
		slog.Debug("Current URL", "url", info.URL)

		// We are authenticated if we land on the main photos page.
		if info.URL == gphotosURL {
			authenticated = true
			slog.Info("Authentication successful.")
			break
		}

		// Show this message only on the first try in non-login mode.
		if try == 0 && !*login {
			slog.Info("Not authenticated. Trying for 60 seconds. If this fails, re-run with the -login flag.")
		}
	}

	if !authenticated {
		return errors.New("browser is not logged in - rerun with the -login flag")
	}
	return nil
}

// start the web server off
func (g *Gphotos) startServer() error {
	slog.Info("Starting web server", "address", *addr)
	http.HandleFunc("GET /", g.getRoot)
	http.HandleFunc("GET /id/{photoID}", g.getID)
	go func() {
		err := http.ListenAndServe(*addr, nil)
		if errors.Is(err, http.ErrServerClosed) {
			slog.Debug("web server closed")
		} else if err != nil {
			slog.Error("Error starting web server", "err", err)
			os.Exit(1)
		}
	}()
	return nil
}

// Serve the root page
func (g *Gphotos) getRoot(w http.ResponseWriter, r *http.Request) {
	slog.Info("got / request")
	_, _ = io.WriteString(w, `
<!DOCTYPE html>
<html lang="en">

<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>`+program+`</title>
</head>

<body>
  <h1>`+program+`</h1>
  <p>`+program+` is used to download full resolution Google Photos in combination with rclone.</p>
</body>

</html>`)
}

// Serve a photo ID
func (g *Gphotos) getID(w http.ResponseWriter, r *http.Request) {
	photoID := r.PathValue("photoID")
	slog.Info("got photo request", "id", photoID)
	path, err := g.Download(photoID)
	if err != nil {
		slog.Error("Download image failed", "id", photoID, "err", err)
		var h httpError
		if errors.As(err, &h) {
			w.WriteHeader(int(h))
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
		return
	}
	slog.Info("Downloaded photo", "id", photoID, "path", path)

	// Remove the file after it has been served
	defer func() {
		err := os.Remove(path)
		if err == nil {
			slog.Debug("Removed downloaded photo", "id", photoID, "path", path)
		} else {
			slog.Error("Failed to remove downloaded photo", "id", photoID, "path", path, "err", err)
		}
	}()

	http.ServeFile(w, r, path)
}

// httpError wraps an HTTP status code
type httpError int

func (h httpError) Error() string {
	return fmt.Sprintf("HTTP Error %d", h)
}

// Download a photo with the ID given
//
// Returns the path to the photo which should be deleted after use
func (g *Gphotos) Download(photoID string) (string, error) {
	// Can only download one picture at once
	g.mu.Lock()
	defer g.mu.Unlock()
	url := gphotoURL + photoID

	slog := slog.With("id", photoID)

	// Create a new blank browser tab
	slog.Debug("Open new tab")
	page, err := g.browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return "", fmt.Errorf("failed to open browser tab for photo %q: %w", photoID, err)
	}
	defer func() {
		err := page.Close()
		if err != nil {
			slog.Error("Error closing tab", "Error", err)
		}
	}()

	var netResponse *proto.NetworkResponseReceived

	// Check the correct network request is received
	waitNetwork := page.EachEvent(func(e *proto.NetworkResponseReceived) bool {
		slog.Debug("network response", "rxURL", e.Response.URL, "status", e.Response.Status)
		if strings.HasPrefix(e.Response.URL, gphotoURLReal) {
			netResponse = e
			return true
		} else if strings.HasPrefix(e.Response.URL, gphotoURL) {
			netResponse = e
			return true
		}
		return false
	})

	// Navigate to the photo URL
	slog.Debug("Navigate to photo URL")
	err = page.Navigate(url)
	if err != nil {
		return "", fmt.Errorf("failed to navigate to photo %q: %w", photoID, err)
	}
	slog.Debug("Wait for page to load")
	err = g.page.WaitLoad()
	if err != nil {
		return "", fmt.Errorf("gphoto page load: %w", err)
	}

	// Wait for the photos network request to happen
	slog.Debug("Wait for network response")
	waitNetwork()

	if netResponse == nil {
		return "", errors.New("did not receive the expected network response for the photo")
	}
	
	// Print request headers
	if netResponse.Response.Status != 200 {
		return "", fmt.Errorf("gphoto fetch failed: %w", httpError(netResponse.Response.Status))
	}

	// Download waiter
	wait := g.browser.WaitDownload(downloadDir)

	// A short delay can help ensure the page is ready for key presses.
	time.Sleep(time.Second)

	// Shift-D to download
	err = page.KeyActions().Press(input.ShiftLeft).Type('D').Do()
	if err != nil {
		return "", fmt.Errorf("failed to send download keypress: %w", err)
	}

	// Wait for download
	slog.Debug("Wait for download")
	info := wait()
	path := filepath.Join(downloadDir, info.GUID)

	// Check file
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("download failed, file not found: %w", err)
	}

	slog.Debug("Download successful", "size", fi.Size(), "path", path)

	return path, nil
}

// Close the browser
func (g *Gphotos) Close() {
	err := g.browser.Close()
	if err == nil {
		slog.Debug("Closed browser")
	} else {
		slog.Error("Failed to close browser", "err", err)
	}
}

func main() {
	err := config()
	if err != nil {
		slog.Error("Configuration failed", "err", err)
		os.Exit(2)
	}
	defer removeDownloadDirectory()

	// The logic for handling the -login flag is now integrated into New() and startBrowser().
	// The application will now proceed directly to creating a browser instance,
	// which will handle both the login flow and the authenticated headless flow.

	g, err := New()
	if err != nil {
		slog.Error("Failed to start application", "err", err)
		os.Exit(2)
	}
	defer g.Close()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, exitSignals...)

	// Wait for CTRL-C or SIGTERM
	slog.Info("Server is running. Press CTRL-C (or kill) to quit.")
	sig := <-quit
	slog.Info("Signal received - shutting down", "signal", sig)
}