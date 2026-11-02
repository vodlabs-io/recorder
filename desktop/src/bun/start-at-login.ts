import { homedir } from "node:os";
import { dirname, join } from "node:path";

// MARK: Start at login
//
// A recorder that is not running records nothing, and the machine it runs on
// reboots — for updates, overnight, after a power cut. Every stream that went
// live in between is simply missed, with nothing to show that anything was
// wrong. So the app can ask the OS to start it at login.
//
// The OS is the source of truth rather than a setting of our own: the user may
// well turn this off in System Settings, Task Manager or their desktop
// environment, and a checkbox that disagreed with the system would be worse
// than no checkbox. Each platform is therefore read back from the artifact it
// keeps, and "enabled" means that artifact exists.

const APP_NAME = "VodLabs Recorder";

/**
 * START_HIDDEN_ENV is set by the login item on macOS and Linux. Someone logging
 * in asked for their streams to be recorded, not for a window in their face, so
 * the app comes up in the tray when the OS starts it. An environment variable
 * rather than a flag: Electrobun's launcher does not forward its arguments, but
 * the bun process inherits its environment.
 */
const START_HIDDEN_ENV = "VLREC_START_HIDDEN";
const IDENTIFIER = "com.vodlabs.recorder";

/**
 * A Windows Run entry cannot set an environment variable, so there a launch
 * this soon after the user's logon, with the Run entry in place, counts as a
 * login start. Windows holds Run entries back for a while after logon, and
 * longer on a slow disk.
 */
const LOGON_WINDOW_S = 180;

const isMac = process.platform === "darwin";
const isWindows = process.platform === "win32";
const isLinux = process.platform === "linux";

/** The macOS LaunchAgent this app installs for itself. */
function launchAgentPath(): string {
  return join(homedir(), "Library", "LaunchAgents", `${IDENTIFIER}.plist`);
}

/** The XDG autostart entry, used by every desktop environment worth the name. */
function autostartPath(): string {
  const configHome =
    process.env.XDG_CONFIG_HOME || join(homedir(), ".config");
  return join(configHome, "autostart", `${IDENTIFIER}.desktop`);
}

/**
 * launcherPath is what the OS should launch: Electrobun's launcher, beside the
 * bun binary this runs in. It starts the app exactly as the Start menu, Dock
 * or app menu would; bun on its own does not know which script to run.
 */
export function launcherPath(): string {
  return join(dirname(process.execPath), isWindows ? "launcher.exe" : "launcher");
}

