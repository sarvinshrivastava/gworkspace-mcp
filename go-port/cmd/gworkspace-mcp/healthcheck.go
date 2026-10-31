package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// -healthcheck: the container HEALTHCHECK. A scratch image ships no shell,
// curl or node, so the binary checks its own /healthz and exits 0/1. Never
// prints the URL's response body: it can hold credential-state detail.
func runHealthcheck(port int) int {
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		fmt.Println("healthcheck: unreachable")
		return 1
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		fmt.Printf("healthcheck: HTTP %d\n", res.StatusCode)
		return 1
	}
	return 0
}

// healthcheckExit handles `-healthcheck` before any boot work. Returns nil
// when the flag is absent.
func healthcheckExit() *int {
	if len(os.Args) < 2 || os.Args[1] != "-healthcheck" {
		return nil
	}
	port := 3014
	if v := os.Getenv("PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 {
			port = p
		}
	}
	code := runHealthcheck(port)
	return &code
}
