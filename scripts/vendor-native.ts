// preBuild entry point: prepare Electrobun's downloaded core binaries for the
// target OS before they're bundled.
//
// Electrobun runs exactly one preBuild script (it spawns it directly, with no
// shell), so this dispatches to the per-OS step and passes its exit code
// through: a failed step still fails the build. Windows and Linux get a
// patched native wrapper; macOS gets room for the code signature in its Intel
// binaries.

const scripts: Record<string, string> = {
	win: "scripts/vendor-win-native.ts",
	linux: "scripts/vendor-linux-native.ts",
	macos: "scripts/fix-macos-headerpad.ts",
};

const script = scripts[process.env.ELECTROBUN_OS ?? ""];
if (!script) process.exit(0);

const result = Bun.spawnSync([process.execPath, script], {
	stdio: ["ignore", "inherit", "inherit"],
	env: process.env,
});
process.exit(result.exitCode ?? 1);
