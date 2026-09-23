// Single-instance enforcement for the desktop app.
//
// Only one desktop app may run per machine, including while minimized to the
// system tray. Electrobun has no built-in single-instance lock (unlike Electron's
// app.requestSingleInstanceLock), and the daemon's own port-bind check only
// guards the *daemon*: two desktop apps would both connect to the one daemon and
// show two windows. This is reachable on Windows by double-clicking the shortcut
// again.
//
// The lock is a fixed loopback TCP port the primary instance binds at startup.
// A later instance fails to bind (EADDRINUSE), takes that as "already running",
// connects to the port to ask the primary to surface its window, and exits. The
// OS releases a listen socket when its process dies, so the lock self-heals after
// a crash — no stale lock file or PID-liveness check needed. The socket doubles
// as the IPC channel for the "activate the existing window" signal.

const HOST = "127.0.0.1";

// Arbitrary app-specific port, distinct from the daemon's NATS port (4222).
// Overridable so a dev build can run alongside an installed one (each needs its
// own lock) without them fighting over a single instance.
const DEFAULT_LOCK_PORT = 47615;

// The token a second instance writes so the primary only reacts to our own
// protocol, not to any stray local connection to the port.
const ACTIVATE_MSG = "vlrec-activate";

function lockPort(): number {
  const override = Number(process.env.VLREC_APP_LOCK_PORT);
  return Number.isInteger(override) && override > 0 ? override : DEFAULT_LOCK_PORT;
}

// The primary's listen socket, held for the app's lifetime so it isn't garbage
// collected (which would drop the lock). Never stopped in normal operation — the
// process exit releases it; releaseSingleInstanceLock exists only for tests.
let listener: ReturnType<typeof Bun.listen> | null = null;

/**
 * acquireSingleInstanceLock tries to become the primary instance by binding the
 * lock port.
 *
 * Returns true when we are the primary (lock held); onActivate is then invoked
 * whenever a later instance starts, so the caller can raise its window.
 *
 * Returns false when another instance already holds the lock: it has been asked
 * to surface its window and the caller should exit immediately without creating
 * a window or touching the daemon.
 *
 * A bind failure for any reason other than the port being taken does not block
 * startup — we log it and run anyway (degraded: no single-instance guarantee),
 * since refusing to launch over it would be worse.
 */
export async function acquireSingleInstanceLock(
  onActivate: () => void,
): Promise<boolean> {
  try {
    listener = Bun.listen({
      hostname: HOST,
      port: lockPort(),
      socket: {
        data(_socket, data) {
          if (data.toString().includes(ACTIVATE_MSG)) onActivate();
        },
        open() {},
        error() {},
      },
    });
    return true;
  } catch (err) {
    if ((err as { code?: string }).code !== "EADDRINUSE") {
      console.error("Single-instance lock bind failed; continuing:", err);
      return true;
    }
    await notifyPrimary();
    return false;
  }
}

// notifyPrimary connects to the running primary and asks it to surface its
// window. Best-effort and time-bounded: if the primary can't be reached (it may
// be mid-exit) we resolve anyway so the second instance never hangs before
// quitting.
function notifyPrimary(): Promise<void> {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve();
    };
    const timer = setTimeout(finish, 1000);
    Bun.connect({
      hostname: HOST,
      port: lockPort(),
      socket: {
        open(socket) {
          socket.write(ACTIVATE_MSG);
          socket.end();
        },
        close: finish,
        error: finish,
        data() {},
      },
    }).catch(finish);
  });
}

// releaseSingleInstanceLock stops the listen socket. The app never calls this —
// it holds the lock for its whole lifetime and the OS reclaims the socket on
// exit; it exists for test teardown.
export function releaseSingleInstanceLock(): void {
  listener?.stop(true);
  listener = null;
}
