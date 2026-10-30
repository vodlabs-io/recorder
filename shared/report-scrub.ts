// Scrubbing for error reports sent from the app's JavaScript, matching the
// daemon's (internal/errreport/scrub.go): emails, tokens, URL query strings
// and the user's name in home-folder paths are removed. IP addresses are kept.

const removed = "[removed]";
const urlQueryRe = /((?:https?|wss?|nats|rtmps?):\/\/[^\s?#"'<>]+)\?[^\s#"'<>]*/g;
// Same-origin paths such as /link?code=….
const pathQueryRe = /(^|[\s"'(])(\/[^\s?#"'<>]*)\?[^\s#"'<>]*/g;
const emailRe = /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g;
const jwtRe = /eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*/g;
// The folder name after /Users/, /home/ or C:\Users\, which is often the
// user's own name. Doubled backslashes are how JSON and logfmt escape them.
const homeRe = /(\/Users\/|\/home\/|[A-Za-z]:(?:\\\\|\\)Users(?:\\\\|\\))[^/\\\s"'<>:]+/g;
const sensitiveKeyRe =
  /cookie|token|authorization|password|secret|jwt|creds|credential|signature|api_?key|email/i;

export function scrubString(s: string): string {
  return s
    .replace(homeRe, "$1~")
    .replace(urlQueryRe, "$1?" + removed)
    .replace(pathQueryRe, "$1$2")
    .replace(jwtRe, removed)
    .replace(emailRe, "[email]");
}

export function scrubValue(v: unknown): unknown {
  if (typeof v === "string") return scrubString(v);
  if (Array.isArray(v)) return v.map(scrubValue);
  if (v && typeof v === "object") return scrubRecord(v as Record<string, unknown>);
  return v;
}

export function scrubRecord(m: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(m)) {
    out[k] = sensitiveKeyRe.test(k) ? removed : scrubValue(v);
  }
  return out;
}

// ReportEvent is the part of a Sentry event that scrubEvent touches, so this
// module needs no Sentry types and runs in the webview and in the app's host
// process alike.
export type ReportEvent = {
  message?: string;
  server_name?: string;
  user?: { id?: string | number; ip_address?: string | null };
  request?: { url?: string; headers?: Record<string, string> };
  exception?: {
    values?: {
      value?: string;
      stacktrace?: { frames?: { filename?: string; abs_path?: string }[] };
    }[];
  };
  breadcrumbs?: { message?: string; data?: Record<string, unknown> }[];
  contexts?: Record<string, unknown>;
  tags?: Record<string, unknown>;
  extra?: Record<string, unknown>;
};

// scrubEvent removes what a report must not carry, in place. The computer's
// name goes; the IP address and User-Agent stay.
export function scrubEvent<T extends ReportEvent>(ev: T): T {
  delete ev.server_name;
  if (ev.user) ev.user = { ip_address: ev.user.ip_address };
  if (ev.request) {
    const ua = ev.request.headers?.["User-Agent"];
    ev.request = {
      url: scrubString(ev.request.url ?? ""),
      headers: ua ? { "User-Agent": ua } : undefined,
    };
  }
  if (ev.message) ev.message = scrubString(ev.message);
  for (const ex of ev.exception?.values ?? []) {
    if (ex.value) ex.value = scrubString(ex.value);
    for (const f of ex.stacktrace?.frames ?? []) {
      if (f.filename) f.filename = scrubString(f.filename);
      if (f.abs_path) f.abs_path = scrubString(f.abs_path);
    }
  }
  if (ev.extra) ev.extra = scrubRecord(ev.extra);
  if (ev.contexts) ev.contexts = scrubRecord(ev.contexts);
  if (ev.tags) ev.tags = scrubRecord(ev.tags);
  for (const b of ev.breadcrumbs ?? []) scrubBreadcrumb(b);
  return ev;
}

export function scrubBreadcrumb<T extends { message?: string; data?: Record<string, unknown> }>(
  b: T,
): T {
  if (b.message) b.message = scrubString(b.message);
  if (b.data) b.data = scrubRecord(b.data);
  return b;
}
