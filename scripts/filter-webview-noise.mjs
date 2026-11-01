// Drops third-party page noise that Electrobun forwards from webviews in dev
// builds. The hidden login-refresh webview loads those sites every few minutes. 
// Everything else passes through.
//
// Usage: electrobun dev | bun run scripts/filter-webview-noise.mjs

import { createInterface } from "node:readline";

const BANNER_START = /^\[webview:\d+\] \[console\.log\] %c {20,}/;
const BANNER_END = /Check out jobs\(\) and apps\(\)/;
const NOISE = /^\[webview:\d+\] \[console\.debug\] New Relic Warning/;
// Bounds the skip if the end marker never arrives.
const BANNER_MAX_LINES = 40;

let bannerLines = 0;

for await (const line of createInterface({ input: process.stdin, crlfDelay: Infinity })) {
	if (bannerLines > 0) {
		bannerLines = BANNER_END.test(line) || ++bannerLines > BANNER_MAX_LINES ? 0 : bannerLines;
		continue;
	}
	if (BANNER_START.test(line)) {
		bannerLines = 1;
		continue;
	}
	if (NOISE.test(line)) continue;
	console.log(line);
}
