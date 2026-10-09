// A BrowserWindow whose own webview lives in a named storage partition.
//
// On Windows only a window's own webview is ever painted: a BrowserView added
// on top of it navigates and fires events but never appears (electrobun 1.18.1
// and 2.0.1). So the window's own webview has to carry the partition. Electrobun
// exposes no partition option for it and builds it synchronously inside the
// BrowserWindow constructor, so BrowserView's initialiser is hooked for exactly
// that one construction.

import { BrowserWindow, BrowserView, type WindowOptionsType } from "electrobun/main";

// The partition to hand the next webview built, set only for the duration of a
// single synchronous `new BrowserWindow(...)` below.
let pendingPartition: string | null = null;

type Initialisable = { init: () => unknown; partition: string | null };
const viewPrototype = BrowserView.prototype as unknown as Initialisable;
const baseInit = viewPrototype.init;

viewPrototype.init = function (this: Initialisable) {
  if (pendingPartition && !this.partition) this.partition = pendingPartition;
  return baseInit.call(this);
};

// createPartitionedWindow opens a window whose own webview loads `options.url`
// in `partition`. The returned view is that webview — the caller drives it as
// it would any BrowserView (loadURL, executeJavascript, events).
//
// Throws if the partition did not take: an electrobun upgrade that changes how
// a window builds its webview must fail loudly here rather than quietly open a
// login window on the shared default partition, which would mix a platform's
// cookies into the app's own jar and leave sign-out with nothing to wipe.
export function createPartitionedWindow(
  options: Partial<WindowOptionsType>,
  partition: string,
): { win: BrowserWindow; view: BrowserView } {
  pendingPartition = partition;
  let win: BrowserWindow;
  try {
    win = new BrowserWindow(options);
  } finally {
    pendingPartition = null;
  }

  const view = win.webview;
  if (!view || view.partition !== partition) {
    try {
      win.close();
    } catch {
      /* nothing to close */
    }
    throw new Error(
      `partitioned window: webview landed on partition "${view?.partition ?? "none"}", wanted "${partition}"`,
    );
  }
  return { win, view };
}
