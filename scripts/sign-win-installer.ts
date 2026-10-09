// Authenticode-sign the Windows installer's Setup.exe.
//
// Runs as Hutch's postPackage hook. Hutch stamps Setup.exe out of its own
// extractor and embeds the icon in it while packaging, after postBuild, and
// ships it zipped beside its .installer payload folder. So the zip is unpacked,
// Setup.exe signed, and the zip rebuilt with the same layout.
//
// Signing itself is scripts/sign-windows.sh; without its configuration this
// signs nothing. A failure fails the build.

import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

if (process.env.ELECTROBUN_OS !== "win" || process.env.ELECTROBUN_BUILD_ENV === "dev") process.exit(0);
if (!process.env.WINDOWS_SIGN_ACCOUNT) {
	console.log("sign-win-installer: WINDOWS_SIGN_ACCOUNT unset; leaving the installer unsigned");
	process.exit(0);
}

const root = process.cwd();
const artifacts = process.env.ELECTROBUN_ARTIFACT_DIR;
if (!artifacts) throw new Error("ELECTROBUN_ARTIFACT_DIR unset");
const zips = readdirSync(artifacts).filter((name) => /Setup.*\.zip$/.test(name));
if (zips.length === 0) throw new Error(`sign-win-installer: no Setup zip in ${artifacts}`);

const gitBash = "C:\\Program Files\\Git\\bin\\bash.exe";
const bash = existsSync(gitBash) ? gitBash : "bash";
const powershell = (command: string, env: Record<string, string>) =>
	execFileSync("powershell", ["-NoProfile", "-NonInteractive", "-Command", command], {
		stdio: "inherit",
		env: { ...process.env, ...env },
	});

for (const zip of zips) {
	const zipPath = join(artifacts, zip);
	const staging = mkdtempSync(join(tmpdir(), "setup-sign-"));
	try {
		powershell("Expand-Archive -LiteralPath $env:ZIP -DestinationPath $env:STAGING -Force", {
			ZIP: zipPath,
			STAGING: staging,
		});
		const setups = readdirSync(staging).filter((name) => /Setup.*\.exe$/.test(name));
		if (setups.length !== 1) throw new Error(`sign-win-installer: expected one Setup exe in ${zip}`);
		const setup = join(staging, setups[0]!);
		console.log(`sign-win-installer: signing ${setups[0]} from ${zip}`);
		execFileSync(bash, [join(root, "scripts", "sign-windows.sh"), setup], { stdio: "inherit" });
		// Both entries by path: the payload folder is hidden, which a wildcard skips.
		powershell(
			"Compress-Archive -Path @($env:SETUP, (Join-Path $env:STAGING '.installer')) -DestinationPath $env:ZIP -Force",
			{ ZIP: zipPath, STAGING: staging, SETUP: setup },
		);
	} finally {
		rmSync(staging, { recursive: true, force: true });
	}
}
