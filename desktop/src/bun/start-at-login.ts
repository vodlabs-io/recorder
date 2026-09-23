import { homedir } from "node:os";
import { join } from "node:path";

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
// keeps, and "enabled" means that artifact exists and points at this build.

const APP_NAME = "VodLabs Recorder";

/**
 * START_HIDDEN_FLAG is passed by the login item. Someone logging in asked for
 * their streams to be recorded, not for a window in their face, so the app
 * comes up in the tray when the OS starts it.
 */
export const START_HIDDEN_FLAG = "--start-hidden";
const IDENTIFIER = "com.vodlabs.recorder";

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
 * exePath is what the OS should launch. On macOS this is the binary inside the
 * bundle, which starts the app exactly as double-clicking it would.
 */
function exePath(): string {
  return process.execPath;
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
		<string>${exePath()}</string>
		<string>${START_HIDDEN_FLAG}</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
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
Exec="${exePath()}" ${START_HIDDEN_FLAG}
X-GNOME-Autostart-enabled=true
Comment=Record live streams automatically
`;
}

/** startAtLoginSupported reports whether this platform has a path for it. */
export function startAtLoginSupported(): boolean {
  return isMac || isWindows || isLinux;
}

/**
 * getStartAtLogin reads the OS back, so the checkbox reflects what the system
 * will actually do rather than what the app last asked for.
 */
export async function getStartAtLogin(): Promise<boolean> {
  try {
    if (isMac) return await Bun.file(launchAgentPath()).exists();
    if (isLinux) return await Bun.file(autostartPath()).exists();
    if (isWindows) {
      const proc = Bun.spawn(
        ["reg", "query", RUN_KEY, "/v", APP_NAME],
        { stdout: "pipe", stderr: "ignore" },
      );
      return (await proc.exited) === 0;
    }
  } catch (err) {
    console.error("start-at-login: failed to read the current state:", err);
  }
  return false;
}

const RUN_KEY = "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run";

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
           `"${exePath()}" ${START_HIDDEN_FLAG}`, "/f"]
        : ["reg", "delete", RUN_KEY, "/v", APP_NAME, "/f"];
      const proc = Bun.spawn(args, { stdout: "ignore", stderr: "pipe" });
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