export function launchAgentPlist(): string {
  // Written by hand rather than through plutil: it is a fixed shape, and
  // shelling out would make a failure harder to report than it is worth.
  return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>${IDENTIFIER}</string>
	<key>ProgramArguments</key>
	<array>
		<string>${launcherPath()}</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>${START_HIDDEN_ENV}</key>
		<string>1</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>AssociatedBundleIdentifiers</key>
	<string>${IDENTIFIER}</string>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`;
}

export function desktopEntry(): string {
  return `[Desktop Entry]
Type=Application
Name=${APP_NAME}
Exec=env ${START_HIDDEN_ENV}=1 "${launcherPath()}"
X-GNOME-Autostart-enabled=true
Comment=Record live streams automatically
`;
}

/** runEntry is the command the Windows Run entry holds. */
export function runEntry(): string {
  return `"${launcherPath()}"`;
}

/** startAtLoginSupported reports whether this platform has a path for it. */
export function startAtLoginSupported(): boolean {
  return isMac || isWindows || isLinux;
}

const RUN_KEY = "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run";

/** parseRunValue pulls the command out of `reg query` output, or null. */
export function parseRunValue(stdout: string): string | null {
  for (const line of stdout.split(/\r?\n/)) {
    const m = line.match(/^\s+(.+?)\s+REG_(?:EXPAND_)?SZ\s+(.*)$/);
    if (m && m[1] === APP_NAME) return m[2].trim();
  }
  return null;
}

/** readEntry returns the login item this platform holds, or null if none. */
async function readEntry(): Promise<string | null> {
  if (isMac || isLinux) {
    const file = Bun.file(isMac ? launchAgentPath() : autostartPath());
    return (await file.exists()) ? await file.text() : null;
  }
  if (isWindows) {
    const proc = Bun.spawn(["reg", "query", RUN_KEY, "/v", APP_NAME], {
      stdout: "pipe",
      stderr: "ignore",
      windowsHide: true,
    });
    const stdout = await new Response(proc.stdout).text();
    if ((await proc.exited) !== 0) return null;
    return parseRunValue(stdout);
  }
  return null;
}

/** expectedEntry is what readEntry returns once this build is installed. */
function expectedEntry(): string {
  if (isMac) return launchAgentPlist();
  if (isLinux) return desktopEntry();
  return runEntry();
}

/**
 * getStartAtLogin reads the OS back, so the checkbox reflects what the system
 * will actually do rather than what the app last asked for.
 */
export async function getStartAtLogin(): Promise<boolean> {
  try {
    return (await readEntry()) !== null;
  } catch (err) {
    console.error("start-at-login: failed to read the current state:", err);
  }
  return false;
}

/**
 * setStartAtLogin turns it on or off, reporting what the OS holds afterwards.
 * A caller shows that rather than what it asked for: a sandboxed or locked-down
 * machine can refuse, and the checkbox should say so instead of lying.
 */
export async function setStartAtLogin(enabled: boolean): Promise<boolean> {
  try {
    if (isMac) {
      const path = launchAgentPath();
      if (enabled) {
        await Bun.write(path, launchAgentPlist());
      } else {
        await Bun.file(path).delete();
      }
    } else if (isLinux) {
      const path = autostartPath();
      if (enabled) {
        await Bun.write(path, desktopEntry());
      } else {
        await Bun.file(path).delete();
      }
    } else if (isWindows) {
      const args = enabled
        ? ["reg", "add", RUN_KEY, "/v", APP_NAME, "/t", "REG_SZ", "/d",
           runEntry(), "/f"]
        : ["reg", "delete", RUN_KEY, "/v", APP_NAME, "/f"];
      const proc = Bun.spawn(args, {
        stdout: "ignore",
        stderr: "pipe",
        windowsHide: true,
      });
      if ((await proc.exited) !== 0) {
        console.error(
          "start-at-login: reg exited non-zero:",
          await new Response(proc.stderr).text(),
        );
      }
    }
  } catch (err) {
    // Deleting an entry that is not there is the common case here, and is not
    // a failure: the read below settles what actually happened either way.
    console.error("start-at-login: failed to apply the change:", err);
  }
  return getStartAtLogin();
}

/**
 * repairStartAtLogin rewrites a login item that no longer launches this build
 * (the app moved, or an older build wrote it), leaving a missing one missing:
 * turning it off is the user's call.
 */
export async function repairStartAtLogin(): Promise<void> {
  try {
    const entry = await readEntry();
    if (entry === null || entry === expectedEntry()) return;
    console.log("start-at-login: rewriting a stale login item");
    await setStartAtLogin(true);
  } catch (err) {
    console.error("start-at-login: failed to repair the login item:", err);
  }
}

/** secondsSinceLogon is how long this Windows session's shell has run. */
async function secondsSinceLogon(): Promise<number | null> {
  // The session's first explorer.exe is its shell, started at logon.
  const script =
    "$s = (Get-Process -Id $PID).SessionId; " +
    "$e = Get-Process explorer -ErrorAction Stop | Where-Object SessionId -eq $s | " +
    "Sort-Object StartTime | Select-Object -First 1; " +
    "[int]((Get-Date) - $e.StartTime).TotalSeconds";
  const proc = Bun.spawn(
    ["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script],
    { stdout: "pipe", stderr: "ignore", windowsHide: true },
  );
  const out = (await new Response(proc.stdout).text()).trim();
  if ((await proc.exited) !== 0) return null;
  const seconds = Number(out);
  return out !== "" && Number.isFinite(seconds) ? seconds : null;
}

/**
 * startedAtLogin reports whether the OS started this process through the login
 * item, so the app should come up in the tray without a window. Call once, at
 * startup: it clears the environment marker so a relaunch after an update
 * opens normally.
 */
export async function startedAtLogin(): Promise<boolean> {
  const marked = process.env[START_HIDDEN_ENV] === "1";
  delete process.env[START_HIDDEN_ENV];
  if (marked) return true;
  if (!isWindows) return false;
  try {
    if (!(await getStartAtLogin())) return false;
    const seconds = await secondsSinceLogon();
    return seconds !== null && seconds <= LOGON_WINDOW_S;
  } catch (err) {
    console.error("start-at-login: failed to check the logon time:", err);
    return false;
  }
}
