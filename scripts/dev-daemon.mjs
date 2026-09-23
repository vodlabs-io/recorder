// Dev launcher: build the Go binaries, then run the daemon.
//
// Resolves a native POSIX shell explicitly. On Windows a bare `bash` usually
// hits the WSL launcher in WindowsApps, which makes build.sh detect Linux and
// cross-build an ELF binary that cannot run on the host.

import { execFileSync, spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const isWindows = process.platform === "win32";

function gitBashFromExecPath() {
	try {
		const execPath = execFileSync("git", ["--exec-path"], { encoding: "utf8" });
		// <root>/mingw64/libexec/git-core -> <root>/bin/bash.exe
		const match = execPath.trim().replaceAll("\\", "/")
			.match(/^(.*)\/(?:mingw64|mingw32|usr)\/libexec\/git-core$/);
		return match ? join(match[1], "bin", "bash.exe") : null;
	} catch {
		return null;
	}
}

function findBash() {
	if (!isWindows) return "bash";
	const candidates = [
		process.env.GIT_BASH,
		gitBashFromExecPath(),
		"C:/Program Files/Git/bin/bash.exe",
		"C:/Program Files (x86)/Git/bin/bash.exe",
		join(process.env.LOCALAPPDATA ?? "", "Programs", "Git", "bin", "bash.exe"),
	];
	const found = candidates.find((p) => p && existsSync(p));
	if (!found) {
		console.error(
			"Could not find Git Bash. Install Git for Windows, or set GIT_BASH to its bash.exe.",
		);
		process.exit(1);
	}
	return found;
}

function run(command, args, env) {
	const result = spawnSync(command, args, {
		cwd: repoRoot,
		stdio: "inherit",
		env: { ...process.env, ...env },
	});
	if (result.error) {
		console.error(`Failed to run ${command}: ${result.error.message}`);
		process.exit(1);
	}
	if (result.status !== 0) process.exit(result.status ?? 1);
}

const passThrough = process.argv.slice(2);

run(findBash(), ["./scripts/build.sh"]);
console.log("Running daemon...");
run(join(repoRoot, isWindows ? "vlrecd.exe" : "vlrecd"), passThrough);
