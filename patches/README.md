# Patches

Source patches we carry against third-party code, with the built artefacts they
produce. Each one exists because a bug upstream blocks a feature here and the
fix has not landed (or shipped) yet.

## `electrobun@1.18.1.patch`

Against the published npm package, file `dist/api/bun/proc/native.ts`. Applied
by `bun install` itself (`patchedDependencies` in `package.json`), so it needs
no build step and covers every platform. It lets the app run on Bun 1.4
(`bunVersion` in `electrobun.config.ts`) while staying on Electrobun 1.18.1.

### Why Bun 1.4

Bun 1.3.x corrupts JavaScriptCore's strong-handle list whenever native code
calls a `threadsafe` JSCallback: `FFI_Callback_threadsafe_call` copies the
callback's wrapper, and with it two `JSC::Strong` handles, on the calling thread
with no lock (oven-sh/bun `src/jsc/bindings/JSFFIFunction.cpp`; fixed in
88403d981, first released in 1.4.0). Electrobun delivers every window and
webview event that way from the UI thread, so the app died after hours of use:
in the GC marker (`HandleSet::visitStrongHandles`), in the Worker, or with the
collector spinning at full CPU.

A standalone reproduction (a pthread calling a threadsafe JSCallback, no
Electrobun) crashed 10 of 10 runs within a second on 1.3.14, and survived 10 of
10 on 1.4.2 plus ten minutes under `BUN_JSC_collectContinuously=1`.

### What the patch changes

1. **cstring arguments.** Bun 1.4 passes `FFIType.cstring` callback arguments
   as strings rather than pointers. Electrobun read all eleven with
   `new CString(ptr)`, which now throws — inside the catch blocks of the webview
   event callback and the RPC bridges, so every webview event and every
   webview-to-bun RPC would vanish without an error. `ffiCStringToString`
   accepts either form, as Electrobun 2.x does.

2. **Exceptions in threadsafe callbacks.** On Bun 1.4 an exception escaping a
   threadsafe JSCallback kills the Worker (with nothing logged) or segfaults;
   one throw was enough to reproduce it. App listeners (`win.on`, `view.on`,
   tray, menus) run inside these callbacks and Node's `EventEmitter` does not
   catch, so a bug in any listener would take down the whole app.
   `guardedJSCallback` logs the error instead. The three bridge handlers already
   caught everything, and the synchronous, non-threadsafe callbacks are
   untouched.

3. **A custom preload in every frame.** On macOS the native side injects an
   app's custom `preload` into the main frame only (`forMainFrameOnly:true` in
   `updateCustomPreloadScript`), while Electrobun's own preload goes to every
   frame. The stream sniffer (`desktop/src/bun/stream-sniff.ts`) has to see
   embedded players, which are usually another site's iframe: measured on 30
   live TV pages, main-frame-only sniffing found 25 streams and all-frames 30.
   A custom preload that opens with `/* electrobun:all-frames */` is now
   appended to Electrobun's preload instead, so it reaches every frame on
   macOS and Windows with no native rebuild. Linux's WebKitGTK wrapper injects
   both preloads into the top frame only, so iframe players stay out of reach
   there.

Moving to Electrobun 2.x makes the first change redundant. It does not carry
the second or the third, so check before dropping them.

## `electrobun-1.18.1-win-cookie-api.patch`

Against `blackboardsh/electrobun` at tag `v1.18.1`, file
`package/src/native/win/nativeWrapper.cpp`. Makes the `Session` cookie API work
on Windows.

`Session.fromPartition(p).cookies.get()` returned `[]` and `.set()` returned
`false` for every partition, while the cookies were plainly present in the
profile on disk. That is the whole mechanism behind desktop platform login: the
sign-in window can render perfectly and still never complete, because
`captureCookies()` never sees a session cookie. It also breaks `wipePartition()`
and the browser-context signed-fetch relay, which round-trips its result through
cookies. Reproduced on 1.18.1 and on 2.0.1, so an upgrade does not fix it.

Three defects, all in the five `session*` exports:

1. **Wrong thread.** Every `ICoreWebView2` interface is apartment-threaded and
   belongs to the thread running the message loop in `startEventLoop`. These
   exports are called over FFI from the app's bun Worker — a different thread —
   so they were making cross-apartment calls on raw pointers, and the completion
   handlers they scheduled were delivered to a queue the caller was not
   draining. Every other webview API in that file already hops across with
   `MainThreadDispatcher`. Fixed by routing through `onUIThread` and waiting with
   `CoWaitForMultipleHandles` so the UI thread keeps dispatching while it blocks.

