"""Python SDK for the substrate-sandbox service.

It talks to the ssbx-api service, which bridges to the Agent Substrate
control plane and router; ``ssbx deploy`` generates the manifests that run
that service in-cluster.

    from ssbx import SandboxClient

    client = SandboxClient("http://localhost:7777", template="sandbox")
    sb = client.create("dev1")
    sb.write_file("/workspace/note.txt", "hello")
    print(sb.cmd("cat /workspace/note.txt").stdout)
    sb.suspend()
    sb.cmd("uname -a")  # auto-resumes transparently
    sb.delete()
"""

from __future__ import annotations

import base64
import json
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Dict, List, Optional, Union

__all__ = [
    "SandboxClient",
    "Sandbox",
    "SandboxInfo",
    "CmdResult",
    "DirEntry",
    "Status",
    "SandboxError",
    "NotFoundError",
]


class Status:
    """Lifecycle states of a sandbox (the values of ``SandboxInfo.status``)."""

    UNKNOWN = "unknown"
    RESUMING = "resuming"
    RUNNING = "running"
    SUSPENDING = "suspending"
    SUSPENDED = "suspended"
    PAUSING = "pausing"
    PAUSED = "paused"
    CRASHED = "crashed"


@dataclass
class SandboxInfo:
    """A point-in-time snapshot of a sandbox's state.

    ``template`` and ``namespace`` identify the ActorTemplate the sandbox
    was created from; the ``worker_pod`` fields identify the pod hosting
    the sandbox, when running.
    """

    id: str
    status: str
    template: str = ""
    namespace: str = ""
    worker_pod: str = ""
    worker_pod_namespace: str = ""
    worker_pod_ip: str = ""


@dataclass
class CmdResult:
    """The outcome of a command run inside a sandbox.

    ``exit_code`` is -1 if the process was killed by a signal or failed to
    start. ``stdout_truncated``/``stderr_truncated`` report whether output
    exceeded the per-stream cap; ``timed_out`` reports a timeout kill.
    """

    stdout: str
    stderr: str
    exit_code: int
    timed_out: bool = False
    stdout_truncated: bool = False
    stderr_truncated: bool = False
    duration: str = ""


@dataclass
class DirEntry:
    """A file or directory inside a sandbox."""

    name: str
    path: str
    size: int
    mode: int
    mode_string: str
    is_dir: bool
    mod_time: str


class SandboxError(Exception):
    """An error returned by the sandbox API.

    ``code`` is the API's machine-readable error code (e.g. "not_found",
    "invalid_argument", "not_file", "not_directory", "internal");
    ``status`` is the HTTP status of the response, when there was one.
    """

    def __init__(self, message: str, code: Optional[str] = None, status: Optional[int] = None):
        super().__init__(message)
        self.code = code
        self.status = status


class NotFoundError(SandboxError):
    """The sandbox, file, or directory does not exist."""

    def __init__(self, message: str, status: Optional[int] = None):
        super().__init__(message, code="not_found", status=status)


def _info(d: dict) -> SandboxInfo:
    return SandboxInfo(
        id=d.get("id", ""),
        status=d.get("status", ""),
        template=d.get("template", ""),
        namespace=d.get("namespace", ""),
        worker_pod=d.get("workerPod", ""),
        worker_pod_namespace=d.get("workerPodNamespace", ""),
        worker_pod_ip=d.get("workerPodIP", ""),
    )


def _cmd_result(d: dict) -> CmdResult:
    return CmdResult(
        stdout=d.get("stdout", ""),
        stderr=d.get("stderr", ""),
        exit_code=d.get("exitCode", 0),
        timed_out=d.get("timedOut", False),
        stdout_truncated=d.get("stdoutTruncated", False),
        stderr_truncated=d.get("stderrTruncated", False),
        duration=d.get("duration", ""),
    )


def _dir_entry(d: dict) -> DirEntry:
    return DirEntry(
        name=d.get("name", ""),
        path=d.get("path", ""),
        size=d.get("size", 0),
        mode=d.get("mode", 0),
        mode_string=d.get("modeString", ""),
        is_dir=d.get("isDir", False),
        mod_time=d.get("modTime", ""),
    )


