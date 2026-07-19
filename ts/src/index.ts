/**
 * TypeScript SDK for the substrate-sandbox service. It talks to the
 * substrate-sandbox-api REST service, which bridges to the Agent Substrate
 * control plane and router; `sbcli deploy` runs that service in-cluster.
 */

/** The lifecycle state of a sandbox. */
export type SandboxStatus =
  | "unknown"
  | "resuming"
  | "running"
  | "suspending"
  | "suspended"
  | "pausing"
  | "paused"
  | "crashed";

/** A point-in-time snapshot of a sandbox's state. */
export interface SandboxInfo {
  id: string;
  status: SandboxStatus;
  /** Name of the ActorTemplate the sandbox was created from. */
  template?: string;
  /** Kubernetes namespace the ActorTemplate lives in. */
  namespace?: string;
  /** Pod currently hosting the sandbox, when running. */
  workerPod?: string;
  workerPodNamespace?: string;
  workerPodIP?: string;
}

/** A command to run inside a sandbox. */
export interface CmdRequest {
  /**
   * Argv of the process to run. It is executed directly, not through a
   * shell; use ["sh", "-c", "..."] for shell syntax.
   */
  command: string[];
  /** Additional environment variables set for the process. */
  env?: Record<string, string>;
  /** Working directory. Defaults to the guest's working directory. */
  cwd?: string;
  /** Data fed to the process's standard input. */
  stdin?: Uint8Array | string;
  /** Execution timeout as a Go duration string, e.g. "30s". */
  timeout?: string;
}

/** The outcome of a command. */
export interface CmdResult {
  stdout: string;
  stderr: string;
  /** -1 if the process was killed by a signal or failed to start. */
  exitCode: number;
  /** Whether the command was killed for exceeding the timeout. */
  timedOut?: boolean;
  /** Whether output exceeded the per-stream cap and was cut off. */
  stdoutTruncated?: boolean;
  stderrTruncated?: boolean;
  /** How long the command ran, as a Go duration string. */
  duration?: string;
}

/** A file or directory inside a sandbox. */
export interface DirEntry {
  name: string;
  path: string;
  size: number;
  mode: number;
  modeString: string;
  isDir: boolean;
  /** Modification time (RFC 3339). */
  modTime: string;
}

/** Configures a {@link SandboxClient}. */
export interface ClientOptions {
  /**
   * Base URL of the substrate-sandbox-api REST service, e.g.
   * "http://localhost:7777" (typically a port-forward of
   * svc/substrate-sandbox-api). A bare host:port implies http.
   */
  endpoint: string;
  /** Default ActorTemplate name for create. */
  template?: string;
  /** Kubernetes namespace the ActorTemplates live in. */
  namespace?: string;
  /** Overrides the fetch implementation used for API traffic. */
  fetch?: typeof fetch;
}

/** Customizes {@link SandboxClient.create}. */
export interface CreateOptions {
  /** Overrides the client's default ActorTemplate name. */
  template?: string;
  /** Overrides the ActorTemplate's Kubernetes namespace. */
  namespace?: string;
  /** Constrains which worker pools can host the sandbox. */
  workerSelector?: Record<string, string>;
  /**
   * Whether the sandbox starts immediately. Defaults to true; when false,
   * the sandbox starts on its first resume.
   */
  start?: boolean;
}

/** An error returned by the sandbox API. */
export class SandboxError extends Error {
  /** Machine-readable error code, e.g. "not_found". */
  readonly code?: string;
  /** HTTP status of the response. */
  readonly status?: number;

  constructor(message: string, code?: string, status?: number) {
    super(message);
    this.name = "SandboxError";
    this.code = code;
    this.status = status;
  }
}

/** The sandbox, file, or directory does not exist. */
export class NotFoundError extends SandboxError {
  constructor(message: string, status?: number) {
    super(message, "not_found", status);
    this.name = "NotFoundError";
  }
}

function toBase64(data: Uint8Array): string {
  if (typeof Buffer !== "undefined") {
    return Buffer.from(data).toString("base64");
  }
  let binary = "";
  for (const b of data) {
    binary += String.fromCharCode(b);
  }
  return btoa(binary);
}

