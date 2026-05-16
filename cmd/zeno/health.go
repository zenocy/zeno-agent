package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/zenocy/zeno-v2/internal/config"
)

// runHealth probes /api/health on the local Zeno daemon. Designed for use
// as a container HEALTHCHECK — exits 0 when the daemon is up and reports
// db_ok=true, exits 1 otherwise. LLM reachability is *not* required (LLM
// can be down without Zeno being unhealthy; that mirrors the handler's
// own ok-policy in internal/http/api/health.go).
func runHealth(args []string) {
	os.Exit(healthMain(args, os.Stderr))
}

func healthMain(args []string, errOut io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(errOut)
	cfgPath := fs.String("config", "config.yaml", "path to config.yaml")
	addr := fs.String("addr", "", "override health URL (default derived from config bind:port)")
	timeoutSec := fs.Int("timeout", 5, "request timeout in seconds")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	url, token, err := healthURL(*cfgPath, *addr)
	if err != nil {
		fmt.Fprintf(errOut, "health: %v\n", err)
		return 1
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(errOut, "health: build request: %v\n", err)
		return 1
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: time.Duration(*timeoutSec) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(errOut, "health: %s unreachable: %v\n", url, err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(errOut, "health: %s returned %d\n", url, resp.StatusCode)
		return 1
	}

	var body struct {
		OK   bool `json:"ok"`
		DBOK bool `json:"db_ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		fmt.Fprintf(errOut, "health: decode response: %v\n", err)
		return 1
	}
	if !body.OK || !body.DBOK {
		fmt.Fprintf(errOut, "health: ok=%v db_ok=%v\n", body.OK, body.DBOK)
		return 1
	}
	return 0
}

// healthURL resolves the URL the health probe should hit. If --addr is set
// it wins for the URL; otherwise the bind/port from config.yaml. Empty
// bind ("") and 0.0.0.0 are normalized to 127.0.0.1 since the probe runs
// in the same network namespace as the daemon. The bearer token is always
// pulled from config when present — even with --addr set, the daemon still
// enforces it.
func healthURL(cfgPath, addrFlag string) (url, token string, err error) {
	cfg, cfgErr := config.Load(cfgPath)
	if addrFlag != "" {
		// Best-effort token from config; URL comes from the flag.
		if cfgErr == nil {
			token = cfg.Server.LANToken
		}
		return addrFlag + "/api/health", token, nil
	}
	if cfgErr != nil {
		return "", "", fmt.Errorf("load %s: %w", cfgPath, cfgErr)
	}
	bind := cfg.Server.Bind
	if bind == "" || bind == "0.0.0.0" {
		bind = "127.0.0.1"
	}
	port := cfg.Server.Port
	if port == 0 {
		port = 7777
	}
	return fmt.Sprintf("http://%s:%d/api/health", bind, port), cfg.Server.LANToken, nil
}
