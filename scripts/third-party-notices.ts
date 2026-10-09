// Regenerates THIRD_PARTY_NOTICES.md: the hand-written notices below, then the
// license text of every Go module linked into vod and vlrecd and every npm
// package bundled into the desktop app. Run it after changing dependencies:
//
//   bun scripts/third-party-notices.ts
import { existsSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

const root = join(import.meta.dir, "..");
const out = join(root, "THIRD_PARTY_NOTICES.md");

// Files whose text a notice has to carry, matched against a package's top level.
const LICENSE_FILE = /^(licen[cs]e|copying|notice|unlicense)([.-].*)?$/i;

type Entry = { name: string; version: string; license?: string; texts: { file: string; text: string }[] };

function licenseTexts(dir: string): Entry["texts"] {
	return readdirSync(dir)
		.filter((f) => LICENSE_FILE.test(f) && statSync(join(dir, f)).isFile())
		.sort()
		.map((f) => ({ file: f, text: readFileSync(join(dir, f), "utf8").replace(/\r\n?/g, "\n").trim() }));
}

function run(cmd: string[], env: Record<string, string> = {}): string {
	const p = Bun.spawnSync(cmd, { cwd: root, env: { ...process.env, ...env }, stderr: "pipe" });
	if (p.exitCode !== 0) throw new Error(`${cmd.join(" ")}: ${p.stderr.toString()}`);
	return p.stdout.toString();
}

// Go: every module either binary links on any platform it ships for.
function goModules(): Entry[] {
	const seen = new Map<string, Entry>();
	const targets = ["darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64"];
	for (const target of targets) {
		const [GOOS, GOARCH] = target.split("/");
		const list = run(
			["go", "list", "-e", "-deps", "-f",
				"{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}",
				"./cmd/cli", "./cmd/daemon"],
			{ GOOS, GOARCH },
		);
		for (const line of list.split("\n")) {
			const [path, version, dir] = line.split("\t");
			if (!path || seen.has(path)) continue;
			if (!dir) throw new Error(`${path}@${version} is not downloaded; run go mod download`);
			seen.set(path, { name: path, version, texts: licenseTexts(dir) });
		}
	}
	const goroot = run(["go", "env", "GOROOT"]).trim();
	const goVersion = run(["go", "env", "GOVERSION"]).trim();
	const std = { name: "Go standard library and runtime", version: goVersion, texts: licenseTexts(goroot) };
	return [std, ...[...seen.values()].sort((a, b) => a.name.localeCompare(b.name))];
}

// npm: the app's runtime dependencies and what they depend on. Electrobun's own
// dependencies are its build tooling, so only Electrobun itself is walked.
// tailwindcss and tw-animate-css are dev dependencies whose CSS lands in the
// bundle.
function npmPackages(): Entry[] {
	const pkg = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
	const seen = new Map<string, Entry>();
	const resolve = (name: string, from: string): string | undefined => {
		for (let d = from; ; d = dirname(d)) {
			const candidate = join(d, "node_modules", name);
			if (existsSync(join(candidate, "package.json"))) return candidate;
			if (d === root || d === dirname(d)) return undefined;
		}
	};
	const visit = (name: string, from: string, walkDeps: boolean) => {
		const dir = resolve(name, from);
		if (!dir) return; // an optional or platform-specific package not installed here
		const meta = JSON.parse(readFileSync(join(dir, "package.json"), "utf8"));
		const key = `${meta.name}@${meta.version}`;
		if (seen.has(key)) return;
		const license = typeof meta.license === "string" ? meta.license : meta.license?.type;
		let texts = licenseTexts(dir);
		// A package that publishes no license file can have its upstream one
		// vendored here.
		const vendored = join(import.meta.dir, "licenses", `${meta.name}.txt`);
		if (texts.length === 0 && existsSync(vendored)) {
			texts = [{ file: `LICENSE (from its source repository)`, text: readFileSync(vendored, "utf8").trim() }];
		}
		seen.set(key, { name: meta.name, version: meta.version, license, texts });
		if (!walkDeps) return;
		const deps = { ...meta.dependencies, ...meta.peerDependencies, ...meta.optionalDependencies };
		for (const dep of Object.keys(deps)) visit(dep, dir, true);
	};
	for (const name of Object.keys(pkg.dependencies)) visit(name, root, name !== "electrobun");
	for (const name of ["tailwindcss", "tw-animate-css"]) visit(name, root, false);
	return [...seen.values()].sort((a, b) => a.name.localeCompare(b.name));
}

function section(entries: Entry[]): string {
	return entries
		.map((e) => {
			const head = `### ${e.name} ${e.version}` + (e.license ? ` (${e.license})` : "");
			if (e.texts.length === 0) return `${head}\n\nThe package ships no license file.\n`;
			const body = e.texts.map((t) => `${t.file}:\n\n\`\`\`text\n${t.text}\n\`\`\`\n`).join("\n");
			return `${head}\n\n${body}`;
		})
		.join("\n");
}

const intro = `# Third-party notices

VodLabs Recorder is licensed under the PolyForm Noncommercial License 1.0.0
(see [LICENSE](LICENSE)). It includes the third-party software below, which
keeps its own license. This file is generated by
\`scripts/third-party-notices.ts\`; edit the script, not this file.

## FFmpeg and x264

The \`vod\` and \`vlrecd\` binaries embed FFmpeg, built with the x264 encoder,
and run it as a separate program. That FFmpeg build is licensed under the GNU
General Public License, version 2 or later. The exact source code, the
configure line and the license text for the build the recorder embeds are
published with the [\`ffmpeg-13-1\` release of
vodlabs-io/custom-ffmpeg](https://github.com/vodlabs-io/custom-ffmpeg/releases/tag/ffmpeg-13-1),
which also documents how it is built. \`internal/ffmpeg/download.sh\` names the
release in use.

## Bun and JavaScriptCore

The desktop app bundles the [Bun](https://bun.com) runtime (MIT), which
statically links JavaScriptCore and WebKit, licensed under the GNU Lesser
General Public License, version 2 (LGPL-2), along with other libraries under
permissive licenses. <https://bun.com/docs/project/license> lists them with
their licenses and explains how to relink Bun against a modified
JavaScriptCore, whose source is at <https://github.com/oven-sh/WebKit>. The
app ships Bun 1.4.2 unmodified.

## Electrobun

The desktop app is built with [Electrobun](https://github.com/blackboardsh/electrobun)
(MIT), and ships its launcher, native wrapper, bspatch (from bsdiff, BSD-2-Clause)
and zstd (BSD-3-Clause) binaries. The Windows and Linux native wrappers are
rebuilt from Electrobun's source with the changes in \`patches/\`, and include
source from the Chromium Embedded Framework (BSD-3-Clause).

## shadcn/ui

Components in \`ui/mainview/components/ui/\` started from
[shadcn/ui](https://github.com/shadcn-ui/ui) (MIT, Copyright (c) 2023 shadcn).

## Streamlink test fixture

A test in \`internal/writer/hlsingest/\` uses a fixture from
[Streamlink](https://github.com/streamlink/streamlink) (BSD-2-Clause,
Copyright (c) 2011-2016, Christopher Rosell; Copyright (c) 2016-2026,
Streamlink Team). It is not part of any build.

## Go modules

Linked into \`vod\` and \`vlrecd\`.

`;

const npmIntro = `
## npm packages

Bundled into the desktop app.

`;

writeFileSync(out, intro + section(goModules()) + npmIntro + section(npmPackages()));
console.log(`wrote ${out}`);