/** Manages sandboxes through the substrate-sandbox-api service. */
export class SandboxClient {
  private readonly endpoint: string;
  private readonly options: ClientOptions;
  private readonly fetchImpl: typeof fetch;

  constructor(options: ClientOptions) {
    if (!options.endpoint) {
      throw new SandboxError("endpoint is required");
    }
    let endpoint = options.endpoint;
    if (!endpoint.includes("://")) {
      endpoint = `http://${endpoint}`;
    }
    this.endpoint = endpoint.replace(/\/+$/, "");
    this.options = options;
    this.fetchImpl = options.fetch ?? fetch;
  }

  /**
   * Registers a new sandbox with the given ID (a DNS-1123 label) and,
   * unless `start: false` is given, starts it.
   */
  async create(id: string, options: CreateOptions = {}): Promise<Sandbox> {
    await this.doJSON("POST", "/v1/sandboxes", undefined, {
      id,
      template: options.template ?? this.options.template,
      namespace: options.namespace ?? this.options.namespace,
      workerSelector: options.workerSelector,
      start: options.start ?? true,
    });
    return new Sandbox(id, this);
  }

  /** Returns a handle to an existing sandbox, verifying it exists. */
  async open(id: string): Promise<Sandbox> {
    const sandbox = new Sandbox(id, this);
    await sandbox.info();
    return sandbox;
  }

  /** Returns a handle to a sandbox by ID without checking that it exists. */
  sandbox(id: string): Sandbox {
    return new Sandbox(id, this);
  }

  /** Returns information about all sandboxes known to the service. */
  async list(): Promise<SandboxInfo[]> {
    const body = (await this.doJSON("GET", "/v1/sandboxes")) as {
      sandboxes?: SandboxInfo[];
    };
    return body.sandboxes ?? [];
  }

  /**
   * Performs an HTTP request against the API service. Non-2xx responses
   * are converted to errors.
   * @internal
   */
  async do(
    method: string,
    path: string,
    query?: Record<string, string>,
    contentType?: string,
    body?: BodyInit,
  ): Promise<Response> {
    let url = this.endpoint + path;
    if (query) {
      url += `?${new URLSearchParams(query)}`;
    }
    const response = await this.fetchImpl(url, {
      method,
      headers: contentType ? { "Content-Type": contentType } : undefined,
      body,
    });
    if (response.ok) {
      return response;
    }

    let message = `API returned HTTP ${response.status}`;
    let code: string | undefined;
    try {
      const payload = (await response.json()) as { error?: string; code?: string };
      if (payload.error) {
        message = payload.error;
        code = payload.code;
      }
    } catch {
      // Not a JSON error envelope; keep the generic message.
    }
    if (code === "not_found" || response.status === 404) {
      throw new NotFoundError(message, response.status);
    }
    throw new SandboxError(message, code, response.status);
  }

  /**
   * Performs a request with an optional JSON body and decodes the JSON
   * response.
   * @internal
   */
  async doJSON(
    method: string,
    path: string,
    query?: Record<string, string>,
    body?: unknown,
  ): Promise<unknown> {
    const response = await this.do(
      method,
      path,
      query,
      body === undefined ? undefined : "application/json",
      body === undefined ? undefined : JSON.stringify(body),
    );
    if (response.status === 204) {
      return undefined;
    }
    return response.json();
  }
}

/** A handle to a single sandbox. */
export class Sandbox {
  /** The sandbox's identifier. */
  readonly id: string;
  private readonly client: SandboxClient;

  /** @internal Use {@link SandboxClient.create}, `open`, or `sandbox`. */
  constructor(id: string, client: SandboxClient) {
    this.id = id;
    this.client = client;
  }

  private path(suffix = ""): string {
    return `/v1/sandboxes/${encodeURIComponent(this.id)}${suffix}`;
  }

  /** Fetches the sandbox's current state. */
  async info(): Promise<SandboxInfo> {
    return (await this.client.doJSON("GET", this.path())) as SandboxInfo;
  }

  /**
   * Restores the sandbox from its latest snapshot onto an available
   * worker. It is a no-op if the sandbox is already running.
   */
  async resume(): Promise<void> {
    await this.client.doJSON("POST", this.path("/resume"));
  }

