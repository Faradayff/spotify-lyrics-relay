package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	addr := env("RELAY_ADDR", ":8899")
	if err := run(addr); err != nil {
		log.Fatal(err)
	}
}

// Normalize a base path: empty or "/" => "", "/lyrics" => "/lyrics".
func normalizeBase(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "/")
	// Leading "/" is expected.
	if !strings.HasPrefix(s, "/") && s != "" {
		s = "/" + s
	}
	return s
}

func run(addr string) error {
	base := normalizeBase(os.Getenv("RELAY_BASE_PATH"))
	mux := http.NewServeMux()
	srv := newRelayServer(base)

	mux.HandleFunc(base+"/", srv.handleIndex)
	mux.HandleFunc(base+"/login", srv.handleLogin)
	mux.HandleFunc(base+"/callback", srv.handleCallback)
	mux.HandleFunc(base+"/logout", srv.handleLogout)
	mux.HandleFunc(base+"/status", srv.handleStatus)
	mux.HandleFunc("POST "+base+"/control", srv.handleControl)

	go srv.runRefreshLoop(refreshInterval())

	s := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("relay listening on %s (base path %q)", addr, base)
	return s.ListenAndServe()
}

// refreshInterval is the wait-mode state refresh cadence. Default 300 ms
// (the same pace the app used for classic polling); override with
// RELAY_WAITS_REFRESH_MS, clamped to a 100 ms floor.
func refreshInterval() time.Duration {
	if v := os.Getenv("RELAY_WAITS_REFRESH_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 100 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 300 * time.Millisecond
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
