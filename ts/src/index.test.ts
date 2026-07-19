import assert from "node:assert/strict";
import { createServer, type Server } from "node:http";
import { after, before, test } from "node:test";

import { NotFoundError, SandboxClient, type SandboxInfo } from "./index.js";

// A minimal in-memory fake of the substrate-sandbox-api REST service.
const sandboxes = new Map<string, SandboxInfo>();
const files = new Map<string, Buffer>();

let server: Server;
let client: SandboxClient;

function notFound(res: import("node:http").ServerResponse, message: string) {
  res.writeHead(404, { "Content-Type": "application/json" });
  res.end(JSON.stringify({ error: message, code: "not_found" }));
}

before(async () => {
  server = createServer(async (req, res) => {
    const url = new URL(req.url ?? "/", "http://localhost");
    const parts = url.pathname.split("/").filter(Boolean); // ["v1", "sandboxes", id?, op?]
    const id = parts[2];
    const op = parts[3];
    const chunks: Buffer[] = [];
    for await (const chunk of req) {
      chunks.push(chunk as Buffer);
    }
    const body = Buffer.concat(chunks);

    if (req.method === "POST" && parts.length === 2) {
      const create = JSON.parse(body.toString());
      const info: SandboxInfo = {
        id: create.id,
        status: create.start ? "running" : "suspended",
        template: create.template ?? "sandbox",
        namespace: create.namespace ?? "default",
      };
      sandboxes.set(create.id, info);
      res.writeHead(201, { "Content-Type": "application/json" });
      res.end(JSON.stringify(info));
      return;
    }
    if (req.method === "GET" && parts.length === 2) {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ sandboxes: [...sandboxes.values()] }));
      return;
    }

    const info = id === undefined ? undefined : sandboxes.get(id);
    if (!info) {
      notFound(res, `sandbox "${id}" not found`);
      return;
    }

    switch (op) {
      case undefined:
        if (req.method === "DELETE") {
          sandboxes.delete(info.id);
          res.writeHead(204).end();
          return;
        }
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify(info));
        return;
      case "suspend":
      case "pause":
      case "resume": {
        info.status = op === "resume" ? "running" : op === "pause" ? "paused" : "suspended";
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify(info));
        return;
      }
      case "cmd": {
        const cmd = JSON.parse(body.toString());
        info.status = "running"; // auto-resume
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(
          JSON.stringify({
            stdout: `ran: ${cmd.command.join(" ")}`,
            stderr: "",
            exitCode: 0,
            stdin: cmd.stdin,
          }),
        );
        return;
      }
      case "files": {
        const path = url.searchParams.get("path") ?? "";
        if (req.method === "PUT") {
          files.set(path, body);
          res.writeHead(204).end();
          return;
        }
        if (req.method === "DELETE") {
          files.delete(path);
          res.writeHead(204).end();
          return;
        }
        const data = files.get(path);
        if (data === undefined) {
          notFound(res, `${path} not found`);
          return;
        }
        res.writeHead(200, { "Content-Type": "application/octet-stream" });
        res.end(data);
        return;
      }
      default:
        notFound(res, `unhandled ${req.method} ${url.pathname}`);
    }
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  assert(address && typeof address === "object");
  client = new SandboxClient({
    endpoint: `127.0.0.1:${address.port}`, // bare host:port implies http
    template: "default",
    namespace: "sandboxes",
  });
});

after(() => server.close());

test("create applies client defaults and starts the sandbox", async () => {
  const sandbox = await client.create("dev1");
  const info = await sandbox.info();
  assert.equal(info.status, "running");
  assert.equal(info.template, "default");
  assert.equal(info.namespace, "sandboxes");
});

test("create with start: false registers without starting", async () => {
  const sandbox = await client.create("cold", { start: false });
  assert.equal((await sandbox.info()).status, "suspended");
});

test("lifecycle transitions and delete", async () => {
  const sandbox = await client.create("life");
  await sandbox.suspend();
  assert.equal((await sandbox.info()).status, "suspended");
  await sandbox.resume();
  assert.equal((await sandbox.info()).status, "running");
  await sandbox.delete();
  await assert.rejects(sandbox.info(), NotFoundError);
});

test("cmd runs through sh -c and encodes stdin as base64", async () => {
  const sandbox = await client.create("exec");
  const result = await sandbox.cmd("echo hello");
  assert.equal(result.stdout, "ran: sh -c echo hello");
  assert.equal(result.exitCode, 0);

  const withStdin = (await sandbox.run({
    command: ["cat"],
    stdin: "piped in",
  })) as { stdin?: string };
  assert.equal(Buffer.from(withStdin.stdin ?? "", "base64").toString(), "piped in");
});

test("file round-trip and remove", async () => {
  const sandbox = await client.create("fs");
  await sandbox.writeFile("app/note.txt", "file body", { mode: 0o600 });
  const data = await sandbox.readFile("app/note.txt");
  assert.equal(new TextDecoder().decode(data), "file body");
  await sandbox.remove("app/note.txt");
  await assert.rejects(sandbox.readFile("app/note.txt"), NotFoundError);
});

test("list and open", async () => {
  const infos = await client.list();
  assert(infos.length > 0);
  await client.open(infos[0]!.id);
  await assert.rejects(client.open("absent"), NotFoundError);
});

test("waitStatus polls until the status matches", async () => {
  const sandbox = await client.create("waiter");
  await sandbox.suspend();
  const waiting = sandbox.waitStatus("running", { intervalMs: 10, timeoutMs: 2000 });
  await sandbox.resume();
  await waiting;
});
