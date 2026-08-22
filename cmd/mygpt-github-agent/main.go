package main

import (
	"log"
	"net/http"
	"time"

	"github.com/xiaoqianran/mygpt-cf-tunnel/internal/agent"
)

func main() {
	cfg, err := agent.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           agent.NewServer(cfg).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("mygpt universal VPS root shell listening on %s", cfg.Address)
	log.Fatal(server.ListenAndServe())
}
