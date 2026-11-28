import { accessSync, constants } from "node:fs";
import { dirname } from "node:path";

// installFolder returns the folder the app was installed into: the parent of
// the bin folder holding the bundled bun (Contents/ on macOS).
export function installFolder(execPath: string): string {
  return dirname(dirname(execPath));
}

// installFolderWritable reports whether the app can replace its own files,
// which self-update needs. A package manager that installs the app system-wide
// (the AUR package under /opt) leaves it read-only.
export function installFolderWritable(execPath: string): boolean {
  try {
    accessSync(installFolder(execPath), constants.W_OK);
    return true;
  } catch {
    return false;
  }
}

export const PACKAGE_MANAGER_MESSAGE =
  "VodLabs Recorder was installed by a package manager, so update it there.";
