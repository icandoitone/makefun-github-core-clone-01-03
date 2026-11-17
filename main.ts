// Forge v3 launcher: starts the native Go core, registers its port with the
// trial environment, and waits until the trial reports it is done.
import { spawn, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";
import path from "node:path";

const dir = path.dirname(fileURLToPath(import.meta.url));
const binary = path.join(dir, "forge");
const envUrl = process.env.MAKEFUN_ENV_URL;

const child: ChildProcess = spawn(binary, [], {
  stdio: ["ignore", "pipe", "inherit"],
  env: process.env,
});

let shuttingDown = false;

function shutdown(code: number): void {
  if (shuttingDown) return;
  shuttingDown = true;
  try {
    child.kill("SIGTERM");
  } catch {
    // child already gone
  }
  process.exit(code);
}

process.on("SIGTERM", () => shutdown(0));
process.on("SIGINT", () => shutdown(0));

child.on("exit", (code) => {
  if (!shuttingDown) process.exit(code ?? 1);
});

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function registerAndWait(port: number): Promise<void> {
  if (!envUrl) return; // local development: just keep the core running
  const base = envUrl.replace(/\/$/, "");
  for (let attempt = 0; attempt < 60; attempt++) {
    try {
      const res = await fetch(`${base}/register`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ port }),
      });
      if (res.ok || res.status === 409) break;
    } catch {
      // environment not reachable yet; retry
    }
    await sleep(500);
  }
  for (;;) {
    try {
      const res = await fetch(`${base}/done`);
      const data = (await res.json()) as { done?: boolean };
      if (data?.done === true) break;
    } catch {
      // transient; keep polling
    }
    await sleep(500);
  }
  shutdown(0);
}

let announced = false;
child.stdout?.on("data", (chunk: Buffer) => {
  const text = chunk.toString("utf8");
  const match = text.match(/ready (\d+)/);
  if (match && !announced) {
    announced = true;
    const port = parseInt(match[1], 10);
    void registerAndWait(port);
  }
});
