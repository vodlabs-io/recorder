// Authenticode-sign the Windows app bundle.
//
// Runs from the postBuild hook (scripts/post-build.ts), after the patched
// native wrapper is in place and before Hutch hashes and packs the bundle, so
// the update tarball and the installer carry signed binaries. Signs every
// .exe/.dll that isn't already validly signed (WebView2Loader.dll keeps
// Microsoft's signature). The installer's Setup.exe is stamped later and signed
// by scripts/sign-win-installer.ts; the release workflow checks both.
//
// Signing itself is scripts/sign-windows.sh; without its configuration this
// signs nothing. A failure fails the build.

import { execFileSync } from "node:child_process";
import { existsSync, readdirSync } from "node:fs";
import { join } from "node:path";

if (process.env.ELECTROBUN_OS !== "win") process.exit(0);
if (!process.env.WINDOWS_SIGN_ACCOUNT) {
	console.log("sign-win-bundle: WINDOWS_SIGN_ACCOUNT unset; building unsigned");
	process.exit(0);
}

const root = process.cwd();
const buildDir = process.env.ELECTROBUN_BUILD_DIR;
if (!buildDir) throw new Error("ELECTROBUN_BUILD_DIR unset");
// The bundle is the one directory in the build folder holding a bin/.
const bundles = readdirSync(buildDir).filter((name) => existsSync(join(buildDir, name, "bin")));
if (bundles.length !== 1) throw new Error(`expected one bundle in ${buildDir}, found ${bundles.length}`);

const listUnsigned = `
$files = @(Get-ChildItem -LiteralPath $env:SIGN_ROOT -Recurse -File -Include *.exe,*.dll | ForEach-Object FullName)
$files | Where-Object { (Get-AuthenticodeSignature -LiteralPath $_).Status -ne 'Valid' }
`;
const unsigned = execFileSync(
	"powershell",
	["-NoProfile", "-NonInteractive", "-Command", listUnsigned],
	{
		encoding: "utf8",
		env: {
			...process.env,
			SIGN_ROOT: join(buildDir, bundles[0]!),
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
