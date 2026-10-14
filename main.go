package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":9080", "listen address (e.g. :9080)")
	var cfg Config
	flag.IntVar(&cfg.MaxConns, "max-conns", 1000, "max concurrent WebSocket connections")
	flag.IntVar(&cfg.MaxConnsPerIP, "max-conns-per-ip", 16, "max concurrent WebSocket connections per client IP (IPv6: per /64)")
	flag.IntVar(&cfg.MaxLobbies, "max-lobbies", 500, "max concurrent lobbies")
	realIPHeaders := flag.String("real-ip-header", defaultRealIPHeaders, "comma-separated headers holding the client IP set by reverse proxies, first valid one wins (e.g. CF-Connecting-IP, X-Real-IP, X-Forwarded-For); only trusted from loopback/private peers; empty to always use the TCP address")
	allowedOrigins := flag.String("allowed-origins", defaultAllowedOrigins, "comma-separated browser origins allowed to connect (the web build); clients without an Origin header are always allowed")
	flag.Parse()
	cfg.RealIPHeaders = splitList(*realIPHeaders)
	cfg.AllowedOrigins = splitList(*allowedOrigins)

	hub := newHub(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	mux.HandleFunc("/", hub.ServeWS)

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// Connections still sending headers aren't counted against the caps yet,
		// so don't let them linger (slowloris).
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("DankNoonerSignalServer listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