class SandboxClient:
    """Manages sandboxes through the ssbx-api service.

    ``endpoint`` is the base URL of the service, e.g.
    ``"http://localhost:7777"`` (typically a port-forward of
    ``svc/ssbx-api``). A bare ``host:port`` implies http.

    ``template`` and ``namespace`` are the defaults applied on
    :meth:`create`; empty means the service's defaults ("sandbox" in
    "substrate-sandbox").
    """

    def __init__(
        self,
        endpoint: str,
        *,
        template: Optional[str] = None,
        namespace: Optional[str] = None,
        timeout: float = 60.0,
    ):
        if not endpoint:
            raise ValueError("endpoint is required")
        if "://" not in endpoint:
            endpoint = "http://" + endpoint
        self._endpoint = endpoint.rstrip("/")
        self._template = template
        self._namespace = namespace
        self._timeout = timeout

    def __enter__(self) -> "SandboxClient":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def close(self) -> None:
        """Release the client's resources."""

    def create(
        self,
        id: str,
        *,
        template: Optional[str] = None,
        namespace: Optional[str] = None,
        worker_selector: Optional[Dict[str, str]] = None,
    ) -> "Sandbox":
        """Register a new sandbox with the given ID (a DNS-1123 label) and
        start it.

        ``template`` and ``namespace`` override the client's defaults;
        ``worker_selector`` constrains which worker pools can host the
        sandbox.
        """
        body: dict = {"id": id}
        if template or self._template:
            body["template"] = template or self._template
        if namespace or self._namespace:
            body["namespace"] = namespace or self._namespace
        if worker_selector:
            body["workerSelector"] = worker_selector
        self._do_json("POST", "/v1/sandboxes", body=body)
        return Sandbox(id, self)

    def open(self, id: str) -> "Sandbox":
        """Return a handle to an existing sandbox, verifying it exists."""
        sb = Sandbox(id, self)
        sb.info()
        return sb

    def sandbox(self, id: str) -> "Sandbox":
        """Return a handle to a sandbox by ID without checking that it
        exists."""
        return Sandbox(id, self)

    # --- HTTP plumbing ---------------------------------------------------

    def _do(self, method, path, query=None, data=None, content_type=None):
        url = self._endpoint + path
        if query:
            url += "?" + urllib.parse.urlencode(query)
        req = urllib.request.Request(url, data=data, method=method)
        if content_type:
            req.add_header("Content-Type", content_type)
        try:
            return urllib.request.urlopen(req, timeout=self._timeout)
        except urllib.error.HTTPError as e:
            payload = e.read()
            message = f"API returned HTTP {e.code}"
            code = None
            try:
                envelope = json.loads(payload)
                if envelope.get("error"):
                    message = envelope["error"]
                    code = envelope.get("code")
            except (ValueError, AttributeError):
                pass
            if code == "not_found" or e.code == 404:
                raise NotFoundError(message, e.code) from None
            raise SandboxError(message, code, e.code) from None
        except urllib.error.URLError as e:
            raise SandboxError(f"reaching the API at {self._endpoint!r}: {e.reason}") from None

    def _do_json(self, method, path, query=None, body=None):
        data = None
        content_type = None
        if body is not None:
            data = json.dumps(body).encode()
            content_type = "application/json"
        with self._do(method, path, query, data, content_type) as resp:
            raw = resp.read()
        if not raw:
            return None
        return json.loads(raw)


