import { describe, expect, test } from "bun:test";
import { dirname } from "node:path";
import {
  desktopEntry,
  launchAgentPlist,
  launcherPath,
  parseRunValue,
  runEntry,
  startAtLoginSupported,
  startedAtLogin,
} from "./start-at-login";

describe("login item", () => {
  // A login item that opened a window on every boot would be the fastest way
  // for a user to turn this off again.
  test("asks the app to come up in the tray", () => {
    expect(launchAgentPlist()).toContain("<key>VLREC_START_HIDDEN</key>");
    expect(desktopEntry()).toContain("Exec=env VLREC_START_HIDDEN=1 ");
  });

  // bun alone, without the launcher's script argument, prints its usage and
  // exits.
  test("launches this build's launcher, not bun or whatever is on PATH", () => {
    expect(launcherPath()).toMatch(/[\\/]launcher(\.exe)?$/);
    expect(dirname(launcherPath())).toBe(dirname(process.execPath));
    for (const entry of [launchAgentPlist(), desktopEntry(), runEntry()]) {
      expect(entry).toContain(launcherPath());
      expect(entry).not.toContain(process.execPath);
    }
  });

  test("writes a plist macOS will load", () => {
    const plist = launchAgentPlist();
    expect(plist).toStartWith('<?xml version="1.0"');
    expect(plist).toContain("<key>RunAtLoad</key>");
    expect(plist).toContain("<true/>");
    expect(plist).toContain("com.vodlabs.recorder");
  });

  test("writes a desktop entry an autostart directory will read", () => {
    const entry = desktopEntry();
    expect(entry).toStartWith("[Desktop Entry]");
    expect(entry).toContain("Type=Application");
    expect(entry).toContain("Exec=");
  });

  test("is offered on the three platforms the app ships to", () => {
    expect(startAtLoginSupported()).toBe(true);
  });
});

describe("reading the Run entry back", () => {
  test("finds this app's value among others", () => {
    const out = [
      "",
      "HKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Run",
      '    OneDrive    REG_SZ    "C:\\OneDrive.exe" /background',
      '    VodLabs Recorder    REG_SZ    "C:\\app\\bin\\launcher.exe"',
      "",
    ].join("\r\n");
    expect(parseRunValue(out)).toBe('"C:\\app\\bin\\launcher.exe"');
  });

  test("reports none when the value is absent", () => {
    expect(parseRunValue("HKEY_CURRENT_USER\\...\\Run\r\n")).toBeNull();
  });
});

describe("startedAtLogin", () => {
  test("honours the login item's marker once", async () => {
    process.env.VLREC_START_HIDDEN = "1";
    expect(await startedAtLogin()).toBe(true);
    expect(process.env.VLREC_START_HIDDEN).toBeUndefined();
  });
});
