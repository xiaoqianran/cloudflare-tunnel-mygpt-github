package agent

import "net/http"

func NewHTTPHandler(cfg Config) http.Handler {
	s := NewServer(cfg)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/command/run", s.CommandEndpoint())
	mux.Handle("/", s.Handler())
	return mux
}
