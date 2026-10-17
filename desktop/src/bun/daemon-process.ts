import {
  chmodSync,
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readdirSync,
  renameSync,
  rmSync,
} from "fs";
import { homedir } from "os";
import { basename, join } from "path";

// DEFAULT_BIN_REPO is the GitHub repo that publishes the daemon releases (the
// same source the CLI/daemon download from — see internal/vlconfig default
// bin_repo). Overridable via VLREC_BIN_REPO for testing against a fork.
export const DEFAULT_BIN_REPO = "https://github.com/vodlabs-io/recorder";

// goPlatform mirrors Go's runtime.GOOS_GOARCH, which is how release archives and
// the cache directory are named (see internal/vlupdate). node's process.platform
// /process.arch use different spellings, so map them.
function goOS(): string {
  switch (process.platform) {
    case "win32":
      return "windows";
    case "darwin":
      return "darwin";
    default:
      return "linux";
  }
}

function goArch(): string {
  switch (process.arch) {
    case "x64":
      return "amd64";
    case "arm64":
      return "arm64";
    default:
      return process.arch;
  }
}

function goPlatform(): string {
  return `${goOS()}_${goArch()}`;
}

function binName(): string {
  return process.platform === "win32" ? "vlrecd.exe" : "vlrecd";
}

// userCacheDir matches Go's os.UserCacheDir so a daemon downloaded by the CLI is
// reused by the desktop app (and vice versa).
function userCacheDir(): string {
  const home = homedir();
  switch (process.platform) {
    case "darwin":
      return join(home, "Library", "Caches");
    case "win32":
      return process.env.LOCALAPPDATA || join(home, "AppData", "Local");
    default:
      return process.env.XDG_CACHE_HOME || join(home, ".cache");
  }
}

// daemonLogDir is where the daemon writes its own logs (matching vllog:
// $VLREC_CACHE_DIR/logs, else <UserCacheDir>/vodlabs/recorder/logs), so its
// stderr lands beside them.
export function daemonLogDir(): string {
  const override = process.env.VLREC_CACHE_DIR?.trim();
  if (override) return join(override, "logs");
  return join(userCacheDir(), "vodlabs", "recorder", "logs");
}

// The daemon's stderr, and the run before it. Stderr is the only place a Go
// runtime failure is ever written: a panic, or a fatal error such as an
// unlock of an unlocked mutex, bypasses the daemon's own file logger entirely
// — that logger dies with the process. A daemon that vanishes leaves nothing
// else behind, so this stream is kept rather than discarded.
const STDERR_LOG = "vlrecd-stderr.log";
const STDERR_LOG_PREV = "vlrecd-stderr.prev.log";

// daemonCacheRoot is <UserCacheDir>/vodlabs/recorder/daemon; per-version binaries
// live under <root>/<version>/<os>_<arch>/ (matching vlupdate.CacheDir).
function daemonCacheRoot(): string {
  return join(userCacheDir(), "vodlabs", "recorder", "daemon");
}

function daemonCacheDir(version: string): string {
  return join(daemonCacheRoot(), version, goPlatform());
}

// compareVersionsDesc orders semver-ish version strings newest-first, ignoring
// any pre-release/build suffix. Used to pick the newest cached daemon.
export function compareVersionsDesc(a: string, b: string): number {
  const parse = (v: string) =>
    v.replace(/^v/, "").split(/[-+]/)[0].split(".").map((n) => parseInt(n, 10) || 0);
  const pa = parse(a);
  const pb = parse(b);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const diff = (pb[i] ?? 0) - (pa[i] ?? 0);
    if (diff !== 0) return diff;
  }
  return 0;
}

// VERSION_PROBE_MS bounds `<binary> --version`; a binary that can't answer that
// quickly isn't one we want to launch and wait 15s on anyway.
const VERSION_PROBE_MS = 3000;

// probeVersion runs `<exePath> --version` and returns the version it prints, or
// null if the binary is missing, not executable, or doesn't answer in time.
// Mirrors Go's vlupdate.checkVersion, which is what keeps the CLI from launching
// a stale daemon off PATH.
async function probeVersion(exePath: string): Promise<string | null> {
  try {
    const proc = Bun.spawn([exePath, "--version"], {
      stdout: "pipe",
      stderr: "ignore",
      stdin: "ignore",
    });
    const timer = setTimeout(() => proc.kill(), VERSION_PROBE_MS);
    try {
      const out = await new Response(proc.stdout).text();
      if ((await proc.exited) !== 0) return null;
      // Accept "0.5.2", "v0.5.2" or "vlrecd version 0.5.2" — take the first
      // dotted-numeric token on the first line.
      const match = out.split("\n")[0].match(/v?(\d+\.\d+(?:\.\d+)?[\w.+-]*)/);
      return match ? match[1] : null;
    } finally {
      clearTimeout(timer);
    }
  } catch {
    return null; // not executable / spawn failed
  }
}

