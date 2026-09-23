import { describe, expect, test } from "bun:test";
import {
  START_HIDDEN_FLAG,
  desktopEntry,
  launchAgentPlist,
  startAtLoginSupported,
} from "./start-at-login";

describe("login item", () => {
  // A login item that opened a window on every boot would be the fastest way
  // for a user to turn this off again.
  test("asks the app to come up in the tray", () => {
    expect(launchAgentPlist()).toContain(START_HIDDEN_FLAG);
    expect(desktopEntry()).toContain(START_HIDDEN_FLAG);
  });

  test("launches this build, not whatever is on PATH", () => {
    expect(launchAgentPlist()).toContain(process.execPath);
    expect(desktopEntry()).toContain(process.execPath);
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
