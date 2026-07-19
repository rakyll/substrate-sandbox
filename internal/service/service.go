// Package service exposes the sandbox abstraction as a REST API, so that
// sandboxes can be managed from any language without the Go SDK.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/rakyll/substrate-sandbox/internal/api"
	"github.com/rakyll/substrate-sandbox/internal/direct"
)

// DefaultTemplate is the ActorTemplate name used when a create request
// does not specify one.
const DefaultTemplate = "sandbox"

func toSandboxInfo(info direct.Info) api.SandboxInfo {
	return api.SandboxInfo{
		ID:                 info.ID,
		Status:             string(info.Status),
		Template:           info.Template,
		Namespace:          info.Namespace,
		WorkerPod:          info.WorkerPod,
		WorkerPodNamespace: info.WorkerPodNamespace,
		WorkerPodIP:        info.WorkerPodIP,
	}
}

// Handler serves the sandbox REST API backed by client.
func Handler(client *direct.Client) http.Handler {
	s := &server{client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /v1/sandboxes", s.create)
	mux.HandleFunc("GET /v1/sandboxes", s.list)
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.get)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.delete)
	mux.HandleFunc("POST /v1/sandboxes/{id}/suspend", s.lifecycle((*direct.Sandbox).Suspend))
	mux.HandleFunc("POST /v1/sandboxes/{id}/pause", s.lifecycle((*direct.Sandbox).Pause))
	mux.HandleFunc("POST /v1/sandboxes/{id}/resume", s.lifecycle((*direct.Sandbox).Resume))
	mux.HandleFunc("POST /v1/sandboxes/{id}/cmd", s.cmd)
	mux.HandleFunc("GET /v1/sandboxes/{id}/file", s.readFile)
	mux.HandleFunc("POST /v1/sandboxes/{id}/file", s.writeFile)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/file", s.removePath)
	mux.HandleFunc("GET /v1/sandboxes/{id}/dir", s.listDir)
	mux.HandleFunc("POST /v1/sandboxes/{id}/dir", s.mkdir)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/dir", s.removeDir)
	mux.HandleFunc("GET /v1/sandboxes/{id}/stat", s.stat)
	return mux
}

type server struct {
	client *direct.Client
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := api.CodeInternal
	if errors.Is(err, direct.ErrNotFound) {
		status = http.StatusNotFound
		code = api.CodeNotFound
	}
	writeJSON(w, status, api.Error{Code: code, Message: err.Error()})
}

func writeBadRequest(w http.ResponseWriter, format string, args ...any) {
	writeJSON(w, http.StatusBadRequest, api.Error{
		Code:    api.CodeInvalidArgument,
		Message: fmt.Sprintf(format, args...),
	})
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	// Start defaults to true; decoding leaves it untouched when the
	// request omits the field.
	req := api.CreateSandboxRequest{Start: true}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "invalid request body: %v", err)
		return
	}
	if req.ID == "" {
		writeBadRequest(w, "id is required")
		return
	}
	if req.Template == "" {
		req.Template = DefaultTemplate
	}
	opts := []direct.CreateOption{direct.WithTemplate(req.Template)}
	if req.Namespace != "" {
		opts = append(opts, direct.WithNamespace(req.Namespace))
	}
	if len(req.WorkerSelector) > 0 {
		opts = append(opts, direct.WithWorkerSelector(req.WorkerSelector))
	}
	if !req.Start {
		opts = append(opts, direct.WithoutStart())
	}
	sb, err := s.client.Create(r.Context(), req.ID, opts...)
	if err != nil {
		writeErr(w, err)
		return
	}
	info, err := sb.Info(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSandboxInfo(info))
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	infos, err := s.client.List(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]api.SandboxInfo, 0, len(infos))
	for _, info := range infos {
		out = append(out, toSandboxInfo(info))
	}
	writeJSON(w, http.StatusOK, api.ListSandboxesResponse{Sandboxes: out})
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	info, err := s.client.Sandbox(r.PathValue("id")).Info(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toSandboxInfo(info))
}

