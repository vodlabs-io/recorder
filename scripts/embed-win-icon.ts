// Embed the Windows app icon into the binaries a Windows build ships.
//
// Why this exists:
//   Electrobun (<= 1.18.1) embeds the .ico via `rcedit`, but its precompiled
//   CLI resolves `rcedit` with a path baked in at *its* build time, so it throws
//   and silently ships no icon (https://github.com/blackboardsh/electrobun/pull/433,
//   unmerged). bun.exe owns the app window, so an unbranded one puts the Bun
//   logo on the taskbar button whenever the window has no icon of its own (see
//   desktop/src/bun/win-window-icon.ts), and Explorer draws it on the files.
//
// How this fixes it:
//   Electrobun runs this as its `postBuild` hook (see electrobun.config.ts),
//   after assembling the app bundle and before packing it. We brand the
//   bundle's binaries with node_modules/rcedit. The bundle, not node_modules,
//   because `build.bunVersion` copies a separately downloaded bun.exe over the
//   bundled one. extractor.exe is the exception: the installer is stamped out
//   of it from node_modules later in the same build.
//
//   Best-effort: the icon is cosmetic and must never break a release, so we
//   warn and carry on.
//
// Remove this hook once Electrobun ships PR #433.

import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { basename, join } from "node:path";

// Hooks fire for every target; only Windows builds need the workaround.
if (process.env.ELECTROBUN_OS !== "win") process.exit(0);

const arch = process.env.ELECTROBUN_ARCH ?? "x64";
const root = process.cwd(); // Electrobun runs hooks from the project root.

const icon = join(root, "desktop", "icon.ico");
const rcedit = join(root, "node_modules", "rcedit", "bin", "rcedit-x64.exe");

const warn = (msg: string) => console.warn(`embed-win-icon: ${msg}`);

if (!existsSync(icon) || !existsSync(rcedit)) {
	warn(`icon or rcedit missing (${icon}, ${rcedit}); skipping`);
	process.exit(0);
}

// Electrobun passes the build folder and the bundle's name to every hook; on
// Windows the bundle's binaries live in <buildDir>/<appName>/bin.
const buildDir = process.env.ELECTROBUN_BUILD_DIR;
const appName = process.env.ELECTROBUN_APP_NAME;
const bundleBin = buildDir && appName ? join(buildDir, appName, "bin") : null;
if (!bundleBin) warn("ELECTROBUN_BUILD_DIR/ELECTROBUN_APP_NAME unset; skipping the app bundle");

const targets = [
	// The app's own exe, and the interpreter that owns the app window.
	...(bundleBin ? [join(bundleBin, "launcher.exe"), join(bundleBin, "bun.exe")] : []),
	// The installer stub, read from node_modules later in this same build.
	join(root, "node_modules", "electrobun", `dist-win-${arch}`, "extractor.exe"),
];

for (const exe of targets) {
	const name = basename(exe);
	try {
		if (!existsSync(exe)) {
			warn(`binary not found, skipping: ${exe}`);
			continue;
		}
		execFileSync(rcedit, [exe, "--set-icon", icon], { stdio: "inherit" });
		console.log(`embed-win-icon: embedded icon into ${name}`);
	} catch (err) {
		warn(`failed to embed ${name}: ${err}`);
	}
}
