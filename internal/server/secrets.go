package server

import (
	"encoding/json"
	"net/http"
)

// Secrets are write-only: set, list names, delete. No API returns a value —
// they exist solely to be injected into the project's deployed containers.

func (s *Server) handleSetSecret(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	org := orgFrom(r.Context())
	p, err := s.Store.GetProject(r.Context(), org, r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if err := s.Store.SetSecret(r.Context(), org, p, in.Key, in.Value, s.actorFrom(r)); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"set": in.Key, "note": "takes effect on the next deployment"}, nil)
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProject(r.Context(), orgFrom(r.Context()), r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	meta, err := s.Store.ListSecretMeta(r.Context(), p.ID)
	respond(w, map[string]any{"secrets": meta}, err)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	p, err := s.Store.GetProject(r.Context(), org, r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	err = s.Store.DeleteSecret(r.Context(), org, p, r.PathValue("key"), s.actorFrom(r))
	respond(w, map[string]any{"deleted": r.PathValue("key")}, err)
}