  /**
   * Snapshots the sandbox's full state (memory and filesystem) to
   * external storage and frees its worker. The sandbox can later be
   * resumed on any eligible worker.
   */
  async suspend(): Promise<void> {
    await this.client.doJSON("POST", this.path("/suspend"));
  }

  /**
   * Snapshots the sandbox but keeps the snapshot local to the node for
   * faster resume. Unlike suspend, the state does not survive node loss.
   */
  async pause(): Promise<void> {
    await this.client.doJSON("POST", this.path("/pause"));
  }

  /** Removes the sandbox permanently, suspending it first if running. */
  async delete(): Promise<void> {
    await this.client.doJSON("DELETE", this.path());
  }

  /**
   * Runs a command inside the sandbox and returns its captured output and
   * exit code. The command is executed directly (not through a shell);
   * see {@link cmd} for a shell-friendly shorthand.
   */
  async run(request: CmdRequest): Promise<CmdResult> {
    const { stdin, ...rest } = request;
    const body = {
      ...rest,
      stdin:
        stdin === undefined
          ? undefined
          : toBase64(typeof stdin === "string" ? new TextEncoder().encode(stdin) : stdin),
    };
    return (await this.client.doJSON("POST", this.path("/cmd"), undefined, body)) as CmdResult;
  }

  /** Runs a shell command line ("sh -c") inside the sandbox. */
  async cmd(commandLine: string): Promise<CmdResult> {
    return this.run({ command: ["sh", "-c", commandLine] });
  }

  /** Returns the contents of the file at path inside the sandbox. */
  async readFile(path: string): Promise<Uint8Array> {
    const response = await this.client.do(
      "POST",
      this.path("/fs/read"),
      undefined,
      "application/json",
      JSON.stringify({ path }),
    );
    return new Uint8Array(await response.arrayBuffer());
  }

  /**
   * Writes data to the file at path inside the sandbox, creating parent
   * directories as needed.
   */
  async writeFile(
    path: string,
    data: Uint8Array | string | Blob,
    options: { mode?: number } = {},
  ): Promise<void> {
    let bytes: Uint8Array;
    if (typeof data === "string") {
      bytes = new TextEncoder().encode(data);
    } else if (data instanceof Blob) {
      bytes = new Uint8Array(await data.arrayBuffer());
    } else {
      bytes = data;
    }
    await this.client.doJSON("POST", this.path("/fs/write"), undefined, {
      path,
      mode: (options.mode ?? 0o644).toString(8),
      content: toBase64(bytes),
    });
  }

  /** Lists the entries of the directory at path inside the sandbox. */
  async listDir(path: string): Promise<DirEntry[]> {
    const body = (await this.client.doJSON("POST", this.path("/fs/ls"), undefined, {
      path,
    })) as { entries?: DirEntry[] };
    return body.entries ?? [];
  }

  /** Returns information about the file or directory at path. */
  async stat(path: string): Promise<DirEntry> {
    return (await this.client.doJSON("POST", this.path("/fs/stat"), undefined, {
      path,
    })) as DirEntry;
  }

  /** Creates the directory at path, along with any missing parents. */
  async mkdir(path: string, options: { mode?: number } = {}): Promise<void> {
    await this.client.doJSON("POST", this.path("/fs/mkdir"), undefined, {
      path,
      mode: (options.mode ?? 0o755).toString(8),
    });
  }

  /** Deletes the file or directory tree at path. */
  async remove(path: string): Promise<void> {
    await this.client.doJSON("POST", this.path("/fs/rm"), undefined, { path });
  }

  /** Polls until the sandbox reaches the given status. */
  async waitStatus(
    want: SandboxStatus,
    options: { timeoutMs?: number; intervalMs?: number } = {},
  ): Promise<void> {
    const deadline = Date.now() + (options.timeoutMs ?? 5 * 60 * 1000);
    const interval = options.intervalMs ?? 250;
    for (;;) {
      const { status } = await this.info();
      if (status === want) {
        return;
      }
      if (Date.now() >= deadline) {
        throw new SandboxError(
          `waiting for "${this.id}" to become ${want}: timed out (last status ${status})`,
        );
      }
      await new Promise((resolve) => setTimeout(resolve, interval));
    }
  }
}
