// Hutch's postBuild hook. It takes one script, so this runs each step in order:
// native libraries must be in place before signing seals the bundle.

import { join } from "node:path";

for (const script of ["vendor-native.ts", "build-macos-native.ts", "sign-win-bundle.ts"]) {
	const { exitCode } = Bun.spawnSync([process.execPath, join(import.meta.dir, script)], {
		stdio: ["ignore", "inherit", "inherit"],
	});
	if (exitCode !== 0) throw new Error(`${script} failed with exit code ${exitCode}`);
}
