// macOS injects a view's custom preload into its main frame only, while the
// stream sniffer has to see embedded players, which usually sit in another
// site's iframe. Electrobun's native wrapper exports addPreloadScriptToWebView,
// which adds a script to every frame, but the SDK does not expose it, so it is
// called here directly. Windows already runs a custom preload in every frame;
// Linux's WebKitGTK wrapper has no all-frames injection.

import { dlopen, FFIType, type Pointer } from "bun:ffi";
import { dirname, join } from "node:path";
import type { BrowserView } from "electrobun/main";

type NativeWrapper = {
  symbols: { addPreloadScriptToWebView: (view: Pointer, script: Buffer, mainFrameOnly: boolean) => void };
};

let native: NativeWrapper | null = null;

// The native webview is built asynchronously, after its BrowserView, so the
// script goes in once the view's first page has loaded, or after this long.
const READY_TIMEOUT_MS = 3_000;

// addAllFramesPreload adds `script` to every frame `view` loads after its first
// page. The main frame also gets the view's own `preload`, so the script must
// tolerate running twice there. Resolves false where this is unsupported or
// failed, leaving the script in the main frame only.
export function addAllFramesPreload(view: BrowserView, script: string): Promise<boolean> {
  if (process.platform !== "darwin") return Promise.resolve(false);
  return new Promise((resolve) => {
    let done = false;
    const add = () => {
      if (done) return;
      done = true;
      resolve(inject(view, script));
    };
    view.on("dom-ready", add);
    setTimeout(add, READY_TIMEOUT_MS);
  });
}

function inject(view: BrowserView, script: string): boolean {
  try {
    const ptr = view.ptr;
    if (!ptr) throw new Error("the view has no native pointer");
    // The app's own bun binary sits beside the native wrapper in Contents/MacOS.
    native ??= dlopen(join(dirname(process.execPath), "libNativeWrapper.dylib"), {
      addPreloadScriptToWebView: { args: [FFIType.ptr, FFIType.cstring, FFIType.bool], returns: FFIType.void },
    }) as unknown as NativeWrapper;
    native.symbols.addPreloadScriptToWebView(ptr, Buffer.from(`${script}\0`), false);
    return true;
  } catch (err) {
    console.error("[all-frames-preload] falling back to the main frame only:", err);
    return false;
  }
}
