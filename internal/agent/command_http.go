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
		Repo           string `json:"repo"`
		Command        string `json:"command"`
		Workdir        string `json:"workdir"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	repo, err := s.authorizeRepo(input.Repo)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if !s.repoExists(repo) {
		writeError(w, 409, "repository is not local; call syncRepository first")
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

	unlock := s.locker.Lock(repo)
	defer unlock()
	result, err := s.runSandboxCommand(r.Context(), repo, input.Command, input.Workdir, input.TimeoutSeconds)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, result)
}
