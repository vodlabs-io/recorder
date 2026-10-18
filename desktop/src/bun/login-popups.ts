// Popup windows for platform sign-in views. Electrobun's macOS and Linux
// webviews drop every window.open, so popup sign-ins ("Sign in with Google"
// through Google Identity Services) do nothing. On macOS,
// libVLLoginPopups.dylib, built into the bundle from
// desktop/native/macos/login-popups.m, opens them as real windows that keep
// window.opener and the view's partition. On Linux the patched native wrapper
// does the same once enableWebviewPopups is called for the view (see
// patches/README.md). Windows' WebView2 already opens popups that way.

import { dlopen, FFIType, type Pointer } from "bun:ffi";
import { dirname, join } from "node:path";
import type { BrowserView } from "electrobun/main";

type Enable = (view: Pointer) => void;

let enable: Enable | null = null;

// A view's pointer exists only once Electrobun has registered its native view.
const PTR_POLL_MS = 50;
const PTR_TIMEOUT_MS = 5_000;

// The library and symbol that enable popups on this platform. Both sit beside
// the app's own bun binary (Contents/MacOS, or bin/ on Linux).
function loadEnable(): Enable {
  const dir = dirname(process.execPath);
  if (process.platform === "darwin") {
    const lib = dlopen(join(dir, "libVLLoginPopups.dylib"), {
      vl_login_popups_enable: { args: [FFIType.ptr], returns: FFIType.void },
    });
    return lib.symbols.vl_login_popups_enable as unknown as Enable;
  }
  const lib = dlopen(join(dir, "libNativeWrapper.so"), {
    enableWebviewPopups: { args: [FFIType.ptr], returns: FFIType.void },
  });
  return lib.symbols.enableWebviewPopups as unknown as Enable;
}

// enableLoginPopups lets `view` open popup windows. Failures are logged: the
// sign-in still works for anything that does not need a popup.
export function enableLoginPopups(view: BrowserView): void {
  if (process.platform !== "darwin" && process.platform !== "linux") return;
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
      enable ??= loadEnable();
      enable(ptr);
    } catch (err) {
      console.error("[login-popups] popups stay disabled:", err);
    }
  };
  tryEnable();
}
