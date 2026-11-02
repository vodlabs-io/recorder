// Swaps the macOS tray icon through the app's own native library, which keeps it
// a template image in the same place in the menu bar (see
// desktop/native/macos/tray-icon.m). A failure is logged: the menu still lists
// whatever needs attention.

import { dlopen, FFIType, ptr } from "bun:ffi";
import { dirname, join } from "node:path";

// Tray's default size, which the icon was created at.
const TRAY_ICON_POINTS = 16;

type Native = {
  vl_tray_watch_status_item: () => void;
  vl_tray_set_template_image: (path: number, width: number, height: number) => void;
};

let native: Native | null = null;

function load(): Native {
  native ??= dlopen(join(dirname(process.execPath), "libVLLoginPopups.dylib"), {
    vl_tray_watch_status_item: { args: [], returns: FFIType.void },
    vl_tray_set_template_image: { args: [FFIType.ptr, FFIType.f64, FFIType.f64], returns: FFIType.void },
  }).symbols as unknown as Native;
  return native;
}

// Call before creating the Tray, so the swap can find its status item.
export function prepareMacTrayIconSwap(): void {
  try {
    load().vl_tray_watch_status_item();
  } catch (err) {
    console.error("[tray-icon] tray icon swaps are unavailable:", err);
  }
}

export function setMacTrayTemplateImage(file: string): void {
  try {
    // The native side copies the path before this returns.
    load().vl_tray_set_template_image(ptr(Buffer.from(`${file}\0`, "utf8")), TRAY_ICON_POINTS, TRAY_ICON_POINTS);
  } catch (err) {
    console.error("[tray-icon] could not swap the tray icon:", err);
  }
}
