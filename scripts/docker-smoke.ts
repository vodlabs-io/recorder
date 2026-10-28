// Smoke-tests the recorder image the way compose.yaml runs it: read-only,
// no capabilities, an unusual uid, and pointed away from the VodLabs service
// so nothing registers with it.
//
//   bun scripts/docker-smoke.ts [image]   (default ghcr.io/vodlabs-io/recorder:latest)
import { wsconnect, type NatsConnection } from "@nats-io/nats-core";
import { mkdtempSync, chmodSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const image = process.argv[2] ?? "ghcr.io/vodlabs-io/recorder:latest";
const name = `vlrec-smoke-${process.pid}`;
const port = 18000 + (process.pid % 1000);
const base = `http://127.0.0.1:${port}/`;
const origin = `http://127.0.0.1:${port}`;

const root = mkdtempSync(join(tmpdir(), "vlrec-smoke-"));
const configDir = join(root, "config");
const recDir = join(root, "recordings");
for (const d of [configDir, recDir]) {
  Bun.spawnSync(["mkdir", "-p", d]);
  // Any uid may write: the container runs as one the host doesn't know.
  chmodSync(d, 0o777);
}
const cacheVolume = `${name}-cache`;

function sh(args: string[], check = true): string {
  const r = Bun.spawnSync(args, { stderr: "pipe", stdout: "pipe" });
  const out = r.stdout.toString() + r.stderr.toString();
  if (check && r.exitCode !== 0) throw new Error(`${args.join(" ")} failed:\n${out}`);
  return r.stdout.toString().trim();
}

function run(hostname: string): void {
  sh([
    "docker", "run", "-d", "--name", name, "--hostname", hostname,
    "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
    "--user", "4242:4242",
    "-p", `127.0.0.1:${port}:8080`,
    "-e", "VLREC_CLOUD_API_URL=http://127.0.0.1:9",
    "-e", "VLREC_CLOUD_NATS_URL=ws://127.0.0.1:9",
    "-v", `${configDir}:/config`,
    "-v", `${recDir}:/recordings`,
    "-v", `${cacheVolume}:/cache`,
    image,
  ]);
}

let failures = 0;
function check(what: string, ok: boolean, detail = ""): void {
  console.log(`${ok ? "ok  " : "FAIL"} ${what}${ok || !detail ? "" : `: ${detail}`}`);
  if (!ok) failures++;
}

async function waitHealthy(): Promise<void> {
  for (let i = 0; i < 90; i++) {
    const s = sh(["docker", "inspect", "-f", "{{.State.Health.Status}} {{.State.Running}}", name], false);
    if (s.startsWith("healthy")) return;
    if (s.endsWith("false")) break;
    await Bun.sleep(1000);
  }
  throw new Error(`container never turned healthy:\n${sh(["docker", "logs", name], false)}`);
}

// Read through the container: the token file is 0600 and owned by its uid.
function token(): string {
  return sh(["docker", "exec", name, "vod", "web", "token"]);
}

async function signIn(tok: string, hdrs: Record<string, string> = {}): Promise<Response> {
  return fetch(base + "api/session", {
    method: "POST",
    headers: { "Content-Type": "application/json", Origin: origin, ...hdrs },
    body: JSON.stringify({ token: tok }),
    redirect: "manual",
  });
}

function cookieOf(res: Response): string {
  return (res.headers.get("set-cookie") ?? "").split(";")[0];
}

// wsStatus opens the bridge socket and reports the handshake's HTTP status.
async function wsStatus(cookie: string, wsOrigin = origin): Promise<number> {
  const res = await fetch(base + "nats", {
    headers: {
      Connection: "Upgrade",
      Upgrade: "websocket",
      "Sec-WebSocket-Version": "13",
      "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==",
      Origin: wsOrigin,
      ...(cookie ? { Cookie: cookie } : {}),
    },
  });
  return res.status;
}

async function connect(cookie: string): Promise<NatsConnection> {
  return wsconnect({
    servers: `ws://127.0.0.1:${port}/nats`,
    reconnect: false,
    wsFactory: async (u: string) => ({
      // Bun's WebSocket takes headers, which a browser sets on its own.
      socket: new WebSocket(u, { headers: { Cookie: cookie, Origin: origin } } as never),
      encrypted: false,
    }),
  });
}

async function api<T>(nc: NatsConnection, subject: string, body: unknown = {}): Promise<T> {
  const m = await nc.request(subject, JSON.stringify(body), { timeout: 30_000 });
  const res = m.json<{ ok: boolean; data?: T; error?: { message: string } }>();
  if (!res.ok) throw new Error(`${subject}: ${res.error?.message}`);
  return res.data as T;
}

try {
  run("smoke-a");
  await waitHealthy();
  check("container turned healthy", true);
  const logs = sh(["docker", "logs", name], false);
  check("logs never show the token", !logs.includes(token()));

  const hz = await fetch(base + "healthz");
  check("healthz answers ok", hz.status === 200 && (await hz.text()) === "ok");
  for (const h of ["content-security-policy", "x-frame-options", "x-content-type-options", "referrer-policy"]) {
    check(`healthz carries ${h}`, hz.headers.has(h));
  }

  const root = await fetch(base, { redirect: "manual" });
  check("/ without a session redirects to signin", root.status === 303 && root.headers.get("location") === "signin");
  check("socket without a cookie is refused", (await wsStatus("")) === 401);
  check("wrong token is refused", (await signIn("x".repeat(43))).status === 401);
  const xo = await signIn(token(), { Origin: "http://evil.example", "Sec-Fetch-Site": "cross-site" });
  check("cross-origin sign-in is refused", xo.status === 403, String(xo.status));
  const ok = await signIn(token());
  check("right token signs in", ok.status === 204, String(ok.status));
  const cookie = cookieOf(ok);

  check("cross-origin socket is refused", (await wsStatus(cookie, "http://evil.example")) === 403);
  const passwd = await fetch(base + "api/vods/file?dir=/etc&file=passwd", { headers: { Cookie: cookie } });
  check("a file outside the recordings is not served", passwd.status === 404, String(passwd.status));

  const nc = await connect(cookie);
  const info = await api<{ container: boolean }>(nc, "info");
  check("the bridge reaches the API, which knows it's in a container", info.container === true);
  const encoders = await api<{ encoders: { id: string; working: boolean }[] }>(nc, "encode.encoders", { refresh: true });
  check(
    "ffmpeg runs on the image",
    encoders.encoders.some((e) => e.id === "libx264" && e.working),
    JSON.stringify(encoders),
  );
  await api(nc, "platformlogin.set", { platform: "tiktok", cookie_header: "sessionid=smoke" });
  await nc.close();

  // Sign-out revokes the cookie it was given.
  const out = await fetch(base + "api/signout", { method: "POST", headers: { Cookie: cookie, Origin: origin } });
  check("sign-out", out.status === 204);
  const after = await fetch(base + "api/session", { headers: { Cookie: cookie } });
  check("a signed-out cookie is refused", after.status === 401);

  // Rotation signs everyone out.
  const second = cookieOf(await signIn(token()));
  sh(["docker", "exec", name, "vod", "web", "token", "--rotate"]);
  await Bun.sleep(4000);
  const rotated = await fetch(base + "api/session", { headers: { Cookie: second } });
  check("a cookie from before a rotation is refused", rotated.status === 401);

  // A recreated container gets a new hostname; stored logins must survive it.
  sh(["docker", "rm", "-f", name]);
  run("smoke-b");
  await waitHealthy();
  const nc2 = await connect(cookieOf(await signIn(token())));
  const logins = await api<{ platform: string }[]>(nc2, "platformlogin.list");
  check("a stored login survives a new hostname", logins.some((l) => l.platform === "tiktok"), JSON.stringify(logins));
  await nc2.close();

  const t0 = Date.now();
  sh(["docker", "stop", "-t", "60", name]);
  const exit = sh(["docker", "inspect", "-f", "{{.State.ExitCode}}", name]);
  check("docker stop exits cleanly", exit === "0" && Date.now() - t0 < 30_000, `exit ${exit} after ${Date.now() - t0} ms`);
} catch (e) {
  failures++;
  console.error("FAIL", e instanceof Error ? e.message : e);
  console.error(sh(["docker", "logs", name], false));
} finally {
  sh(["docker", "rm", "-f", name], false);
  sh(["docker", "volume", "rm", "-f", cacheVolume], false);
  try {
    rmSync(root, { recursive: true, force: true });
  } catch (e) {
    // Folders the container made are its uid's, which the host may not remove.
    console.warn(`left ${root}: ${e instanceof Error ? e.message : e}`);
  }
}

if (failures > 0) {
  console.error(`${failures} check(s) failed`);
  process.exit(1);
}
console.log("all checks passed");
