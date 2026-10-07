// Electrobun's postBuild hook. It takes one script, so this runs each step in
// order: the icon must be embedded before signing, which seals the binaries.

import { join } from "node:path";

for (const script of ["embed-win-icon.ts", "sign-win-bundle.ts"]) {
	const { exitCode } = Bun.spawnSync([process.execPath, join(import.meta.dir, script)], {
		stdio: ["ignore", "inherit", "inherit"],
	});
	if (exitCode !== 0) throw new Error(`${script} failed with exit code ${exitCode}`);
}
