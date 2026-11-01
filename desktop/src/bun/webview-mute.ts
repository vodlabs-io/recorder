// Silences the app's hidden platform webviews. Their pages (TikTok's feed, for
// one) autoplay with sound, which would play through the speakers with no
// window in sight. MUTE_MEDIA, used as the view's preload, mutes every media
// element and keeps Web Audio suspended on all platforms; on macOS muteWebview
// also mutes the page natively (libVLLoginPopups.dylib, built from
// desktop/native/macos/webview-mute.m), which covers iframes too.

import { dlopen, FFIType, type Pointer } from "bun:ffi";
import { dirname, join } from "node:path";
import type { BrowserView } from "electrobun/main";

// MUTE_MEDIA runs before the page's own scripts. It may run twice in a frame
// and ignores the second run.
export const MUTE_MEDIA = `(function () {
  if (window.__vlMute) return; window.__vlMute = true;
  try {
    var proto = HTMLMediaElement.prototype;
    var muted = Object.getOwnPropertyDescriptor(proto, "muted");
    function silence(el) { try { muted.set.call(el, true); } catch (e) {} }
    // The page can never unmute.
    Object.defineProperty(proto, "muted", {
      configurable: true, enumerable: muted.enumerable, get: muted.get,
      set: function () { silence(this); },
    });
    var play = proto.play;
    proto.play = function () { silence(this); return play.apply(this, arguments); };
    // Elements that start on their own (autoplay) never call play().
    ["loadstart", "play", "playing", "volumechange"].forEach(function (name) {
      document.addEventListener(name, function (e) {
        if (e.target instanceof HTMLMediaElement) silence(e.target);
      }, true);
    });
  } catch (e) {}
  ["AudioContext", "webkitAudioContext"].forEach(function (name) {
    var Ctx = window[name];
    if (!Ctx) return;
    try {
      Ctx.prototype.resume = function () { return Promise.resolve(); };
      var Muted = function () {
        var ctx = new (Function.prototype.bind.apply(Ctx, [null].concat([].slice.call(arguments))))();
        try { ctx.suspend(); } catch (e) {}
        return ctx;
      };
      Muted.prototype = Ctx.prototype;
      window[name] = Muted;
    } catch (e) {}
  });
})();`;

type Mute = (view: Pointer) => void;

let nativeMute: Mute | null = null;

// A view's pointer exists only once Electrobun has registered its native view.
const PTR_POLL_MS = 50;
const PTR_TIMEOUT_MS = 5_000;

// muteWebview mutes `view`'s page natively on macOS; a no-op elsewhere, where
// MUTE_MEDIA alone silences it. Failures are logged.
export function muteWebview(view: BrowserView): void {
  if (process.platform !== "darwin") return;
  const deadline = Date.now() + PTR_TIMEOUT_MS;
  const tryMute = (): void => {
    if (view.isRemoved) return;
    const ptr = view.ptr;
    if (!ptr) {
      if (Date.now() < deadline) setTimeout(tryMute, PTR_POLL_MS);
      else console.error("[webview-mute] the view never got a native pointer");
      return;
    }
    try {
      // The library sits beside the app's own bun binary in Contents/MacOS.
      nativeMute ??= dlopen(join(dirname(process.execPath), "libVLLoginPopups.dylib"), {
        vl_webview_mute: { args: [FFIType.ptr], returns: FFIType.void },
      }).symbols.vl_webview_mute as unknown as Mute;
      nativeMute(ptr);
    } catch (err) {
      console.error("[webview-mute] native mute unavailable:", err);
    }
  };
  tryMute();
}