2. **Wrong partition.** Each partition is its own WebView2 environment with its
   own jar, and the lookups took "whichever view is first in the map" — so at
   best another partition's cookies, at worst a view with none. Views now record
   the partition they were created with, and the lookups match on it.

3. **Registry keyed per window.** `g_webview2Views` was keyed by the container
   HWND, which every view in a window shares, so a second view evicted the
   first; nothing ever erased an entry either, leaving the session APIs walking
   raw pointers to freed views after a window closed. Now keyed by webview id
   and erased in `remove()`.

The patch also JSON-escapes cookie values on the way out (they were interpolated
raw, so one quote or backslash produced JSON the JS side silently read as `[]`).

## Same-origin policy in named partitions

Carried in the same patch file, because it is the same file and the same build.

Electrobun passed `--disable-web-security` to every WebView2 environment on
Windows. Unlike current Chrome, which now ignores the flag, WebView2 honours it:
verified by a cross-origin `fetch` to a no-CORS origin returning 200 inside a
login webview. So every third-party page we host — a platform's own login page,
with the user's session cookies — ran with the same-origin policy switched off,
free to read any origin it liked.

The flag is now given only to the default partition, where the app's own trusted
pages live and where the `views://` scheme and file dialogs were set up against
that behaviour. The condition is the partition rather than the view's `sandbox`
flag on purpose: WebView2 keys an environment by its user data folder, which
comes from the partition, and a second environment for the same folder with
different options fails outright — only a per-partition decision stays
consistent.

Verified after the rebuild: a named partition reports `SOP ENFORCED`, the default
partition is unchanged.

This was investigated as a suspect for TikTok's QR login failing on Windows. It
is not the cause — a fresh Chrome profile on the same machine got the same
`error_code: 7` "Maximum number of attempts reached" from
`passport/web/check_qrconnect`, so that limit is scoped to the IP or device, not
the browser. Worth fixing on its own terms regardless.

Verified on this machine after the rebuild: 7 cookies read back from the right
partition with HttpOnly values intact (`ttwid`, `tt_csrf_token`), an unrelated
partition reading 0, and set/remove round-tripping. Reads settle at 3-9ms once the
page is up. A read issued while the webview is still initialising waits out the 5s
timeout and returns empty — the wait pumps messages so the UI stays responsive, and
the login path only reads on navigation events, so it does not hit this.

## The event bridge in sandboxed views

Carried in the same patch file.

Touching `chrome.webview.hostObjects` at all breaks some sites. TikTok's QR
login is one: the page starts rejecting its own polling endpoint
(`check_qrconnect` answering `error_code: 7`) the moment the page's JS
environment has been through that proxy, which made desktop sign-in impossible
on Windows however long you waited. Electrobun's preload reached for it on every
page, to find the event bridge:

    window.__electrobunEventBridge = ... || window.chrome?.webview?.hostObjects?.eventBridge;

Established by bisection against a bare WebView2 host (a window, a WebView2, a
URL, no Electrobun), which polls 16/16 successfully. Adding Electrobun's browser
arguments, its `views://` scheme registration, or its host objects changes
nothing; adding its preload takes it to 0/17. Within the preload it is none of
the four init calls -- it is the bridge lookup they share. Reading the bare
container property is enough, and it fails whether or not the host object is
actually registered. A trivial injected script and plain globals are both fine.

Sandboxed views now reach the bridge through `chrome.webview.postMessage`,
which is safe even when called. The shim is registered as a document-created
script BEFORE the preload, so the preload's "||" chain short-circuits on
`window.__electrobunEventBridge` and never evaluates the hostObjects fallback;
a new `add_WebMessageReceived` handler routes those messages to the same
callback the host object fed. Trusted views are untouched -- they carry our own
pages and need the RPC bridges.

A lazy getter instead of a shim does not work: fingerprinting code enumerates
`window`, which fires the getter (0/16).

Verified end to end: the app's login webview went from 2 successes then 14
errors to 16/16 -- cleaner than a fresh Edge sampled at the same moment (9/3) --
`dom-ready` still arrives over the new transport (browser-context depends on
it), and a real QR sign-in completed with the account showing Active.

