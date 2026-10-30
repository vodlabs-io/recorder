// Durable log for the desktop app, beside the daemon's own logs in
// <UserCacheDir>/vodlabs/recorder/logs, since stdout is discarded outside a dev
// terminal. console.* still writes to stdout; this adds a copy on disk, in the
// daemon's logfmt so one lnav view parses every component the same way.

import { appendFileSync, mkdirSync, renameSync, statSync } from "fs";
import { join } from "path";
import { daemonLogDir } from "./daemon-process";
import { noteConsole } from "./error-reports";

const LOG_FILE = "desktop.log";
const LOG_FILE_PREV = "desktop.prev.log";

// Matches vllog's per-file rotation size. One previous generation is kept: the
// desktop is far less chatty than the daemon's services, so this is days of
// history, not minutes.
const MAX_BYTES = 5 * 1024 * 1024;

type Level = "DEBUG" | "INFO" | "WARN" | "ERROR";

let logPath = "";
let installed = false;

// quote renders a value as a logfmt field: always quoted, since a log line is
// prose and would otherwise break parsing at its first space.
function quote(s: string): string {
  return '"' + s.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/\n/g, "\\n") + '"';
}

// localTimestamp renders ISO-8601 with the local UTC offset, matching Go's
// time.RFC3339 output from the daemon's logger. A UTC "Z" timestamp here would
// sort and interleave wrongly against the daemon's lines in one lnav view,
// which is the whole reason this file lives in that directory.
function localTimestamp(d: Date): string {
  const pad = (n: number, w = 2) => String(Math.abs(n)).padStart(w, "0");
  const offsetMin = -d.getTimezoneOffset();
  const sign = offsetMin >= 0 ? "+" : "-";
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
    `T${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}` +
    `.${pad(d.getMilliseconds(), 3)}` +
    `${sign}${pad(Math.trunc(offsetMin / 60))}:${pad(offsetMin % 60)}`
  );
}

function format(arg: unknown): string {
  if (typeof arg === "string") return arg;
  if (arg instanceof Error) return arg.stack || `${arg.name}: ${arg.message}`;
  try {
    return JSON.stringify(arg);
  } catch {
    return String(arg);
  }
}

// rotate moves the current file aside once it passes MAX_BYTES. Failures are
// swallowed: logging must never be the reason the app misbehaves.
function rotateIfNeeded(): void {
  try {
    if (statSync(logPath).size < MAX_BYTES) return;
    renameSync(logPath, join(daemonLogDir(), LOG_FILE_PREV));
  } catch {
    /* no file yet, or the rename lost a race — either way, keep logging */
  }
}

function write(level: Level, args: unknown[]): void {
  if (!logPath) return;
  try {
    rotateIfNeeded();
    const msg = args.map(format).join(" ");
    const line = `time=${localTimestamp(new Date())} level=${level} msg=${quote(msg)} service=desktop\n`;
    appendFileSync(logPath, line);
  } catch {
    /* a log write must never take the app down */
  }
}

// desktopLogPath is the file console.* is mirrored into, for the About screen
// to point at. "" until installFileLogging has run (or if it found no writable
// log dir).
export function desktopLogPath(): string {
  return logPath;
}

// installFileLogging mirrors console.* into desktop.log. Call once, as early as
// possible: anything logged before it lands only on stdout.
export function installFileLogging(): void {
  if (installed) return;
  installed = true;

  const dir = daemonLogDir();
  try {
    mkdirSync(dir, { recursive: true });
  } catch {
    return; // no writable log dir: stdout still works, carry on
  }
  logPath = join(dir, LOG_FILE);

  const levels: Array<[Level, "log" | "info" | "warn" | "error" | "debug"]> = [
    ["INFO", "log"],
    ["INFO", "info"],
    ["WARN", "warn"],
    ["ERROR", "error"],
    ["DEBUG", "debug"],
  ];
  for (const [level, method] of levels) {
    const original = console[method].bind(console);
    const report = method === "log" ? "info" : method;
    console[method] = (...args: unknown[]): void => {
      original(...args);
      write(level, args);
      noteConsole(report, args);
    };
  }

  console.log(`[desktop] logging to ${logPath}`);
}
