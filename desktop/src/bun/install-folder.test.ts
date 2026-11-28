import { describe, expect, test } from "bun:test";
import { chmodSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { installFolder, installFolderWritable } from "./install-folder";

describe("installFolder", () => {
  test("is the parent of the bin folder", () => {
    expect(installFolder("/opt/vodlabs-recorder/bin/bun")).toBe("/opt/vodlabs-recorder");
    expect(installFolder("/Applications/VodLabs Recorder.app/Contents/MacOS/bun")).toBe(
      "/Applications/VodLabs Recorder.app/Contents",
    );
  });
});

describe("installFolderWritable", () => {
  function fakeInstall(mode: number): string {
    const root = mkdtempSync(join(tmpdir(), "install-folder-"));
    mkdirSync(join(root, "bin"));
    const bun = join(root, "bin", "bun");
    writeFileSync(bun, "");
    chmodSync(root, mode);
    return bun;
  }

  test("is true for a folder the user owns", () => {
    expect(installFolderWritable(fakeInstall(0o755))).toBe(true);
  });

  test.skipIf(process.platform === "win32" || process.getuid?.() === 0)(
    "is false for a read-only folder",
    () => {
      expect(installFolderWritable(fakeInstall(0o555))).toBe(false);
    },
  );
});