### The built artefact

`desktop/native/win-x64/libNativeWrapper.dll` is the patched build, committed
deliberately (`.gitignore` has an exception for it) because a Windows build
without it silently ships a sign-in flow that cannot finish.
`scripts/vendor-win-native.ts` installs it over the copy in `node_modules` during
Electrobun's `preBuild` hook, and fails the build rather than falling back.

The committed copy is `libNativeWrapper.dll`, SHA-256
`7cb81929f7e6f744e813b327da1a3d78fd51130dfa2c48db043d20b362274709`, built from
electrobun `v1.18.1` (`4eba723c85b97559e1d9e13439d9a92ede0832e8`) with this
patch applied — replacing an earlier local build that embedded a developer's
own `C:\Users\rs\...` paths with no record of how it was produced.

### Rebuilding

`.github/workflows/electrobun-native-win.yml` checks out the pinned electrobun
tag, applies this patch, and builds. Run it from the Actions tab. Two things
that bit us building this DLL, both already fixed in the workflow:

- **Runner image**: `windows-2025` now ships Visual Studio 2026 (v18), but
  electrobun `v1.18.1`'s `build.ts` hardcodes the CMake generator string
  `"Visual Studio 17 2022"` — CMake fails with "could not find any instance of
  Visual Studio" when only v18 is present. The workflow now pins
  `windows-2022`, which still ships the matching toolchain.
- **Silent hangs**: a run can stall indefinitely with no error (observed 2+
  hours at the CEF extraction step, which normally takes ~15s). The workflow
  now sets `timeout-minutes: 30` so a stuck run fails instead of burning
  Actions minutes unnoticed.

**Building locally**, e.g. on a spare Windows machine, is a faster iteration
loop than a CI round trip and was how this DLL was actually produced (CI kept
stalling). Needs Visual Studio 2022 Build Tools (`winget install
Microsoft.VisualStudio.2022.BuildTools --override "--add
Microsoft.VisualStudio.Workload.VCTools --includeRecommended"`), CMake
(`winget install Kitware.CMake`), and Node.js (`winget install
OpenJS.NodeJS.LTS`) — `build.ts` shells out to `node` for one vendoring step,
which a bun-only machine won't have and which no error message here points at
directly (it surfaces as "Failed to download electrobun-dawn binaries").
Bun alone is not enough.

    git clone --branch v1.18.1 --recurse-submodules ^
        https://github.com/blackboardsh/electrobun.git electrobun
    cd electrobun
    git apply ..\recorder\patches\electrobun-1.18.1-win-cookie-api.patch
    cd package
    bun install
    call "C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools\VC\Auxiliary\Build\vcvarsall.bat" x64
    bun build.ts --release

`dist\libNativeWrapper.dll` is the result. A local build embeds that
machine's own paths in its debug info (unavoidable with MSVC unless the PDB
path is scrubbed) — harmless, but worth naming the machine and recording the
checksum here so the artefact's origin stays traceable either way.

When bumping electrobun, re-cut the patch against the new tag, update the tag
in the workflow, and update the checksum and commit hash above.

## `electrobun-1.18.1-linux-cookie-api.patch`

Against `blackboardsh/electrobun` at tag `v1.18.1`, file
`package/src/native/linux/nativeWrapper.cpp`. Makes the `Session` cookie API
work on Linux.

Same feature broken as on Windows, different cause, and it does not announce
itself: reads succeed and return plausible-looking cookies, just never the ones
that were set after the first read. Desktop platform sign-in cannot finish —
the QR scan or password is accepted, the platform sets its session cookie, and
`captureCookies()` keeps reading a jar that no longer exists, so the window
never closes and no login reaches the daemon. It also blocks the browser-context
signed-fetch relay, which round-trips its result through cookies, and blinds
`wipePartition()`'s survivor sweep into a false all-clear.

Two defects in the five `session*` exports:

