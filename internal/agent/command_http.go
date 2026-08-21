package agent

import (
	"net/http"
	"strings"
)

func (s *Server) CommandEndpoint() http.Handler {
	return s.auth(http.HandlerFunc(s.handleRunCommand))
}

func (s *Server) handleRunCommand(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Command        string `json:"command"`
		Workdir        string `json:"workdir"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(input.Command) == "" {
		writeError(w, 400, "command is required")
		return
	}
	if input.TimeoutSeconds < 0 {
		writeError(w, 400, "timeout_seconds must be positive")
		return
	}

	result, err := s.runHostCommand(r.Context(), input.Command, input.Workdir, input.TimeoutSeconds)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, result)
}
