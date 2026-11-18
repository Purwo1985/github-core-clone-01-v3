// Entrypoint: the forge itself is a statically linked Go binary
// (`./forge`, committed as vendored source). This launcher starts it,
// reads the ephemeral port it binds, registers that port with the trial
// environment, then long-polls /done until the trial ends and shuts the
// binary down cleanly.
//
// Environment (set by the trial harness):
//   MAKEFUN_ENV_URL    base URL of the environment service (required)
//   MAKEFUN_SHOP_TOKEN the bearer token clients must present (empty = dev mode)
//   MAKEFUN_TRIAL_NOW  the deterministic "now" in epoch seconds

import { spawn, type ChildProcess } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = process.env.MAKEFUN_ENV_URL ?? '';

if (ENV_URL === '') {
  console.error('MAKEFUN_ENV_URL is not set');
  process.exit(1);
}

const BINARY = join(dirname(fileURLToPath(import.meta.url)), 'forge');

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function postJson(url: string, body: unknown): Promise<number> {
  const response = await fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
  // Drain the body so the connection can be reused or closed cleanly.
  await response.text();
  return response.status;
}

async function getJson(url: string): Promise<{ status: number; body: unknown }> {
  const response = await fetch(url, { method: 'GET' });
  const text = await response.text();
  let body: unknown = null;
  try {
    body = JSON.parse(text) as unknown;
  } catch {
    body = null;
  }
  return { status: response.status, body };
}

async function register(port: number): Promise<void> {
  const url = `${ENV_URL.replace(/\/$/, '')}/register`;
  let delayMs = 250;
  for (;;) {
    let status: number;
    try {
      status = await postJson(url, { port });
    } catch {
      status = 0;
    }
    if (status === 200 || status === 409) return;
    if (status === 400 || status === 405) {
      console.error(`register rejected with HTTP ${status}; giving up`);
      process.exit(1);
    }
    await sleep(delayMs);
    delayMs = Math.min(delayMs * 2, 2000);
  }
}

async function waitForDone(): Promise<void> {
  const url = `${ENV_URL.replace(/\/$/, '')}/done`;
  for (;;) {
    try {
      const { status, body } = await getJson(url);
      if (status === 200 && body !== null && typeof body === 'object' && (body as Record<string, unknown>).done === true) {
        return;
      }
    } catch {
      // The environment service may restart; keep polling.
    }
    await sleep(1000);
  }
}

function startForge(): Promise<{ child: ChildProcess; port: number }> {
  return new Promise((resolve, reject) => {
    const child = spawn(BINARY, [], {
      stdio: ['ignore', 'pipe', 'inherit'],
      env: process.env,
    });
    let buffer = '';
    let settled = false;
    child.stdout?.on('data', (chunk: Buffer) => {
      buffer += chunk.toString('utf8');
      const match = /READY (\d+)/.exec(buffer);
      if (match && !settled) {
        settled = true;
        resolve({ child, port: Number.parseInt(match[1], 10) });
      }
    });
    child.once('error', (err) => {
      if (!settled) {
        settled = true;
        reject(err);
      }
    });
    child.once('exit', (code) => {
      if (!settled) {
        settled = true;
        reject(new Error(`forge exited early with code ${code}`));
      }
    });
  });
}

async function main(): Promise<void> {
  const { child, port } = await startForge();
  await register(port);
  await waitForDone();
  child.kill('SIGTERM');
  await new Promise<void>((resolve) => {
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      resolve();
    }, 2000);
    child.once('exit', () => {
      clearTimeout(timer);
      resolve();
    });
  });
  process.exit(0);
}

main().catch((err: unknown) => {
  console.error(err instanceof Error ? err.message : String(err));
  process.exit(1);
});