1. **The wrong data manager.** `getContextForPartition()` (what webviews render
   in) and `getDataManagerForPartition()` (what the session APIs read) each built
   their *own* `WebKitWebsiteDataManager` over the same directory, cached in two
   different maps. Two managers over one directory are two independent network
   sessions: WebKitGTK's SQLite jar is loaded into memory when a manager is
   constructed and written through thereafter, so the session APIs answered from
   a snapshot frozen at whatever was on disk the first time anything touched the
   partition — and never saw a cookie set afterwards. Measured on a fresh
   partition loading a page that sets 12 cookies: a read at t=1s returned 1, and
   still returned 1 at t=18s. The session APIs now resolve the partition's web
   context and use *its* manager, so there is one per partition.

   The ordering made it look intermittent. `runPlatformLogin()` wipes the
   partition before opening the login window on a fresh sign-in, and that wipe is
   itself a session call — so it took the snapshot before the login page had
   loaded, and a first-time sign-in could never succeed while a repeat one
   sometimes could.

2. **No way to ask for the whole jar.** `sessionGetCookies` substituted
   `https://localhost` when the filter carried no `url`, so `cookies.get({})` —
   which on macOS and patched-Windows enumerates the partition — returned `[]`
   on Linux for a jar full of cookies. WebKitGTK genuinely has no "all cookies"
   call, so the fix asks the data manager which domains hold cookies
   (`webkit_website_data_manager_fetch`) and queries each in turn, merging on
   name+domain+path. Cookies scoped to a path below the root are the one thing
   this cannot reach; session cookies are not.

The patch also JSON-escapes cookie values on the way out — the same defect the
Windows patch fixed, and the same consequence: one quote or backslash produces
JSON the JS side silently reads as `[]`.

### The built artefact

`desktop/native/linux-x64/libNativeWrapper.so` is the patched build, committed
deliberately (`.gitignore` has an exception for it) because a Linux build without
it silently ships a sign-in flow that cannot finish. `scripts/vendor-native.ts`
installs it over the copy in `node_modules` during Electrobun's `preBuild` hook,
and fails the build rather than falling back. That hook runs for `electrobun dev`
too, so dev builds get it as well.

### Rebuilding

`.github/workflows/electrobun-native-linux.yml` checks out the pinned electrobun
tag, applies this patch, builds on `ubuntu-22.04`, and uploads
`libNativeWrapper.so` as an artifact. Run it from the Actions tab, then commit
the artifact to `desktop/native/linux-x64/`.

The committed artefact should come from that workflow rather than a local build:
22.04 keeps the glibc baseline low enough for the distros we ship to.

**Iterating on the patch locally** is much faster than a CI round trip, and only
needs headers — the GTK-only wrapper compiles against CEF but does not link it,
so there is no need for electrobun's full vendoring (a bun/zig toolchain and the
CEF binary distribution):

    sudo apt install build-essential cmake pkg-config libgtk-3-dev \
        libwebkit2gtk-4.1-dev libayatana-appindicator3-dev librsvg2-dev \
        libx11-dev libxext-dev

    git clone --depth 1 --branch v1.18.1 \
        https://github.com/blackboardsh/electrobun.git src
    cd src/package
    # CEF headers only (~4MB of a ~150MB stream), and the dawn headers
    mkdir -p vendors/cef && curl -sL \
      "https://cef-builds.spotifycdn.com/cef_binary_147.0.10%2Bgd58e84d%2Bchromium-147.0.7727.118_linux64_minimal.tar.bz2" \
      | tar -xj --strip-components=1 -C vendors/cef --wildcards "*/include/*"
    mkdir -p vendors/wgpu && curl -sL \
      "https://github.com/blackboardsh/electrobun-dawn/releases/download/v0.2.3/electrobun-dawn-linux-x64.tar.gz" \
      | tar -xz -C vendors/wgpu
    cp ../../../recorder/node_modules/electrobun/dist-linux-x64/libasar.so vendors/

Then compile and link (seconds per iteration):

    g++ -c -std=c++20 -fPIC $(pkg-config --cflags webkit2gtk-4.1 gtk+-3.0 \
        ayatana-appindicator3-0.1) -Ivendors/cef -Ivendors/wgpu/linux-x64/include \
        -o src/native/linux/build/nativeWrapper.o src/native/linux/nativeWrapper.cpp
    g++ -shared -o src/native/build/libNativeWrapper.so \
        src/native/linux/build/nativeWrapper.o vendors/libasar.so \
        $(pkg-config --libs webkit2gtk-4.1 gtk+-3.0 ayatana-appindicator3-0.1) \
        -ldl -lpthread

Drop the result into a built app's `bin/` to test it. Expect deprecation
warnings from electrobun's own GTK and appindicator calls; they are pre-existing.

When bumping electrobun, re-cut the patch against the new tag and update the tag
in that workflow.
