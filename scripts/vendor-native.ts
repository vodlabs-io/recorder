// Swap Electrobun's Windows or Linux native wrapper in the assembled bundle for
// our patched build.
//
// Why this exists:
//   Electrobun's Session cookie API is a no-op on Windows (get() returns [],
//   set() returns false, for every partition) and on Linux answers from a stale
//   snapshot of the partition's jar. Either way desktop platform login cannot
//   finish: the user signs in and captureCookies() never sees the session
//   cookie. patches/README.md has the diagnosis. The fixes are patches to
//   Electrobun's own nativeWrapper.cpp, so they ride in as rebuilt libraries
//   until upstream ships them.
//
// How this fixes it:
//   Runs from the postBuild hook (scripts/post-build.ts), after Hutch has
//   copied its core binaries into the bundle's bin/ and before it signs and
//   packs the bundle, and overwrites the bundled library with the vendored one.
//
//   NOT best-effort: shipping the stock library means shipping a sign-in flow
//   that cannot finish, and nothing downstream would say so. A build with no
//   vendored library, or one built for another Electrobun release (its native
//   ABI changes between releases), fails here instead.
//
// Rebuilding the libraries: see patches/README.md.

import { copyFileSync, existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";

const LIBS: Record<string, string> = { win: "libNativeWrapper.dll", linux: "libNativeWrapper.so" };
const os = process.env.ELECTROBUN_OS ?? "";
const lib = LIBS[os];
if (!lib) process.exit(0);

const arch = process.env.ELECTROBUN_ARCH ?? "x64";
const root = process.cwd(); // Hutch runs hooks from the project root.
const vendored = join(root, "desktop", "native", `${os}-${arch}`);
const patched = join(vendored, lib);
const fail = (msg: string): never => {
	throw new Error(`vendor-native: ${msg}; see patches/README.md`);
};

if (!existsSync(patched)) {
	fail(`${patched} is missing, and ${os} builds need it or platform sign-in cannot capture cookies`);
}
const builtFor = readFileSync(join(vendored, "ELECTROBUN_VERSION"), "utf8").trim();
const { version } = JSON.parse(readFileSync(join(root, ".hutch", "devkit", "projection.json"), "utf8")).product;
if (builtFor !== version) fail(`${patched} was built for Electrobun ${builtFor}, this build uses ${version}`);

// The bundle is the one directory in the build folder holding bin/<lib>.
const buildDir = process.env.ELECTROBUN_BUILD_DIR ?? fail("ELECTROBUN_BUILD_DIR is unset");
const targets = readdirSync(buildDir)
	.map((name) => join(buildDir, name, "bin", lib))
	.filter((path) => existsSync(path));
if (targets.length !== 1) fail(`expected one bundle with bin/${lib} in ${buildDir}, found ${targets.length}`);

copyFileSync(patched, targets[0]!);
console.log(`vendor-native: installed patched ${lib} (${statSync(patched).size} bytes) into ${targets[0]}`);
