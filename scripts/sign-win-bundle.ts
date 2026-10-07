// Authenticode-sign the Windows app bundle and the installer stub.
//
// Runs from the postBuild hook (scripts/post-build.ts), after the icon is
// embedded and before Electrobun hashes and packs the bundle, so the update
// tarball and the installer carry signed binaries. Signs every .exe/.dll that
// isn't already validly signed (WebView2Loader.dll keeps Microsoft's
// signature), plus extractor.exe, which Electrobun copies into Setup.exe later
// in this build. The release workflow checks the finished Setup.exe.
//
// Signing itself is scripts/sign-windows.sh; without its configuration this
// signs nothing. A failure fails the build.

import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { join } from "node:path";

if (process.env.ELECTROBUN_OS !== "win") process.exit(0);
if (!process.env.WINDOWS_SIGN_ACCOUNT) {
	console.log("sign-win-bundle: WINDOWS_SIGN_ACCOUNT unset; building unsigned");
	process.exit(0);
}

const root = process.cwd();
const arch = process.env.ELECTROBUN_ARCH ?? "x64";
const { ELECTROBUN_BUILD_DIR: buildDir, ELECTROBUN_APP_NAME: appName } = process.env;
if (!buildDir || !appName) throw new Error("ELECTROBUN_BUILD_DIR/ELECTROBUN_APP_NAME unset");

const listUnsigned = `
$files = @(Get-ChildItem -LiteralPath $env:SIGN_ROOT -Recurse -File -Include *.exe,*.dll | ForEach-Object FullName) + $env:SIGN_EXTRA
$files | Where-Object { (Get-AuthenticodeSignature -LiteralPath $_).Status -ne 'Valid' }
`;
const unsigned = execFileSync(
	"powershell",
	["-NoProfile", "-NonInteractive", "-Command", listUnsigned],
	{
		encoding: "utf8",
		env: {
			...process.env,
			SIGN_ROOT: join(buildDir, appName),
			SIGN_EXTRA: join(root, "node_modules", "electrobun", `dist-win-${arch}`, "extractor.exe"),
		},
	},
)
	.split(/\r?\n/)
	.map((line) => line.trim())
	.filter(Boolean);

console.log(`sign-win-bundle: signing ${unsigned.length} file(s)`);
for (const file of unsigned) console.log(`  ${file}`);
// Git Bash by path: a bare `bash` can resolve to WSL's System32\bash.exe.
const gitBash = "C:\\Program Files\\Git\\bin\\bash.exe";
const bash = existsSync(gitBash) ? gitBash : "bash";
execFileSync(bash, [join(root, "scripts", "sign-windows.sh"), ...unsigned], { stdio: "inherit" });