class Sandbox:
    """A handle to a single sandbox.

    Obtain one from :meth:`SandboxClient.create`, :meth:`SandboxClient.open`,
    or :meth:`SandboxClient.sandbox`.
    """

    def __init__(self, id: str, client: SandboxClient):
        self.id = id
        self._client = client

    def _path(self, suffix: str = "") -> str:
        return "/v1/sandboxes/" + urllib.parse.quote(self.id, safe="") + suffix

    def info(self) -> SandboxInfo:
        """Fetch the sandbox's current state."""
        return _info(self._client._do_json("GET", self._path()))

    def resume(self) -> None:
        """Restore the sandbox from its latest snapshot onto an available
        worker. It is a no-op on the control plane if the sandbox is
        already running."""
        self._client._do_json("POST", self._path("/resume"))

    def suspend(self) -> None:
        """Snapshot the sandbox's full state (memory and filesystem) to
        external storage and free its worker."""
        self._client._do_json("POST", self._path("/suspend"))

    def pause(self) -> None:
        """Snapshot the sandbox but keep the snapshot local to the node for
        faster resume. Unlike suspend, the state does not survive node
        loss."""
        self._client._do_json("POST", self._path("/pause"))

    def delete(self) -> None:
        """Remove the sandbox permanently, suspending it first if it is
        running."""
        self._client._do_json("DELETE", self._path())

    def run(
        self,
        command: List[str],
        *,
        env: Optional[Dict[str, str]] = None,
        cwd: Optional[str] = None,
        stdin: Optional[Union[bytes, str]] = None,
        timeout: Optional[str] = None,
    ) -> CmdResult:
        """Run a command inside the sandbox and return its captured output
        and exit code.

        The command is executed directly (not through a shell); see
        :meth:`cmd` for a shell-friendly shorthand. ``timeout`` is a Go
        duration string, e.g. ``"30s"``.
        """
        body: dict = {"command": command}
        if env:
            body["env"] = env
        if cwd:
            body["cwd"] = cwd
        if stdin is not None:
            if isinstance(stdin, str):
                stdin = stdin.encode()
            body["stdin"] = base64.b64encode(stdin).decode()
        if timeout:
            body["timeout"] = timeout
        return _cmd_result(self._client._do_json("POST", self._path("/cmd"), body=body))

    def cmd(self, command_line: str) -> CmdResult:
        """Run a shell command line ("sh -c") inside the sandbox."""
        return self.run(["sh", "-c", command_line])

    def read_file(self, path: str) -> bytes:
        """Return the contents of the file at ``path`` inside the sandbox."""
        with self._client._do("GET", self._path("/file"), {"path": path}) as resp:
            return resp.read()

    def write_file(self, path: str, data: Union[bytes, str], mode: int = 0o644) -> None:
        """Write ``data`` to the file at ``path`` inside the sandbox with
        the given permissions, creating parent directories as needed."""
        if isinstance(data, str):
            data = data.encode()
        body = {
            "path": path,
            "mode": format(mode & 0o777, "o"),
            "content": base64.b64encode(data).decode(),
        }
        self._client._do_json("POST", self._path("/file"), body=body)

    def list_dir(self, path: str) -> List[DirEntry]:
        """List the entries of the directory at ``path`` inside the
        sandbox."""
        body = self._client._do_json("GET", self._path("/dir"), {"path": path}) or {}
        return [_dir_entry(e) for e in (body.get("entries") or [])]

    def stat(self, path: str) -> DirEntry:
        """Return information about the file or directory at ``path``."""
        return _dir_entry(self._client._do_json("GET", self._path("/stat"), {"path": path}))

    def mkdir(self, path: str, mode: int = 0o755) -> None:
        """Create the directory at ``path``, along with any missing
        parents."""
        body = {"path": path, "mode": format(mode & 0o777, "o")}
        self._client._do_json("POST", self._path("/dir"), body=body)

    def remove(self, path: str) -> None:
        """Delete the file at ``path``. Fails if it is a directory; use
        :meth:`remove_dir` for directories."""
        self._client._do_json("DELETE", self._path("/file"), {"path": path})

    def remove_dir(self, path: str) -> None:
        """Delete the directory tree at ``path``. Fails if it is not a
        directory."""
        self._client._do_json("DELETE", self._path("/dir"), {"path": path})

    def wait_status(self, want: str, *, timeout: float = 300.0, interval: float = 0.25) -> None:
        """Poll until the sandbox reaches the ``want`` status or ``timeout``
        seconds elapse."""
        deadline = time.monotonic() + timeout
        while True:
            status = self.info().status
            if status == want:
                return
            if time.monotonic() >= deadline:
                raise SandboxError(
                    f'waiting for "{self.id}" to become {want}: timed out (last status {status})'
                )
            time.sleep(interval)
