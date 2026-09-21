package main

import (
	"log"
	"net/http"
	"os"
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

	s := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("relay listening on %s (base path %q)", addr, base)
	return s.ListenAndServe()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
