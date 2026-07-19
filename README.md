# substrate-sandbox

A sandboxing service on top of [Agent Substrate](https://github.com/agent-substrate/substrate): isolated, stateful execution environments
that can be **suspended**, **resumed** on any available worker,
and driven remotely with **command execution** and **filesystem operations**.

Each sandbox is a Substrate *actor* running in an isolated container.
Substrate provides the heavy lifting — snapshotting, scheduling,
multiplexing many idle sandboxes onto a small worker pool, and routing —
while this project adds the sandbox-shaped API on top.

## Overview

```
                 lifecycle (create/suspend/resume/delete)
   ┌──────────┐          ┌────────────┐
   │ SDK      ├─────────▶│   ateapi   │  Substrate control plane
   │ sbcli    │          └────────────┘
   │ sandboxd │          ┌────────────┐      ┌──────────────────────┐
   └──────────┘─────────▶│   atenet   ├─────▶│ actor                │
                  cmd/fs │   router   │      │  └ substrate-guestd  │
       Host: <id>.actors.└────────────┘      │     /v1/cmd          │
        resources.substrate.ate.dev          │     /v1/fs/*         │
                                             └──────────────────────┘
```

- **`sandbox/`** — the SDK. `Create`, `Open`, `List`, and per-sandbox
  `Suspend`, `Pause`, `Resume`, `Delete`, `Cmd`, `ReadFile`, `WriteFile`,
  `ListDir`, `Stat`, `Mkdir`, `Remove` per actor.
- **`cmd/substrate-sandboxd`** — a REST service exposing the API.
- **`cmd/substrate-guestd`** — the daemon baked into the sandbox. It runs
  inside every actor and serves command executions and filesystem operations.

## Installation

```bash
go install github.com/rakyll/substrate-sandbox/cmd/...@latest
```

## Quickstart

Prerequisites: a cluster with Agent Substrate installed (see the Substrate
README), `ko`, and a snapshots bucket.

```bash
# 1. Deploy the system: namespace, worker pool, and sandbox template.
#    Images must be digest-pinned; build and push them with ko.
export KO_DOCKER_REPO=gcr.io/<your-project>
sbcli deploy \
  --guestd-image $(ko build github.com/rakyll/substrate-sandbox/cmd/substrate-guestd) \
  --ateom-image  $(cd <substrate-checkout> && ko build ./cmd/ateom-gvisor) \
  --snapshots-location gs://<your-bucket>/substrate-sandbox/

# 2. Port-forward the Substrate control plane and router.
kubectl port-forward -n ate-system svc/ateapi 8080:443 &
kubectl port-forward -n ate-system svc/atenet-router 8000:80 &

# 3. Create and use a sandbox.
sbcli create sandbox-dev
sbcli cmd sandbox-dev 'echo hello > /workspace/note.txt'
sbcli suspend sandbox-dev
sbcli cmd sandbox-dev 'cat /workspace/note.txt' # auto-resumes; prints hello
sbcli rm sandbox-dev
```

Or run the REST service:

```bash
substrate-sandboxd
curl -X POST localhost:8081/v1/sandboxes -d '{"id":"sandbox-dev"}'
curl -X POST localhost:8081/v1/sandboxes/sandbox-dev/cmd \
     -d '{"command":["sh","-c","uname -a"]}'
```

## CLI

```bash
$ sbcli
Manage sandboxes on Agent Substrate

Usage:
  sbcli [command]

Available Commands:
  cmd         Run a shell command line in the sandbox
  completion  Generate the autocompletion script for the specified shell
  create      Create and start a sandbox
  deploy      Deploy the system to a cluster running Agent Substrate
  help        Help about any command
  info        Show a sandbox's status
  ls          List sandboxes
  lsdir       List a sandbox directory
  mkdir       Create a directory in the sandbox
  pause       Snapshot locally on the node for fast resume
  read        Print a sandbox file to stdout
  resume      Resume from the latest snapshot
  rm          Delete a sandbox (suspends it first if needed)
  rmpath      Delete a file or directory tree in the sandbox
  stat        Stat a sandbox path
  suspend     Snapshot to external storage and free the worker
  write       Write stdin to a sandbox file
```

## SDK

```go
client, err := sandbox.New(sandbox.Options{
    ControlAddr: "localhost:8080",              // ateapi gRPC
    RouterAddr:  "localhost:8000",              // atenet router
    Template:    "sandbox",                     // ActorTemplate name
    SkipVerify:  true,                          // ateapi uses pod certs
    AutoResume:  true,                          // wake sandboxes on use
})
if err != nil {
    log.Fatalf("connecting to Substrate: %v", err)
}
defer client.Close()

sb, err := client.Create(ctx, "sandbox-dev")
if err != nil {
    log.Fatalf("creating sandbox: %v", err)
}
if err := sb.WriteFile(ctx, "/workspace/main.go", src, 0o644); err != nil {
    log.Fatalf("writing main.go: %v", err)
}
res, err := sb.Cmd(ctx, "cd /workspace && go run main.go")
if err != nil {
    log.Fatalf("running main.go: %v", err)
}
fmt.Println(res.Stdout, res.ExitCode)

sb.Suspend(ctx)
sb.Resume(ctx)
sb.Delete(ctx)
```

See [examples/quickstart](examples/quickstart/main.go) for a complete
program.

`Suspend` writes the snapshot to object storage and survives node loss;
`Pause` keeps it on the node for faster resume. With `AutoResume`, command
and file operations transparently resume a suspended sandbox and retry.

## API

`substrate-sandboxd` serves the REST API (default `:8081`). Responses are
JSON unless noted.

### Sandboxes

| Method   | Path                 | Description                            |
| -------- | -------------------- | -------------------------------------- |
| `POST`   | `/v1/sandboxes`      | Create a sandbox                        |
| `GET`    | `/v1/sandboxes`      | List sandboxes                          |
| `GET`    | `/v1/sandboxes/{id}` | Get a sandbox's status                  |
| `DELETE` | `/v1/sandboxes/{id}` | Delete (suspends first if running)      |

Create body (only `id` is required):

```json
{
  "id": "sandbox-dev",
  "template": "sandbox",  // defaults to "sandbox"
  "namespace": "default", // defaults to "default"
  "start": true           // defaults to true
}
```

### Lifecycle

| Method | Path                         | Description                              |
| ------ | ---------------------------- | ---------------------------------------- |
| `POST` | `/v1/sandboxes/{id}/suspend` | Snapshot to object storage, free worker  |
| `POST` | `/v1/sandboxes/{id}/pause`   | Snapshot locally on node for fast resume |
| `POST` | `/v1/sandboxes/{id}/resume`  | Restore from the latest snapshot         |

Each returns the sandbox's new status: `{"id": "...", "status": "suspended", ...}`.

### Commands

`POST /v1/sandboxes/{id}/cmd`

```json
{                                           {
  "command": ["sh", "-c", "make test"],       "stdout": "ok\n",
  "cwd": "/workspace/app",                    "stderr": "",
  "env": {"CI": "true"},                      "exitCode": 0,
  "timeout": "60s"                            "duration": "1.2s"
}                                           }
```

Output is capped at 2 MiB per stream; `stdoutTruncated`/`stderrTruncated`
report when the cap was hit, and `timedOut` reports a timeout kill.

### Filesystem

| Method   | Path                                      | Description                    |
| -------- | ----------------------------------------- | ------------------------------ |
| `GET`    | `/v1/sandboxes/{id}/files?path=`          | Read a file (raw bytes)        |
| `PUT`    | `/v1/sandboxes/{id}/files?path=&mode=644` | Write a file (raw body)        |
| `DELETE` | `/v1/sandboxes/{id}/files?path=`          | Delete a file or directory tree|
| `GET`    | `/v1/sandboxes/{id}/dir?path=`            | List a directory               |
| `POST`   | `/v1/sandboxes/{id}/dir?path=&mode=755`   | Create a directory (mkdir -p)  |
| `GET`    | `/v1/sandboxes/{id}/stat?path=`           | Stat a path                    |

Relative paths resolve against the guest's workdir (`/workspace` in the
shipped template). Writes create missing parent directories.

### Errors

Non-2xx responses carry a JSON envelope:

```json
{"error": "sandbox \"sandbox-dev\" not found", "code": "not_found"}
```

Codes: `not_found`, `invalid_argument`, `is_directory`, `not_directory`,
`internal`.
