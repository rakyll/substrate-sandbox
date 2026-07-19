# substrate-sandbox

A sandboxing service on top of [Agent Substrate](https://github.com/agent-substrate/substrate),
in the spirit of Claude Sandbox: isolated, stateful execution environments
that can be **suspended** (full memory + filesystem snapshot to object
storage), **resumed** on any available worker, and driven remotely with
**command execution** and **filesystem operations**.

Each sandbox is a Substrate *actor* running in a gVisor-isolated container.
Substrate provides the heavy lifting — snapshotting, scheduling,
multiplexing many idle sandboxes onto a small worker pool, and routing —
while this project adds the sandbox-shaped API on top.

## Architecture

```
                 lifecycle (create/suspend/resume/delete)
   ┌──────────┐   gRPC   ┌────────────┐
   │ Go SDK   ├─────────▶│   ateapi   │  Substrate control plane
   │  sbcli   │          └────────────┘
   │ sandboxd │   HTTP   ┌────────────┐      ┌──────────────────────┐
   └──────────┘─────────▶│   atenet   ├─────▶│ actor (gVisor)       │
                 exec/fs │   router   │      │  └ substrate-guestd  │
       Host: <id>.actors.└────────────┘      │     /v1/exec         │
        resources.substrate.ate.dev          │     /v1/fs/*         │
                                             └──────────────────────┘
```

- **`sandbox/`** — the Go SDK. `Create`, `Open`, `List`, and per-sandbox
  `Suspend`, `Pause`, `Resume`, `Delete`, `Exec`, `ReadFile`, `WriteFile`,
  `ListDir`, `Stat`, `Mkdir`, `Remove`. Lifecycle calls go to the `ateapi`
  gRPC service; exec/fs calls go through the `atenet` router, addressed by
  Host header.
- **`cmd/substrate-guestd`** — the daemon baked into the sandbox image. Runs
  inside every actor and serves exec + filesystem endpoints. Because
  Substrate snapshots the whole container (RAM and filesystem), everything
  a command creates survives suspend/resume.
- **`cmd/substrate-sandboxd`** — a REST service exposing the same
  abstraction to any language (see the HTTP API below).
- **`cmd/sbcli`** — a small CLI over the SDK.
- **`service/`** — the REST handler used by `substrate-sandboxd`.
- **`manifests/`** — the `WorkerPool` and `ActorTemplate` that define the
  sandbox environment.

## Quickstart

Prerequisites: a cluster with Agent Substrate installed (see the Substrate
README), `ko`, and a snapshots bucket.

```bash
# 1. Deploy the worker pool and sandbox template.
export KO_DOCKER_REPO=gcr.io/<your-project>
make deploy BUCKET_NAME=<your-bucket>
kubectl wait --for=condition=Ready actortemplate/sandbox -n substrate-sandbox --timeout=5m

# 2. Port-forward the Substrate control plane and router.
kubectl port-forward -n ate-system svc/ateapi 8080:443 &
kubectl port-forward -n ate-system svc/atenet-router 8000:80 &

# 3. Use the CLI.
go build -o bin/sbcli ./cmd/sbcli
export SBCLI_TEMPLATE=substrate-sandbox/sandbox

bin/sbcli create dev-1
bin/sbcli exec dev-1 'echo hello > /workspace/note.txt'
bin/sbcli suspend dev-1          # snapshot + free the worker
bin/sbcli exec dev-1 'cat /workspace/note.txt'   # auto-resumes; prints hello
bin/sbcli rm dev-1
```

Or run the REST service:

```bash
go run ./cmd/substrate-sandboxd -template substrate-sandbox/sandbox
curl -X POST localhost:8081/v1/sandboxes -d '{"id":"dev-1"}'
curl -X POST localhost:8081/v1/sandboxes/dev-1/exec \
     -d '{"command":["sh","-c","uname -a"]}'
```

## Go SDK

```go
client, err := sandbox.New(sandbox.Options{
    ControlAddr: "localhost:8080",              // ateapi gRPC
    RouterAddr:  "localhost:8000",              // atenet router
    Template:    "substrate-sandbox/sandbox",   // ActorTemplate ns/name
    SkipVerify:  true,                          // ateapi uses pod certs
    AutoResume:  true,                          // wake sandboxes on use
})

sb, _ := client.Create(ctx, "dev-1")
sb.WriteFile(ctx, "/workspace/main.go", src, 0o644)
res, _ := sb.Command(ctx, "cd /workspace && go run main.go")
fmt.Println(res.Stdout, res.ExitCode)

sb.Suspend(ctx)   // hibernate: RAM + fs snapshotted, worker freed
sb.Resume(ctx)    // restore on any eligible worker
sb.Delete(ctx)    // suspends first if needed, then deletes
```

See [examples/quickstart](examples/quickstart/main.go) for a complete
program.

`Suspend` writes the snapshot to object storage and survives node loss;
`Pause` keeps it on the node for faster resume. With `AutoResume`, exec and
file operations transparently resume a suspended sandbox and retry.

## HTTP API (`substrate-sandboxd`)

| Method & path                        | Meaning                                  |
| ------------------------------------ | ---------------------------------------- |
| `POST /v1/sandboxes`                 | create (`{"id", "template"?, "start"?}`) |
| `GET /v1/sandboxes`                  | list                                     |
| `GET /v1/sandboxes/{id}`             | status                                   |
| `DELETE /v1/sandboxes/{id}`          | delete                                   |
| `POST /v1/sandboxes/{id}/suspend`    | snapshot to storage, free worker         |
| `POST /v1/sandboxes/{id}/pause`      | snapshot locally for fast resume         |
| `POST /v1/sandboxes/{id}/resume`     | restore from latest snapshot             |
| `POST /v1/sandboxes/{id}/exec`       | run a command (`api.ExecRequest`)        |
| `GET /v1/sandboxes/{id}/files?path=` | read file (raw bytes)                    |
| `PUT /v1/sandboxes/{id}/files?path=&mode=` | write file (raw body)              |
| `DELETE /v1/sandboxes/{id}/files?path=&recursive=` | delete path                |
| `GET /v1/sandboxes/{id}/dir?path=`   | list directory                           |
| `POST /v1/sandboxes/{id}/dir?path=`  | mkdir -p                                 |
| `GET /v1/sandboxes/{id}/stat?path=`  | stat                                     |

Errors are `{"error": "...", "code": "not_found" | "invalid_argument" | ...}`.

## Notes

- Relative paths (in exec `cwd` and file operations) resolve against the
  guest's workdir, `/workspace` in the shipped template.
- Exec output is capped at 2 MiB per stream (`stdoutTruncated` /
  `stderrTruncated` are set when hit); write large output to files and
  fetch them instead.
- This module pins Substrate to the local checkout at `../substrate` via a
  `replace` directive, since Substrate's API is still churning; `make
  deploy` also builds `ateom-gvisor` from that checkout through the ko
  `ko://` reference.
- Like the Substrate sandbox demo, there is **no authn/authz** on
  `substrate-sandboxd` or the guest daemon — do not expose them to
  untrusted networks.

## Development

```bash
make build   # build SDK, guest daemon, CLI
make test    # unit + integration tests (fake control plane & router)
make vet
```
