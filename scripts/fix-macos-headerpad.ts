// Make room for the code signature in Electrobun's Intel macOS core binaries.
//
// Why this exists:
//   Electrobun 1.18.1's darwin-x64 binaries are unsigned and have no spare
//   bytes after their load commands. Signing adds a 16-byte LC_CODE_SIGNATURE
//   command there, so codesign writes it over the start of the code: the
//   signature verifies and notarizes, and the app segfaults on launch
//   (blackboardsh/electrobun#485, fixed only in 2.x, which has no Intel build).
//   The arm64 binaries are already linker-signed, so signing replaces their
//   signature in place.
//
// How this fixes it:
//   Electrobun runs this as its `preBuild` hook, before it copies the core
//   binaries into the app bundle. For each unsigned Mach-O without that room,
//   `vtool -remove-source-version` drops LC_SOURCE_VERSION, an informational
//   16-byte command, and leaves the code byte-for-byte the same. A binary it
//   can't fix fails the build instead of shipping an app that can't start.

import { closeSync, openSync, readdirSync, readSync, statSync } from "node:fs";
import { join } from "node:path";

// preBuild fires for every target; only the Intel macOS binaries need this.
if (process.env.ELECTROBUN_OS !== "macos" || process.env.ELECTROBUN_ARCH !== "x64") {
	process.exit(0);
}

const MH_MAGIC_64 = 0xfeedfacf;
const MACH_HEADER_64_SIZE = 32;
const LC_SEGMENT_64 = 0x19;
const LC_CODE_SIGNATURE = 0x1d;
const LC_SOURCE_VERSION = 0x2a;
const SIGNATURE_COMMAND_SIZE = 16;

type LoadCommands = { signed: boolean; hasSourceVersion: boolean; room: number };

function readHead(path: string, length: number): Buffer {
	const fd = openSync(path, "r");
	try {
		const buf = Buffer.alloc(length);
		return buf.subarray(0, readSync(fd, buf, 0, length, 0));
	} finally {
		closeSync(fd);
	}
}

// inspect reads a thin 64-bit Mach-O's load commands, or returns null for any
// other file. room is the gap between the last load command and the first
// section's contents.
function inspect(path: string): LoadCommands | null {
	let head = readHead(path, MACH_HEADER_64_SIZE);
	if (head.length < MACH_HEADER_64_SIZE || head.readUInt32LE(0) !== MH_MAGIC_64) {
		return null;
	}
	const ncmds = head.readUInt32LE(16);
	const sizeofcmds = head.readUInt32LE(20);
	head = readHead(path, MACH_HEADER_64_SIZE + sizeofcmds);

	let signed = false;
	let hasSourceVersion = false;
	let firstSection = statSync(path).size;
	for (let i = 0, off = MACH_HEADER_64_SIZE; i < ncmds; i++) {
		const cmd = head.readUInt32LE(off);
		if (cmd === LC_CODE_SIGNATURE) signed = true;
		if (cmd === LC_SOURCE_VERSION) hasSourceVersion = true;
		if (cmd === LC_SEGMENT_64) {
			// segment_command_64 is 72 bytes, then nsects section_64 records of
			// 80 bytes, each with its file offset at +48 (0 for zero-fill).
			const nsects = head.readUInt32LE(off + 64);
			for (let s = 0; s < nsects; s++) {
				const offset = head.readUInt32LE(off + 72 + s * 80 + 48);
				if (offset > 0) firstSection = Math.min(firstSection, offset);
			}
		}
		off += head.readUInt32LE(off + 4);
	}
	return {
		signed,
		hasSourceVersion,
		room: firstSection - MACH_HEADER_64_SIZE - sizeofcmds,
	};
}

const dir = join(process.cwd(), "node_modules", "electrobun", "dist-macos-x64");

for (const name of readdirSync(dir)) {
	const path = join(dir, name);
	if (!statSync(path).isFile()) continue;

	let macho = inspect(path);
	if (!macho || macho.signed || macho.room >= SIGNATURE_COMMAND_SIZE) continue;

	if (macho.hasSourceVersion) {
		const result = Bun.spawnSync(
			["vtool", "-remove-source-version", "-output", path, path],
			{ stdio: ["ignore", "inherit", "inherit"] },
		);
		if (result.exitCode !== 0) {
			throw new Error(`fix-macos-headerpad: vtool failed on ${path}`);
		}
		macho = inspect(path)!;
	}
	if (macho.room < SIGNATURE_COMMAND_SIZE) {
		throw new Error(
			`fix-macos-headerpad: ${path} has ${macho.room} bytes free after its load ` +
				`commands and signing needs ${SIGNATURE_COMMAND_SIZE}, so codesign would ` +
				`overwrite its code (blackboardsh/electrobun#485).`,
		);
	}
	console.log(`fix-macos-headerpad: made room for the code signature in ${name}`);
}
