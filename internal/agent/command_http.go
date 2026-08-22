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
		Stdin          string `json:"stdin"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(input.Command) == "" {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	if input.TimeoutSeconds < 0 {
		writeError(w, http.StatusBadRequest, "timeout_seconds must be positive")
		return
	}

	result, err := s.runHostCommand(r.Context(), input.Command, input.Workdir, input.Stdin, input.TimeoutSeconds)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
