// Swap Electrobun's Linux native wrapper for our patched build.
//
// Why this exists:
//   Electrobun's Session cookie API answers from the wrong WebKit data manager
//   on Linux. The session APIs build their own manager over the partition's
//   directory rather than using the one the partition's webviews render in, so a
//   read returns a snapshot of whatever the SQLite jar held when that manager was
//   first constructed and never sees a cookie set afterwards. Desktop platform
//   login cannot finish: the user signs in, the page sets its session cookie, and
//   captureCookies() keeps reading the pre-login snapshot, so the window never
//   closes and no login reaches the daemon. patches/README.md has the diagnosis.
//   The fix is a patch to Electrobun's own nativeWrapper.cpp, so it has to ride
//   in as a rebuilt libNativeWrapper.so until upstream ships it.
//
// How this fixes it:
//   Electrobun runs this as its `preBuild` hook (see electrobun.config.ts),
//   before it copies the core binaries out of node_modules into the app bundle.
//   We overwrite the .so in that cache with the vendored one, and Electrobun
//   then bundles ours. `electrobun dev` runs the same hook, so dev builds pick it
//   up too.
//
//   Like the Windows hook this is NOT best-effort: shipping the stock .so means
//   shipping a sign-in flow that cannot finish, and nothing downstream would say
//   so. A Linux build with no vendored .so fails here instead.
//
// Rebuilding the .so: see patches/README.md.
// Remove this hook (and the patch) once Electrobun ships the fix.

import { copyFileSync, existsSync, statSync } from "node:fs";
import { join } from "node:path";

// preBuild fires for every target; only Linux builds carry a patched .so.
if (process.env.ELECTROBUN_OS !== "linux") process.exit(0);

const arch = process.env.ELECTROBUN_ARCH ?? "x64";
const root = process.cwd(); // Electrobun runs hooks from the project root.

const SO = "libNativeWrapper.so";
const patched = join(root, "desktop", "native", `linux-${arch}`, SO);
const target = join(root, "node_modules", "electrobun", `dist-linux-${arch}`, SO);

if (!existsSync(patched)) {
	throw new Error(
		`vendor-linux-native: ${patched} is missing. Linux builds need the patched ` +
			`native wrapper or platform sign-in cannot capture cookies; see patches/README.md.`,
	);
}
if (!existsSync(target)) {
	throw new Error(
		`vendor-linux-native: ${target} is missing — Electrobun's linux-${arch} core has ` +
			`not been downloaded yet, so there is nothing to replace.`,
	);
}

copyFileSync(patched, target);
console.log(
	`vendor-linux-native: installed patched ${SO} (${statSync(patched).size} bytes) into dist-linux-${arch}`,
);
