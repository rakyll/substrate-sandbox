package service_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/rakyll/substrate-sandbox/internal/api"
	"github.com/rakyll/substrate-sandbox/internal/fakecontrol"
	"github.com/rakyll/substrate-sandbox/internal/fakerouter"
	"github.com/rakyll/substrate-sandbox/internal/guest"
	"github.com/rakyll/substrate-sandbox/internal/service"
	"github.com/rakyll/substrate-sandbox/sandbox"
)

func newAPI(t *testing.T) (*httptest.Server, *fakerouter.Router) {
	t.Helper()

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.Status(id) == ateapipb.Actor_STATUS_RUNNING
	}
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	client, err := sandbox.New(sandbox.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		SkipVerify:  true,
		AutoResume:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	srv := httptest.NewServer(service.Handler(client))
	t.Cleanup(srv.Close)
	return srv, router
}

func do(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return v
}

func TestRESTLifecycleAndExec(t *testing.T) {
	srv, router := newAPI(t)
	router.Register("web-1", (&guest.Server{Workdir: t.TempDir()}).Handler())

	// Create.
	resp := do(t, "POST", srv.URL+"/v1/sandboxes", `{"id":"web-1","template":"default","namespace":"sandboxes"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	created := decode[service.SandboxInfo](t, resp)
	if created.ID != "web-1" || created.Status != "running" {
		t.Fatalf("created = %+v, want web-1 running", created)
	}

	// Write and read a file through the REST API.
	resp = do(t, "PUT", srv.URL+"/v1/sandboxes/web-1/files?path="+url.QueryEscape("app/main.txt"), "file body")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("write file status = %d, want 204", resp.StatusCode)
	}

	// Exec.
	resp = do(t, "POST", srv.URL+"/v1/sandboxes/web-1/cmd", `{"command":["sh","-c","cat app/main.txt"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cmd status = %d, want 200", resp.StatusCode)
	}
	res := decode[api.CmdResult](t, resp)
	if res.Stdout != "file body" || res.ExitCode != 0 {
		t.Fatalf("cmd result = %+v, want stdout %q", res, "file body")
	}

	// Suspend, then exec again: auto-resume kicks in.
	resp = do(t, "POST", srv.URL+"/v1/sandboxes/web-1/suspend", "")
	if got := decode[service.SandboxInfo](t, resp); got.Status != "suspended" {
		t.Fatalf("after suspend = %+v, want suspended", got)
	}
	resp = do(t, "POST", srv.URL+"/v1/sandboxes/web-1/cmd", `{"command":["sh","-c","echo back"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cmd after suspend status = %d, want 200", resp.StatusCode)
	}
	if res := decode[api.CmdResult](t, resp); res.Stdout != "back\n" {
		t.Fatalf("cmd after suspend = %+v, want stdout %q", res, "back\n")
	}

	// List directory.
	resp = do(t, "GET", srv.URL+"/v1/sandboxes/web-1/dir?path=app", "")
	listing := decode[api.ListDirResponse](t, resp)
	if len(listing.Entries) != 1 || listing.Entries[0].Name != "main.txt" {
		t.Fatalf("listing = %+v, want [main.txt]", listing.Entries)
	}

	// Delete.
	resp = do(t, "DELETE", srv.URL+"/v1/sandboxes/web-1", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp = do(t, "GET", srv.URL+"/v1/sandboxes/web-1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", resp.StatusCode)
	}
}

func TestCreateStartDefaults(t *testing.T) {
	srv, router := newAPI(t)
	router.Register("started", (&guest.Server{Workdir: t.TempDir()}).Handler())

	// Omitting "start" starts the sandbox.
	resp := do(t, "POST", srv.URL+"/v1/sandboxes", `{"id":"started","template":"default","namespace":"sandboxes"}`)
	if got := decode[service.SandboxInfo](t, resp); got.Status != "running" {
		t.Errorf("create without start = %+v, want running", got)
	}

	// An explicit "start": false registers the sandbox without starting it.
	resp = do(t, "POST", srv.URL+"/v1/sandboxes", `{"id":"cold","template":"default","namespace":"sandboxes","start":false}`)
	if got := decode[service.SandboxInfo](t, resp); got.Status != "suspended" {
		t.Errorf("create with start=false = %+v, want suspended", got)
	}
}

func TestRESTValidation(t *testing.T) {
	srv, _ := newAPI(t)

	resp := do(t, "POST", srv.URL+"/v1/sandboxes", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create without id status = %d, want 400", resp.StatusCode)
	}
	resp = do(t, "POST", srv.URL+"/v1/sandboxes", `{"id":"no-template"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create without template status = %d, want 400", resp.StatusCode)
	}
	resp = do(t, "GET", srv.URL+"/v1/sandboxes/absent", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get absent status = %d, want 404", resp.StatusCode)
	}
	apiErr := decode[api.Error](t, resp)
	if apiErr.Code != api.CodeNotFound {
		t.Errorf("error code = %q, want %q", apiErr.Code, api.CodeNotFound)
	}
}
