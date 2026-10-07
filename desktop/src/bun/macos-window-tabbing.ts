// Keep macOS from merging the app's windows into tabs.
//
// With "Prefer tabs when opening documents" set to Always in System Settings,
// AppKit adds every new window to the frontmost window's tab group, and a tab
// group moves as one window. Electrobun never opts out, so a background window
// (the stream sniffer's, parked off-screen) joined the main window's group and
// took the main window off-screen with it.
//
// NSWindow.allowsAutomaticWindowTabbing = NO turns this off for the whole app.
// It is set over bun:ffi, through the Objective-C runtime, because Electrobun
// exposes no window tabbing option. Call it before the first window opens.
// Best-effort: a failure is logged, never thrown.

import { dlopen, FFIType, ptr } from "bun:ffi";

export function disableWindowTabbing(): void {
  if (process.platform !== "darwin") return;
  try {
    const { symbols: objc } = dlopen("/usr/lib/libobjc.A.dylib", {
      objc_getClass: { args: [FFIType.ptr], returns: FFIType.ptr },
      sel_registerName: { args: [FFIType.ptr], returns: FFIType.ptr },
      objc_msgSend: { args: [FFIType.ptr, FFIType.ptr, FFIType.bool], returns: FFIType.void },
    });
    // Held in variables so the buffers outlive the calls reading them.
    const className = Buffer.from("NSWindow\0", "utf8");
    const selName = Buffer.from("setAllowsAutomaticWindowTabbing:\0", "utf8");
    const nsWindow = objc.objc_getClass(ptr(className));
    const setter = objc.sel_registerName(ptr(selName));
    if (!nsWindow || !setter) throw new Error("NSWindow or its tabbing setter not found");
    objc.objc_msgSend(nsWindow, setter, false);
  } catch (err) {
    console.warn("[window-tabbing] could not disable automatic window tabbing:", err);
  }
}
