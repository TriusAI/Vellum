package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/exec"

	"vellum/internal/api"
	"vellum/internal/config"
)

// cmdServe runs the local web UI + JSON API. Binds 127.0.0.1 by default —
// the UI has no authentication; it is meant for the person at the machine.
func cmdServe(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8090", "address to listen on")
	open := fs.Bool("open", false, "open the browser")
	fs.Parse(args)

	conn := mustOpen(cfg)
	server := api.New(cfg, conn)

	url := "http://" + *listen
	fmt.Printf("vellum web UI:  %s\n", url)
	fmt.Printf("JSON API:       %s/api/status  (docs: `vellum agent`)\n", url)
	if *open {
		exec.Command("xdg-open", url).Start() // best effort; Linux pack
	}
	log.Printf("listening on %s — Ctrl+C to stop", *listen)
	if err := http.ListenAndServe(*listen, server.Mux()); err != nil {
		log.Fatalf("serve: %s", err)
	}
}

// versionJSON is a tiny self-description used by scripts.
func versionJSONString() string {
	b, _ := json.Marshal(map[string]string{"version": versionString})
	return string(b)
}
