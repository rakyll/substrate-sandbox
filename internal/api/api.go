// Package api defines the wire types shared between the sandbox guest
// daemon (which runs inside a Substrate actor) and clients of the
// sandbox service.
package api

import "time"

// CmdRequest describes a command to run inside a sandbox.
type CmdRequest struct {
	// Command is the argv of the process to run. It is executed directly,
	// not through a shell. Use []string{"sh", "-c", "..."} for shell syntax.
	Command []string `json:"command"`

	// Env holds additional environment variables set for the process, on
	// top of the guest daemon's environment.
	Env map[string]string `json:"env,omitempty"`

	// Cwd is the working directory for the process. Defaults to the guest
	// daemon's working directory.
	Cwd string `json:"cwd,omitempty"`

	// Stdin is fed to the process's standard input. It is base64-encoded
	// in JSON.
	Stdin []byte `json:"stdin,omitempty"`

	// Timeout bounds command execution, formatted as a Go duration string
	// (e.g. "30s"). Zero or empty means no timeout beyond the HTTP
	// request's own deadline.
	Timeout string `json:"timeout,omitempty"`
}

// CmdResult is the outcome of a CmdRequest.
type CmdResult struct {
	// Stdout and Stderr hold the captured output, capped at the guest's
	// output limit per stream.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`

	// ExitCode is the process exit code. -1 if the process was killed by
	// a signal or failed to start.
	ExitCode int `json:"exitCode"`

	// TimedOut reports whether the command was killed because it exceeded
	// the requested timeout.
	TimedOut bool `json:"timedOut,omitempty"`

	// StdoutTruncated / StderrTruncated report whether output exceeded the
	// per-stream cap and was cut off.
	StdoutTruncated bool `json:"stdoutTruncated,omitempty"`
	StderrTruncated bool `json:"stderrTruncated,omitempty"`

	// Duration is how long the command ran, as a Go duration string.
	Duration string `json:"duration,omitempty"`
}

// DirEntry describes a file or directory inside the sandbox.
type DirEntry struct {
	// Name is the base name of the entry.
	Name string `json:"name"`

	// Path is the absolute path of the entry inside the sandbox.
	Path string `json:"path"`

	// Size is the file size in bytes (system-dependent for directories).
	Size int64 `json:"size"`

	// Mode is the numeric file mode (fs.FileMode bits).
	Mode uint32 `json:"mode"`

	// ModeString is the human-readable form, e.g. "-rw-r--r--".
	ModeString string `json:"modeString"`

	IsDir   bool      `json:"isDir"`
	ModTime time.Time `json:"modTime"`
}

// ListDirResponse is the response of the directory listing endpoint.
type ListDirResponse struct {
	Entries []DirEntry `json:"entries"`
}

// CreateSandboxRequest is the body of POST /v1/sandboxes.
type CreateSandboxRequest struct {
	// ID is the sandbox identifier (a DNS-1123 label). Required.
	ID string `json:"id"`

	// Template is the name of the ActorTemplate the sandbox is created
	// from. Defaults to the service's default template ("sandbox").
	Template string `json:"template,omitempty"`

	// Namespace is the Kubernetes namespace the ActorTemplate lives in.
	// Defaults to "default".
	Namespace string `json:"namespace,omitempty"`

	// WorkerSelector constrains which worker pools can host the sandbox.
	WorkerSelector map[string]string `json:"workerSelector,omitempty"`
}

// SandboxInfo is the JSON representation of a sandbox.
type SandboxInfo struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Template  string `json:"template,omitempty"`
	Namespace string `json:"namespace,omitempty"`

	// WorkerPod fields identify the pod hosting the sandbox, when running.
	WorkerPod          string `json:"workerPod,omitempty"`
	WorkerPodNamespace string `json:"workerPodNamespace,omitempty"`
	WorkerPodIP        string `json:"workerPodIP,omitempty"`
}

// FSRequest is the body of the filesystem endpoints
// (POST /v1/sandboxes/{id}/{file,dir,stat}).
type FSRequest struct {
	// Path of the file or directory inside the sandbox. Relative paths
	// resolve against the guest's workdir. Required.
	Path string `json:"path"`

	// Mode is the octal file mode for write and mkdir, e.g. "644".
	// Defaults to "644" for files and "755" for directories.
	Mode string `json:"mode,omitempty"`

	// Content is the file content for write. It is base64-encoded in
	// JSON.
	Content []byte `json:"content,omitempty"`
}

// Error codes returned in Error.Code.
const (
	CodeNotFound        = "not_found"
	CodeInvalidArgument = "invalid_argument"
	CodeNotFile         = "not_file"
	CodeNotDirectory    = "not_directory"
	CodeInternal        = "internal"
)

// Error is the JSON error envelope returned by the guest daemon and the
// ssbx-api service on non-2xx responses.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return e.Message }
