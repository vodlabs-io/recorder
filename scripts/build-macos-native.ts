// Compile the app's own macOS native library into the assembled bundle, beside
// Electrobun's libNativeWrapper.dylib in Contents/MacOS.
//
// libVLLoginPopups.dylib (desktop/native/macos/*.m) gives platform sign-in
// windows real popup windows; without it "Sign in with Google" and similar
// popup sign-ins do nothing (see desktop/src/bun/login-popups.ts). It also
// mutes the hidden platform webviews (see desktop/src/bun/webview-mute.ts).
//
// Runs from the postBuild hook (scripts/post-build.ts), before Hutch signs the
// bundle, so the library is signed with it. NOT best-effort: a macOS build
// without the library ships sign-in buttons that silently do nothing.

import { existsSync, readdirSync } from "node:fs";
import { join } from "node:path";

if (process.env.ELECTROBUN_OS !== "macos") process.exit(0);

const LIB = "libVLLoginPopups.dylib";
// Matches libNativeWrapper.dylib's deployment target.
const MIN_MACOS = "14.0";

const fail = (msg: string): never => {
	throw new Error(`build-macos-native: ${msg}`);
};

const root = process.cwd(); // Hutch runs hooks from the project root.
const arch = process.env.ELECTROBUN_ARCH === "x64" ? "x86_64" : "arm64";
const buildDir = process.env.ELECTROBUN_BUILD_DIR ?? fail("ELECTROBUN_BUILD_DIR is unset");

// The bundle is the one .app in the build folder holding Electrobun's wrapper.
const targets = readdirSync(buildDir)
	.filter((name) => name.endsWith(".app"))
	.map((name) => join(buildDir, name, "Contents", "MacOS"))
	.filter((dir) => existsSync(join(dir, "libNativeWrapper.dylib")));
if (targets.length !== 1) fail(`expected one .app with Contents/MacOS/libNativeWrapper.dylib in ${buildDir}, found ${targets.length}`);

const out = join(targets[0]!, LIB);
const { exitCode } = Bun.spawnSync(
	[
		"clang", "-dynamiclib", "-fobjc-arc", "-O2", "-Wall", "-Wextra", "-Werror",
		"-arch", arch, `-mmacosx-version-min=${MIN_MACOS}`,
		"-framework", "Cocoa", "-framework", "WebKit",
		"-install_name", `@loader_path/${LIB}`,
		"-o", out,
		...["login-popups.m", "webview-mute.m"].map((src) => join(root, "desktop", "native", "macos", src)),
	],
	{ stdio: ["ignore", "inherit", "inherit"] },
);
if (exitCode !== 0) fail(`clang exited with ${exitCode}`);
console.log(`build-macos-native: built ${out}`);
