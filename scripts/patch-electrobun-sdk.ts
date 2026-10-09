// preBuild hook: apply patches/electrobun-<version>-sdk.patch to the Electrobun
// SDK that Hutch projects into .hutch/devkit, which the main process is bundled
// from. Hutch prepares the devkit before this hook runs and leaves edits to it
// in place, so an already-patched devkit is left alone. A patch that neither
// applies nor is already applied fails the build: the SDK has moved and the
// patch needs re-cutting (see patches/README.md).

import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

const root = process.cwd(); // Hutch runs hooks from the project root.
const devkit = join(".hutch", "devkit");
const { version } = JSON.parse(readFileSync(join(root, devkit, "projection.json"), "utf8")).product;
const patch = join("patches", `electrobun-${version}-sdk.patch`);
if (!existsSync(join(root, patch))) {
	throw new Error(`patch-electrobun-sdk: no ${patch} for Electrobun ${version}; see patches/README.md`);
}

const apply = (...args: string[]) =>
	execFileSync("git", ["apply", `--directory=${devkit}/api`, ...args, patch], { cwd: root, stdio: "pipe" });

try {
	apply("--check", "--reverse");
	console.log(`patch-electrobun-sdk: ${patch} already applied`);
} catch {
	try {
		apply();
	} catch (err) {
		const stderr = (err as { stderr?: Buffer }).stderr?.toString().trim();
		throw new Error(`patch-electrobun-sdk: ${patch} does not apply; re-cut it\n${stderr ?? err}`);
	}
	console.log(`patch-electrobun-sdk: applied ${patch}`);
}