// newestCachedBinary returns the path to the newest previously-downloaded daemon,
// or null. This is offline-friendly: once a binary is cached the app starts with
// no network.
function newestCachedBinary(): string | null {
  let versions: string[];
  try {
    versions = readdirSync(daemonCacheRoot());
  } catch {
    return null; // cache root doesn't exist yet
  }
  for (const v of versions.sort(compareVersionsDesc)) {
    const bin = join(daemonCacheRoot(), v, goPlatform(), binName());
    if (existsSync(bin)) return bin;
  }
  return null;
}

/**
 * locateDaemonBinary resolves an already-present daemon executable WITHOUT any
 * network access:
 *   1. VLREC_DAEMON_PATH / VLREC_DAEMON_BIN wins outright (a developer pointing
 *      at a locally built daemon; second-guessing it would defeat the point)
 *   2. otherwise `vlrecd` on PATH and the newest cached download are both probed
 *      with `--version` and the NEWER one is launched
 * Returns null when nothing is found; the caller then falls back to
 * downloadDaemon().
 *
 * The version probe matters: a CLI user has vlrecd on PATH, and preferring it
 * unconditionally would keep launching a stale binary over a newer cached
 * download. Go's vlupdate.Locate version-checks each candidate the same way.
 */
export async function locateDaemonBinary(): Promise<string | null> {
  for (const p of [process.env.VLREC_DAEMON_PATH, process.env.VLREC_DAEMON_BIN]) {
    if (p && existsSync(p)) return p;
  }

  const candidates: string[] = [];
  const onPath = Bun.which("vlrecd");
  if (onPath) candidates.push(onPath);
  const cached = newestCachedBinary();
  if (cached) candidates.push(cached);
  // Nothing to weigh up: no probe, no subprocess, no delay to startup.
  if (candidates.length < 2) return candidates[0] ?? null;

  const probed = await Promise.all(
    candidates.map(async (path) => ({ path, version: await probeVersion(path) })),
  );
  // A candidate that won't report its version won't run either; fall back to the
  // preference order only if none of them answered.
  const usable = probed.filter((c): c is { path: string; version: string } =>
    c.version !== null,
  );
  if (usable.length === 0) return candidates[0];
  usable.sort((a, b) => compareVersionsDesc(a.version, b.version));
  return usable[0].path;
}

/**
 * latestDaemonVersion returns the newest release tag for repo (without a leading
 * "v"). It reads the Location header of <repo>/releases/latest, which GitHub
 * serves as a 302 to /releases/tag/v<X.Y.Z> — no API token required. Mirrors
 * vlupdate.LatestVersion.
 */
export async function latestDaemonVersion(repo: string): Promise<string> {
  const url = `${repo.replace(/\/+$/, "")}/releases/latest`;
  const res = await fetch(url, { redirect: "manual" });
  // redirect:"manual" exposes the 3xx Location in Bun; if a runtime followed it
  // instead, res.url is the final tag URL — basename works for either.
  const loc = res.headers.get("location") || res.url;
  if (!loc) {
    throw new Error(`no redirect target from ${url} (HTTP ${res.status})`);
  }
  const tag = basename(loc);
  if (!tag || tag === "/" || tag === ".") {
    throw new Error(`malformed redirect target ${loc}`);
  }
  return tag.replace(/^v/, "");
}

/**
 * downloadDaemon fetches the daemon archive for the given version from repo,
 * verifies its SHA256 against the published checksums.txt, extracts the binary,
 * and installs it into the shared cache dir. Returns the installed path. Mirrors
 * vlupdate.Download. onProgress (0–100) reports download percent when the server
 * sends a Content-Length.
 */
export async function downloadDaemon(
  repo: string,
  version: string,
  onProgress?: (percent: number) => void,
): Promise<string> {
  const isWin = process.platform === "win32";
  const ext = isWin ? "zip" : "tar.gz";
  const archiveName = `daemon_${goPlatform()}.${ext}`;
  const tag = version.startsWith("v") ? version : `v${version}`;
  const base = `${repo.replace(/\/+$/, "")}/releases/download/${tag}`;

  const expectedSum = await fetchExpectedSum(`${base}/checksums.txt`, archiveName);

  const destDir = daemonCacheDir(version);
  mkdirSync(destDir, { recursive: true });

  const archivePath = join(destDir, `${archiveName}.tmp`);
  const extractDir = join(destDir, ".extract");
  try {
    await downloadAndVerify(`${base}/${archiveName}`, archivePath, expectedSum, onProgress);

    rmSync(extractDir, { recursive: true, force: true });
    mkdirSync(extractDir, { recursive: true });
    await extractArchive(archivePath, extractDir, isWin);

    const extracted = join(extractDir, binName());
    if (!existsSync(extracted)) {
      throw new Error(`${binName()} not found in ${archiveName}`);
    }
    const finalPath = join(destDir, binName());
    renameSync(extracted, finalPath);
    if (!isWin) chmodSync(finalPath, 0o755);
    return finalPath;
  } finally {
    rmSync(archivePath, { force: true });
    rmSync(extractDir, { recursive: true, force: true });
  }
}

