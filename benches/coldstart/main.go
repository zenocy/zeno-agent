// Bench: cold-start latency (gate 3).
//
// Measures the time from `zeno` process start to first successful
// /api/health response. Run on the laptop and the RPi separately.
//
// Usage:
//
//	go run ./benches/coldstart -binary=/path/to/zeno -config=./config.yaml
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	var (
		binary  = flag.String("binary", "./zeno", "path to compiled zeno binary")
		config  = flag.String("config", "./config.yaml", "path to config")
		host    = flag.String("host", "http://127.0.0.1:7777", "URL where zeno will listen")
		report  = flag.String("report", "benches/REPORT.md", "path to append result to")
		label   = flag.String("label", "", "label for this run")
		timeout = flag.Duration("timeout", 30*time.Second, "max time to wait for /api/health")
	)
	flag.Parse()
	if *label == "" {
		hostName, _ := os.Hostname()
		*label = hostName
	}

	cmd := exec.Command(*binary, "-config", *config)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "[coldstart] start binary: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}()

	first := time.Time{}
	deadline := startedAt.Add(*timeout)
	for time.Now().Before(deadline) {
		r, err := http.Get(*host + "/api/health")
		if err == nil && r.StatusCode == 200 {
			r.Body.Close()
			first = time.Now()
			break
		}
		if r != nil {
			r.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if first.IsZero() {
		fmt.Fprintf(os.Stderr, "[coldstart] /api/health did not respond within %s\n", *timeout)
		os.Exit(2)
	}

	dur := first.Sub(startedAt)
	block := fmt.Sprintf("\n## coldstart · %s · %s\n\n_first /api/health response in **%s**_\n",
		*label, time.Now().Format(time.RFC3339), dur.Round(time.Millisecond))
	if err := appendFile(*report, block); err != nil {
		fmt.Fprintf(os.Stderr, "[coldstart] write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[coldstart] %s: %s\n", *label, dur.Round(time.Millisecond))

	if dur > 10*time.Second {
		os.Exit(2)
	}
}

func appendFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}
