// Popup windows for platform sign-in views. Electrobun's macOS webview drops
// every window.open, so popup sign-ins ("Sign in with Google" through Google
// Identity Services) do nothing. libVLLoginPopups.dylib, built into the bundle
// from desktop/native/macos/login-popups.m, opens them as real windows that keep
// window.opener and the view's partition. Windows' WebView2 already opens
// popups that way.

import { dlopen, FFIType, type Pointer } from "bun:ffi";
import { dirname, join } from "node:path";
import type { BrowserView } from "electrobun/main";

type LoginPopups = { symbols: { vl_login_popups_enable: (view: Pointer) => void } };

let native: LoginPopups | null = null;

// A view's pointer exists only once Electrobun has registered its native view.
const PTR_POLL_MS = 50;
const PTR_TIMEOUT_MS = 5_000;

// enableLoginPopups lets `view` open popup windows. Failures are logged: the
// sign-in still works for anything that does not need a popup.
export function enableLoginPopups(view: BrowserView): void {
  if (process.platform !== "darwin") return;
  const deadline = Date.now() + PTR_TIMEOUT_MS;
  const tryEnable = (): void => {
    if (view.isRemoved) return;
    const ptr = view.ptr;
    if (!ptr) {
      if (Date.now() < deadline) setTimeout(tryEnable, PTR_POLL_MS);
      else console.error("[login-popups] the view never got a native pointer");
      return;
    }
    try {
      // Built beside the app's own bun binary in Contents/MacOS.
      native ??= dlopen(join(dirname(process.execPath), "libVLLoginPopups.dylib"), {
        vl_login_popups_enable: { args: [FFIType.ptr], returns: FFIType.void },
      }) as unknown as LoginPopups;
      native.symbols.vl_login_popups_enable(ptr);
    } catch (err) {
      console.error("[login-popups] popups stay disabled:", err);
    }
  };
  tryEnable();
}