func (s *server) delete(w http.ResponseWriter, r *http.Request) {
	if err := s.client.Sandbox(r.PathValue("id")).Delete(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) lifecycle(op func(*direct.Sandbox, context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sb := s.client.Sandbox(r.PathValue("id"))
		if err := op(sb, r.Context()); err != nil {
			writeErr(w, err)
			return
		}
		info, err := sb.Info(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, toSandboxInfo(info))
	}
}

func (s *server) cmd(w http.ResponseWriter, r *http.Request) {
	var req api.CmdRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "invalid request body: %v", err)
		return
	}
	res, err := s.client.Sandbox(r.PathValue("id")).Run(r.Context(), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// queryPath returns the required "path" query parameter, writing an error
// response and reporting false when it is missing.
func queryPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeBadRequest(w, "path query parameter is required")
		return "", false
	}
	return path, true
}

// decodeFS decodes and validates the shared filesystem request body. It
// writes an error response and reports false when the request is invalid.
func decodeFS(w http.ResponseWriter, r *http.Request) (api.FSRequest, bool) {
	var req api.FSRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "invalid request body: %v", err)
		return req, false
	}
	if req.Path == "" {
		writeBadRequest(w, "path is required")
		return req, false
	}
	return req, true
}

// fsMode parses the request's octal mode, falling back to def when unset.
func fsMode(w http.ResponseWriter, req api.FSRequest, def fs.FileMode) (fs.FileMode, bool) {
	if req.Mode == "" {
		return def, true
	}
	v, err := strconv.ParseUint(req.Mode, 8, 32)
	if err != nil {
		writeBadRequest(w, "invalid mode %q: %v", req.Mode, err)
		return 0, false
	}
	return fs.FileMode(v), true
}

func (s *server) readFile(w http.ResponseWriter, r *http.Request) {
	path, ok := queryPath(w, r)
	if !ok {
		return
	}
	rc, err := s.client.Sandbox(r.PathValue("id")).ReadFile(r.Context(), path)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, rc)
}

func (s *server) writeFile(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeFS(w, r)
	if !ok {
		return
	}
	mode, ok := fsMode(w, req, 0o644)
	if !ok {
		return
	}
	if err := s.client.Sandbox(r.PathValue("id")).WriteFile(r.Context(), req.Path, bytes.NewReader(req.Content), mode); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) removePath(w http.ResponseWriter, r *http.Request) {
	path, ok := queryPath(w, r)
	if !ok {
		return
	}
	sb := s.client.Sandbox(r.PathValue("id"))
	entry, err := sb.Stat(r.Context(), path)
	if err != nil {
		writeErr(w, err)
		return
	}
	if entry.IsDir {
		writeJSON(w, http.StatusBadRequest, api.Error{
			Code:    api.CodeNotFile,
			Message: fmt.Sprintf("%s is a directory; use the directory endpoint", path),
		})
		return
	}
	if err := sb.Remove(r.Context(), path); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listDir(w http.ResponseWriter, r *http.Request) {
	path, ok := queryPath(w, r)
	if !ok {
		return
	}
	entries, err := s.client.Sandbox(r.PathValue("id")).ListDir(r.Context(), path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ListDirResponse{Entries: entries})
}

func (s *server) mkdir(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeFS(w, r)
	if !ok {
		return
	}
	mode, ok := fsMode(w, req, 0o755)
	if !ok {
		return
	}
	if err := s.client.Sandbox(r.PathValue("id")).Mkdir(r.Context(), req.Path, mode); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) removeDir(w http.ResponseWriter, r *http.Request) {
	path, ok := queryPath(w, r)
	if !ok {
		return
	}
	sb := s.client.Sandbox(r.PathValue("id"))
	entry, err := sb.Stat(r.Context(), path)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !entry.IsDir {
		writeJSON(w, http.StatusBadRequest, api.Error{
			Code:    api.CodeNotDirectory,
			Message: fmt.Sprintf("%s is not a directory", path),
		})
		return
	}
	if err := sb.Remove(r.Context(), path); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) stat(w http.ResponseWriter, r *http.Request) {
	path, ok := queryPath(w, r)
	if !ok {
		return
	}
	entry, err := s.client.Sandbox(r.PathValue("id")).Stat(r.Context(), path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}