// fetchExpectedSum reads the "<sha256>  <filename>" line for archiveName out of a
// goreleaser checksums.txt.
async function fetchExpectedSum(url: string, archiveName: string): Promise<string> {
  const res = await fetch(url);
  if (!res.ok) throw new Error(`checksums.txt: HTTP ${res.status}`);
  for (const line of (await res.text()).split("\n")) {
    const fields = line.trim().split(/\s+/);
    if (fields.length === 2 && fields[1] === archiveName) return fields[0];
  }
  throw new Error(`no checksum entry for ${archiveName}`);
}

// downloadAndVerify streams url to dst, hashing as it goes, and throws on a
// SHA256 mismatch (leaving the partial file for the caller to clean up).
async function downloadAndVerify(
  url: string,
  dst: string,
  expectedSum: string,
  onProgress?: (percent: number) => void,
): Promise<void> {
  const res = await fetch(url);
  if (!res.ok || !res.body) throw new Error(`archive: HTTP ${res.status}`);
  const total = Number(res.headers.get("content-length")) || 0;

  const hasher = new Bun.CryptoHasher("sha256");
  const sink = Bun.file(dst).writer();
  let received = 0;
  let lastPct = -1;
  try {
    for await (const chunk of res.body as ReadableStream<Uint8Array>) {
      hasher.update(chunk);
      sink.write(chunk);
      if (onProgress && total > 0) {
        received += chunk.byteLength;
        const pct = Math.floor((received / total) * 100);
        if (pct !== lastPct) {
          lastPct = pct;
          onProgress(pct);
        }
      }
    }
  } finally {
    await sink.end();
  }

  const got = hasher.digest("hex");
  if (got !== expectedSum) {
    throw new Error(`sha256 mismatch: got ${got} want ${expectedSum}`);
  }
}

// extractArchive shells out to the system tar to unpack the single-binary
// archive into destDir. tar ships on macOS, Linux, and Windows 10+; bsdtar
// (macOS, Windows) auto-detects the zip format, while GNU tar (Linux) needs -z
// for the .tar.gz it receives.
async function extractArchive(
  archivePath: string,
  destDir: string,
  isWin: boolean,
): Promise<void> {
  const flags = isWin ? "-xf" : "-xzf";
  const proc = Bun.spawn(["tar", flags, archivePath, "-C", destDir], {
    stdout: "ignore",
    stderr: "pipe",
    stdin: "ignore",
  });
  const exitCode = await proc.exited;
  if (exitCode !== 0) {
    const stderr = (await new Response(proc.stderr).text()).trim();
    throw new Error(`tar exited ${exitCode}: ${stderr}`);
  }
}

/**
 * openDaemonStderr opens the file the daemon's stderr is written to, rolling
 * the previous run's aside first. A daemon that dies is restarted straight
 * away, and a log truncated on every spawn would erase the trace explaining
 * the death being restarted from — so the last run's output is what has to
 * survive, not this one's.
 *
 * Returns "ignore" if the file cannot be opened: not being able to record a
 * crash must never be a reason the daemon fails to start.
 */
function openDaemonStderr(): number | "ignore" {
  try {
    const dir = daemonLogDir();
    mkdirSync(dir, { recursive: true });
    const current = join(dir, STDERR_LOG);
    if (existsSync(current)) {
      renameSync(current, join(dir, STDERR_LOG_PREV));
    }
    return openSync(current, "a");
  } catch {
    return "ignore";
  }
}

/**
 * spawnDaemon launches the daemon as a background process and unrefs it so it
 * does not keep the app process alive. The daemon runs its own embedded NATS
 * server; the app connects to it over TCP afterwards.
 */
export function spawnDaemon(exePath: string): void {
  const stderr = openDaemonStderr();
  const proc = Bun.spawn([exePath], {
    stdout: "ignore",
    stderr,
    stdin: "ignore",
    // Tell the daemon the app started it; it reports this to the backend.
    env: { ...process.env, VLREC_LAUNCHER: "desktop" },
  });
  // The child holds its own copy; ours would otherwise stay open for the life
  // of the app.
  if (typeof stderr === "number") closeSync(stderr);
  proc.unref();
}
