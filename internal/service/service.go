// Package service exposes the sandbox abstraction as a REST API, so that
// sandboxes can be managed from any language without the Go SDK.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"

	"github.com/rakyll/substrate-sandbox/internal/api"
	"github.com/rakyll/substrate-sandbox/sandbox"
)

// CreateSandboxRequest is the body of POST /v1/sandboxes.
type CreateSandboxRequest struct {
	// ID is the sandbox identifier (a DNS-1123 label). Required.
	ID string `json:"id"`

	// Template is the name of the ActorTemplate the sandbox is created
	// from. Required.
	Template string `json:"template"`

	// Namespace is the Kubernetes namespace the ActorTemplate lives in.
	// Defaults to "default".
	Namespace string `json:"namespace,omitempty"`

	// WorkerSelector constrains which worker pools can host the sandbox.
	WorkerSelector map[string]string `json:"workerSelector,omitempty"`

	// Start controls whether the sandbox starts immediately. Defaults to
	// true when omitted from the request.
	Start bool `json:"start"`
}

// SandboxInfo is the JSON representation of a sandbox.
type SandboxInfo struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Template string `json:"template,omitempty"`
}

func toSandboxInfo(info sandbox.Info) SandboxInfo {
	out := SandboxInfo{ID: info.ID, Status: string(info.Status)}
	if info.TemplateName != "" {
		out.Template = info.Namespace + "/" + info.TemplateName
	}
	return out
}

// Handler serves the sandbox REST API backed by client.
func Handler(client *sandbox.Client) http.Handler {
	s := &server{client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /v1/sandboxes", s.create)
	mux.HandleFunc("GET /v1/sandboxes", s.list)
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.get)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.delete)
	mux.HandleFunc("POST /v1/sandboxes/{id}/suspend", s.lifecycle((*sandbox.Sandbox).Suspend))
	mux.HandleFunc("POST /v1/sandboxes/{id}/pause", s.lifecycle((*sandbox.Sandbox).Pause))
	mux.HandleFunc("POST /v1/sandboxes/{id}/resume", s.lifecycle((*sandbox.Sandbox).Resume))
	mux.HandleFunc("POST /v1/sandboxes/{id}/cmd", s.cmd)
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", s.readFile)
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files", s.writeFile)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/files", s.deleteFile)
	mux.HandleFunc("GET /v1/sandboxes/{id}/dir", s.listDir)
	mux.HandleFunc("POST /v1/sandboxes/{id}/dir", s.mkdir)
	mux.HandleFunc("GET /v1/sandboxes/{id}/stat", s.stat)
	return mux
}

type server struct {
	client *sandbox.Client
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := api.CodeInternal
	if errors.Is(err, sandbox.ErrNotFound) {
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
	req := CreateSandboxRequest{Start: true}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeBadRequest(w, "invalid request body: %v", err)
		return
	}
	if req.ID == "" {
		writeBadRequest(w, "id is required")
		return
	}
	if req.Template == "" {
		writeBadRequest(w, "template is required")
		return
	}
	opts := []sandbox.CreateOption{sandbox.WithTemplate(req.Template)}
	if req.Namespace != "" {
		opts = append(opts, sandbox.WithNamespace(req.Namespace))
	}
	if len(req.WorkerSelector) > 0 {
		opts = append(opts, sandbox.WithWorkerSelector(req.WorkerSelector))
	}
	if !req.Start {
		opts = append(opts, sandbox.WithoutStart())
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
	out := make([]SandboxInfo, 0, len(infos))
	for _, info := range infos {
		out = append(out, toSandboxInfo(info))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
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

func (s *server) lifecycle(op func(*sandbox.Sandbox, context.Context) error) http.HandlerFunc {
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

func (s *server) readFile(w http.ResponseWriter, r *http.Request) {
	data, err := s.client.Sandbox(r.PathValue("id")).ReadFile(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(data)
}

func (s *server) writeFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := fs.FileMode(0o644)
	if m := q.Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			writeBadRequest(w, "invalid mode %q: %v", m, err)
			return
		}
		mode = fs.FileMode(v)
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.client.Sandbox(r.PathValue("id")).WriteFile(r.Context(), q.Get("path"), data, mode); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) deleteFile(w http.ResponseWriter, r *http.Request) {
	err := s.client.Sandbox(r.PathValue("id")).Remove(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listDir(w http.ResponseWriter, r *http.Request) {
	entries, err := s.client.Sandbox(r.PathValue("id")).ListDir(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ListDirResponse{Entries: entries})
}

func (s *server) mkdir(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := fs.FileMode(0o755)
	if m := q.Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			writeBadRequest(w, "invalid mode %q: %v", m, err)
			return
		}
		mode = fs.FileMode(v)
	}
	if err := s.client.Sandbox(r.PathValue("id")).Mkdir(r.Context(), q.Get("path"), mode); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) stat(w http.ResponseWriter, r *http.Request) {
	entry, err := s.client.Sandbox(r.PathValue("id")).Stat(r.Context(), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}
