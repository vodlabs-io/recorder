// React 19's *development* build emits a DevTools "Performance Track" by calling
// performance.measure() on every render/commit, gated only on console.timeStamp
// existing (see react-dom-client.development.js `supportsUserTiming`). Chrome's
// Performance panel drains those User Timing entries; the Electrobun WKWebView
// never does, so they accumulate in the timeline buffer unbounded — a steady
// memory leak whenever anything re-renders (e.g. a recording's live stats, which
// tick ~4x/s). Each emit also captures a component stack via `new Error()` and
// freezes detail objects, which pegs a core and thrashes the GC.
//
// Removing the API React probes at init flips its internal `supportsUserTiming`
// to false, disabling the track entirely. This must run before `react-dom` is
// first imported (hence the first import in main.tsx). Dev-only: the production
// react-dom build contains none of this code, so the probe is irrelevant there
// and this branch is stripped from the production bundle.
if (import.meta.env.DEV) {
  try {
    Object.defineProperty(console, "timeStamp", {
      value: undefined,
      configurable: true,
      writable: true,
    });
  } catch {
    /* console.timeStamp not redefinable; the track stays on, no harm done */
  }
}
