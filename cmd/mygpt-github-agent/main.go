package main

import (
	"log"
	"net/http"
	"time"

	"github.com/xiaoqianran/cloudflare-tunnel-mygpt-github/internal/agent"
)

func main() {
	cfg, err := agent.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.EnsureWorkspace(cfg); err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           agent.NewServer(cfg).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	log.Printf("mygpt github agent listening on %s", cfg.Address)
	log.Fatal(server.ListenAndServe())
}
