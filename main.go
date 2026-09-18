package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := env("RELAY_ADDR", ":8899")
	if err := run(addr); err != nil {
		log.Fatal(err)
	}
}

func run(addr string) error {
	mux := http.NewServeMux()
	srv := newRelayServer()

	mux.HandleFunc("GET /", srv.handleIndex)
	mux.HandleFunc("GET /login", srv.handleLogin)
	mux.HandleFunc("GET /callback", srv.handleCallback)
	mux.HandleFunc("GET /logout", srv.handleLogout)
	mux.HandleFunc("GET /status", srv.handleStatus)
	mux.HandleFunc("POST /control", srv.handleControl)

	s := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("relay listening on %s", addr)
	return s.ListenAndServe()
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
