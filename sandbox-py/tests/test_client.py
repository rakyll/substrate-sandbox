"""Tests for the substrate-sandbox Python SDK.

They run against an in-process fake of the ssbx-api service, so no
cluster is required.
"""

import base64
import json
import posixpath
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

from ssbx import NotFoundError, SandboxClient, SandboxError, Status

# In-memory state of the fake service.
_sandboxes = {}
_files = {}  # path -> bytes
_dirs = set()


class _Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # silence per-request logging
        pass

    def _send(self, status, payload=None, raw=None):
        self.send_response(status)
        if raw is not None:
            self.send_header("Content-Type", "application/octet-stream")
            self.end_headers()
            self.wfile.write(raw)
        elif payload is not None:
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode())
        else:
            self.end_headers()

    def _error(self, status, code, message):
        self._send(status, {"error": message, "code": code})

    def _body(self):
        n = int(self.headers.get("Content-Length", 0))
        return json.loads(self.rfile.read(n)) if n else {}

    def _route(self):
        u = urlsplit(self.path)
        parts = [p for p in u.path.split("/") if p]  # ["v1", "sandboxes", id?, op?]
        query = {k: v[0] for k, v in parse_qs(u.query).items()}
        sid = parts[2] if len(parts) > 2 else None
        op = parts[3] if len(parts) > 3 else None
        return sid, op, query

    @staticmethod
    def _entry(path, *, is_dir, size=0):
        return {
            "name": posixpath.basename(path),
            "path": path,
            "size": size,
            "mode": 0o755 if is_dir else 0o644,
            "modeString": "drwxr-xr-x" if is_dir else "-rw-r--r--",
            "isDir": is_dir,
            "modTime": "2026-01-01T00:00:00Z",
        }

    def do_POST(self):
        sid, op, _ = self._route()
        if sid is None:  # POST /v1/sandboxes
            req = self._body()
            info = {
                "id": req["id"],
                "status": Status.RUNNING,
                "template": req.get("template", "sandbox"),
                "namespace": req.get("namespace", "substrate-sandbox"),
            }
            _sandboxes[req["id"]] = info
            return self._send(201, info)

        info = _sandboxes.get(sid)
        if info is None:
            return self._error(404, "not_found", f'sandbox "{sid}" not found')

        if op in ("suspend", "pause", "resume"):
            info["status"] = {
                "suspend": Status.SUSPENDED,
                "pause": Status.PAUSED,
                "resume": Status.RUNNING,
            }[op]
            return self._send(200, info)
        if op == "cmd":
            req = self._body()
            info["status"] = Status.RUNNING  # the service auto-resumes
            if req["command"][0] == "cat" and req.get("stdin"):
                stdout = base64.b64decode(req["stdin"]).decode()
            else:
                stdout = "ran: " + " ".join(req["command"])
            return self._send(200, {"stdout": stdout, "stderr": "", "exitCode": 0})
        if op == "file":
            req = self._body()
            _files[req["path"]] = base64.b64decode(req.get("content", ""))
            parent = posixpath.dirname(req["path"])
            if parent:  # writes create missing parent directories
                _dirs.add(parent)
            return self._send(204)
        if op == "dir":
            _dirs.add(self._body()["path"])
            return self._send(204)
        return self._error(404, "not_found", f"unhandled POST {self.path}")

    def do_GET(self):
        sid, op, query = self._route()
        info = _sandboxes.get(sid)
        if info is None:
            return self._error(404, "not_found", f'sandbox "{sid}" not found')
        if op is None:
            return self._send(200, info)

        path = query.get("path", "")
        if op == "file":
            data = _files.get(path)
            if data is None:
                return self._error(404, "not_found", f"{path} not found")
            return self._send(200, raw=data)
        if op == "dir":
            if path not in _dirs:
                return self._error(404, "not_found", f"{path} not found")
            entries = [
                self._entry(p, is_dir=False, size=len(b))
                for p, b in sorted(_files.items())
                if posixpath.dirname(p) == path
            ]
            return self._send(200, {"entries": entries})
        if op == "stat":
            if path in _files:
                return self._send(200, self._entry(path, is_dir=False, size=len(_files[path])))
            if path in _dirs:
                return self._send(200, self._entry(path, is_dir=True))
            return self._error(404, "not_found", f"{path} not found")
        return self._error(404, "not_found", f"unhandled GET {self.path}")

    def do_DELETE(self):
        sid, op, query = self._route()
        info = _sandboxes.get(sid)
        if info is None:
            return self._error(404, "not_found", f'sandbox "{sid}" not found')
        if op is None:
            del _sandboxes[sid]
            return self._send(204)

        path = query.get("path", "")
        if op == "file":
            if path in _dirs:
                return self._error(400, "not_file", f"{path} is a directory")
            if path not in _files:
                return self._error(404, "not_found", f"{path} not found")
            del _files[path]
            return self._send(204)
        if op == "dir":
            if path in _files or path not in _dirs:
                return self._error(400, "not_directory", f"{path} is not a directory")
            _dirs.discard(path)
            for p in [p for p in _files if p.startswith(path + "/")]:
                del _files[p]
            return self._send(204)
        return self._error(404, "not_found", f"unhandled DELETE {self.path}")


