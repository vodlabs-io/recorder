// Swap Electrobun's Windows native wrapper for our patched build.
//
// Why this exists:
//   Electrobun's Session cookie API is a no-op on Windows — get() returns [],
//   set() returns false, for every partition — which leaves desktop platform
//   login unable to complete: the sign-in window renders, the user signs in, and
//   captureCookies() still sees nothing. patches/README.md has the diagnosis.
//   The fix is a patch to Electrobun's own nativeWrapper.cpp, so it has to ride
//   in as a rebuilt libNativeWrapper.dll until upstream ships it.
//
// How this fixes it:
//   Electrobun runs this as its `preBuild` hook (see electrobun.config.ts),
//   before it copies the core binaries out of node_modules into the app bundle.
//   We overwrite the DLL in that cache with the vendored one, and Electrobun
//   then bundles ours.
//
//   Unlike the icon hook this is NOT best-effort: shipping the stock DLL means
//   shipping a sign-in flow that cannot finish, and nothing downstream would say
//   so. A Windows build with no vendored DLL fails here instead.
//
// Rebuilding the DLL: see patches/README.md.
// Remove this hook (and the patch) once Electrobun ships the fix.

import { copyFileSync, existsSync, statSync } from "node:fs";
import { join } from "node:path";

// preBuild fires for every target; only Windows builds carry a patched DLL.
if (process.env.ELECTROBUN_OS !== "win") process.exit(0);

const arch = process.env.ELECTROBUN_ARCH ?? "x64";
const root = process.cwd(); // Electrobun runs hooks from the project root.

const DLL = "libNativeWrapper.dll";
const patched = join(root, "desktop", "native", `win-${arch}`, DLL);
const target = join(root, "node_modules", "electrobun", `dist-win-${arch}`, DLL);

if (!existsSync(patched)) {
	throw new Error(
		`vendor-win-native: ${patched} is missing. Windows builds need the patched ` +
			`native wrapper or platform sign-in cannot capture cookies; see patches/README.md.`,
	);
}
if (!existsSync(target)) {
	throw new Error(
		`vendor-win-native: ${target} is missing — Electrobun's win-${arch} core has ` +
			`not been downloaded yet, so there is nothing to replace.`,
	);
}

copyFileSync(patched, target);
console.log(
	`vendor-win-native: installed patched ${DLL} (${statSync(patched).size} bytes) into dist-win-${arch}`,
);
