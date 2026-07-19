# TypeScript SDK

TypeScript SDK for [substrate-sandbox](https://github.com/rakyll/substrate-sandbox):
isolated, stateful execution environments on
[Agent Substrate](https://github.com/agent-substrate/substrate) that can be
suspended, resumed on any available worker, and driven remotely with command
execution and filesystem operations.

## Usage

```ts
import { SandboxClient } from "substrate-sandbox";

const client = new SandboxClient({
  endpoint: "http://localhost:7777", // substrate-sandbox-api
  template: "sandbox",               // ActorTemplate name
});

const sandbox = await client.create("dev1");
await sandbox.writeFile("/workspace/note.txt", "hello");

const result = await sandbox.cmd("cat /workspace/note.txt");
console.log(result.stdout, result.exitCode);

await sandbox.suspend(); // snapshot to object storage, free the worker
await sandbox.cmd("uname -a"); // auto-resumes transparently

await sandbox.delete();
```

Errors carry the API's machine-readable code; missing sandboxes and files
throw `NotFoundError`:

```ts
import { NotFoundError } from "substrate-sandbox";

try {
  await client.open("absent");
} catch (err) {
  if (err instanceof NotFoundError) {
    // handle missing sandbox
  }
}
```

## Development

```bash
npm install
npm test     # builds and runs the test suite against a fake API server
```