_server = None
_thread = None
_client = None


def setUpModule():
    global _server, _thread, _client
    _server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    _thread = threading.Thread(target=_server.serve_forever, daemon=True)
    _thread.start()
    host, port = _server.server_address
    # A bare host:port implies http.
    _client = SandboxClient(f"{host}:{port}", template="default", namespace="sandboxes")


def tearDownModule():
    _server.shutdown()
    _thread.join()


class SDKTest(unittest.TestCase):
    def setUp(self):
        _sandboxes.clear()
        _files.clear()
        _dirs.clear()

    def test_create_applies_client_defaults(self):
        sb = _client.create("dev1")
        info = sb.info()
        self.assertEqual(info.status, Status.RUNNING)
        self.assertEqual(info.template, "default")
        self.assertEqual(info.namespace, "sandboxes")

    def test_create_overrides(self):
        sb = _client.create("dev2", template="mini", namespace="other")
        info = sb.info()
        self.assertEqual(info.template, "mini")
        self.assertEqual(info.namespace, "other")

    def test_lifecycle_and_delete(self):
        sb = _client.create("life")
        sb.suspend()
        self.assertEqual(sb.info().status, Status.SUSPENDED)
        sb.resume()
        self.assertEqual(sb.info().status, Status.RUNNING)
        sb.pause()
        self.assertEqual(sb.info().status, Status.PAUSED)
        sb.delete()
        with self.assertRaises(NotFoundError):
            sb.info()

    def test_cmd_and_stdin_encoding(self):
        sb = _client.create("exec")
        res = sb.cmd("echo hello")
        self.assertEqual(res.stdout, "ran: sh -c echo hello")
        self.assertEqual(res.exit_code, 0)
        self.assertEqual(sb.run(["cat"], stdin="piped in").stdout, "piped in")

    def test_auto_resume_on_cmd(self):
        sb = _client.create("wake")
        sb.suspend()
        self.assertEqual(sb.cmd("echo awake").exit_code, 0)
        self.assertEqual(sb.info().status, Status.RUNNING)

    def test_file_roundtrip(self):
        sb = _client.create("fs")
        sb.write_file("app/note.txt", "file body", mode=0o600)
        self.assertEqual(sb.read_file("app/note.txt"), b"file body")

        entries = sb.list_dir("app")
        self.assertEqual([e.name for e in entries], ["note.txt"])
        self.assertFalse(entries[0].is_dir)

        self.assertTrue(sb.stat("app").is_dir)
        self.assertEqual(sb.stat("app/note.txt").size, len("file body"))

    def test_remove_is_file_only(self):
        sb = _client.create("rm")
        sb.mkdir("project")
        sb.write_file("project/a.txt", "a")

        with self.assertRaises(SandboxError) as ctx:
            sb.remove("project")
        self.assertEqual(ctx.exception.code, "not_file")

        with self.assertRaises(SandboxError) as ctx:
            sb.remove_dir("project/a.txt")
        self.assertEqual(ctx.exception.code, "not_directory")

        sb.remove("project/a.txt")
        sb.remove_dir("project")
        with self.assertRaises(NotFoundError):
            sb.stat("project")

    def test_open(self):
        _client.create("present")
        self.assertEqual(_client.open("present").id, "present")
        with self.assertRaises(NotFoundError):
            _client.open("absent")

    def test_wait_status(self):
        sb = _client.create("waiter")
        sb.suspend()
        sb.wait_status(Status.SUSPENDED, timeout=2, interval=0.01)
        with self.assertRaises(SandboxError):
            sb.wait_status(Status.RUNNING, timeout=0.05, interval=0.01)


if __name__ == "__main__":
    unittest.main()
