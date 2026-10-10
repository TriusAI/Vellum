package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"vellum/internal/config"
)

// cmdOpen opens the running web UI in the browser. It never starts a server:
// if nothing answers /api/status at the address, it says so and exits.
func cmdOpen(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8090", "address of the running `vellum serve`")
	rawURL := fs.String("url", "", "full base URL to open (overrides --listen)")
	fs.Parse(args)

	base := *rawURL
	if base == "" {
		base = "http://" + *listen
	}
	base = strings.TrimRight(base, "/")

	if !serverUp(base) {
		log.Fatalf("there is no server running at %s", base)
	}
	if err := openURL(base); err != nil {
		log.Fatalf("open: %s", err)
	}
	fmt.Printf("opened %s\n", base)
}

// serverUp reports whether a vellum server answers /api/status at base.
func serverUp(base string) bool {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(base + "/api/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// openURL launches url in the user's browser (best effort). $BROWSER wins,
// then the usual desktop openers.
func openURL(url string) error {
	try := func(name string) bool {
		p, err := exec.LookPath(name)
		if err != nil {
			return false
		}
		cmd := exec.Command(p, url)
		if err := cmd.Start(); err != nil {
			return false
		}
		go cmd.Wait() // reap; never block the caller on the browser
		return true
	}
	if b := os.Getenv("BROWSER"); b != "" && try(b) {
		return nil
	}
	for _, name := range []string{"xdg-open", "open", "sensible-browser", "x-www-browser"} {
		if try(name) {
			return nil
		}
	}
	return fmt.Errorf("no browser opener found (set $BROWSER or install xdg-open)")
}
