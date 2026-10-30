// Error reports from the desktop app's own process, to VodLabs' Sentry
// project. They follow the answer the user gave the daemon (reports.get /
// reports.set), so they are sent only while the user has them on. Bug reports
// go through the daemon instead, which attaches the logs.
//
// What is sent: console.error calls, and a crash (an uncaught exception, on
// which Electrobun exits at once) on the next launch. Every event passes
// through shared/report-scrub first.

import { readFileSync, renameSync, writeFileSync } from "fs";
import { join } from "path";
import {
  Client,
  addBreadcrumb,
  captureException,
  captureMessage,
  createStackParser,
  createTransport,
  dedupeIntegration,
  eventFromMessage,
  eventFromUnknownInput,
  getCurrentScope,
  initAndBind,
  linkedErrorsIntegration,
  resolvedSyncPromise,
  type ClientOptions,
  type Event,
  type EventHint,
  type ParameterizedString,
  type SeverityLevel,
  type StackLineParser,
  type Transport,
  type BaseTransportOptions,
} from "@sentry/core";
import { scrubBreadcrumb, scrubEvent } from "../../../shared/report-scrub";
import { daemonLogDir } from "./daemon-process";

// The recorder project's public client key, as in internal/errreport.
const DSN =
  "https://748fd4caebaf0ca976ef37c2c4b4ddc9@o4512343562780672.ingest.us.sentry.io/4512344170758144";

// CRASH_FILE holds the uncaught exception that ended the last run, until it
// has been read on the next launch.
const CRASH_FILE = "desktop-crash.log";

// A call site that keeps failing is reported once an hour.
const MIN_INTERVAL_MS = 60 * 60 * 1000;

let ready = false;
let enabled = false;
const lastSent = new Map<string, number>();

// Bun's stack lines: "    at fn (/path/file.js:10:5)" or "    at /path/file.js:10:5".
const bunStackLine: StackLineParser = [
  90,
  (line) => {
    const m = /^\s*at (?:(.+?) \()?(.+?):(\d+):(\d+)\)?$/.exec(line);
    if (!m) return undefined;
    return {
      function: m[1] || "?",
      filename: m[2],
      lineno: Number(m[3]),
      colno: Number(m[4]),
      in_app: !m[2].includes("node_modules"),
    };
  },
];
const stackParser = createStackParser(bunStackLine);

type HostOptions = ClientOptions<BaseTransportOptions>;

class HostClient extends Client<HostOptions> {
  // Public, as initAndBind requires.
  public constructor(options: HostOptions) {
    super(options);
  }

  eventFromException(exception: unknown, hint?: EventHint): PromiseLike<Event> {
    return resolvedSyncPromise(eventFromUnknownInput(this, stackParser, exception, hint));
  }
  eventFromMessage(
    message: ParameterizedString,
    level: SeverityLevel = "info",
    hint?: EventHint,
  ): PromiseLike<Event> {
    return resolvedSyncPromise(eventFromMessage(stackParser, message, level, hint));
  }
}

function fetchTransport(options: BaseTransportOptions): Transport {
  return createTransport(options, async (req) => {
    const res = await fetch(options.url, { method: "POST", body: req.body as BodyInit });
    return {
      statusCode: res.status,
      headers: {
        "x-sentry-rate-limits": res.headers.get("x-sentry-rate-limits"),
        "retry-after": res.headers.get("retry-after"),
      },
    };
  });
}

// initErrorReports creates the client, off until followErrorReports says the
// user turned reports on. A dev build reports only to VLREC_SENTRY_DSN.
export function initErrorReports(appVersion: string, channel: string): void {
  const override = process.env.VLREC_SENTRY_DSN?.trim();
  const dev = channel === "dev";
  const dsn = override !== undefined ? (override === "off" ? "" : override) : dev ? "" : DSN;
  if (!dsn) return;
  initAndBind(HostClient, {
    dsn,
    release: `recorder@${appVersion || "unknown"}`,
    environment: dev ? "development" : "production",
    stackParser,
    transport: fetchTransport,
    integrations: [dedupeIntegration(), linkedErrorsIntegration()],
    maxBreadcrumbs: 50,
    beforeSend: (ev) => (enabled ? scrubEvent(ev) : null),
    beforeBreadcrumb: (b) => scrubBreadcrumb(b),
  });
  const scope = getCurrentScope();
  scope.setTag("component", "desktop");
  scope.setTag("os", process.platform);
  scope.setTag("arch", process.arch);
  scope.setUser({ ip_address: "{{auto}}" });
  ready = true;
}

// followErrorReports applies the user's answer, as the daemon reports it, and
// sends the previous run's crash once reports are on.
export function followErrorReports<T extends { enabled: boolean | null }>(state: T): T {
  const was = enabled;
  enabled = ready && state.enabled === true;
  if (enabled && !was) reportPreviousCrash();
  return state;
}

// noteConsole is called with every console.* call the file logger mirrors:
// errors are reported, everything else becomes breadcrumbs for them.
export function noteConsole(level: "error" | "warn" | "info" | "debug", args: unknown[]): void {
  if (level === "error" && isUncaught(args)) {
    // Electrobun exits right after logging this, before anything can be
    // sent, so it goes out on the next launch.
    saveCrash(args);
    return;
  }
  if (!enabled) return;
  const text = args.map((a) => (a instanceof Error ? `${a.name}: ${a.message}` : String(a))).join(" ");
  if (level !== "error") {
    if (level !== "debug") addBreadcrumb({ category: "console", level: level === "warn" ? "warning" : "info", message: text });
    return;
  }
  const key = text.replace(/\d+/g, "N").slice(0, 120);
  const now = Date.now();
  if (now - (lastSent.get(key) ?? 0) < MIN_INTERVAL_MS) return;
  lastSent.set(key, now);
  const err = args.find((a): a is Error => a instanceof Error);
  if (err) {
    captureException(err, { extra: { message: text } });
  } else {
    captureMessage(text, "error");
  }
}

function isUncaught(args: unknown[]): boolean {
  return typeof args[0] === "string" && args[0].startsWith("Uncaught exception");
}

function saveCrash(args: unknown[]): void {
  const err = args.find((a): a is Error => a instanceof Error);
  const record = err
    ? { name: err.name, message: err.message, stack: err.stack ?? "" }
    : { name: "Error", message: args.map(String).join(" "), stack: "" };
  try {
    writeFileSync(join(daemonLogDir(), CRASH_FILE), JSON.stringify(record));
  } catch {
    /* nothing more can be done on the way out */
  }
}

function reportPreviousCrash(): void {
  const path = join(daemonLogDir(), CRASH_FILE);
  let raw: string;
  try {
    raw = readFileSync(path, "utf8");
    // Kept beside the logs, so a bug report still attaches it.
    renameSync(path, join(daemonLogDir(), "desktop-crash.prev.log"));
  } catch {
    return;
  }
  try {
    const rec = JSON.parse(raw) as { name: string; message: string; stack: string };
    const err = new Error(rec.message);
    err.name = rec.name;
    if (rec.stack) err.stack = rec.stack;
    captureException(err, { level: "fatal", tags: { crash: "previous_run" } });
  } catch {
    /* not ours, or cut short */
  }
}
