// Dev preflight: quit any installed VodLabs Recorder and wait for the daemon
// port to free up, so the dev app can't attach to the installed daemon.
//
// Dev builds share the installed app's bundle id, so apps are told apart by
// bundle name: "VodLabs Recorder-dev.app" is left running. The quit goes
// through the app's own quit path, which stops its daemon (and asks first if
// it has work in flight).

import { execFileSync } from "node:child_process";
import { connect } from "node:net";

const BUNDLE_ID = "com.vodlabs.recorder";
const NATS_HOST = "127.0.0.1";
const NATS_PORT = 4222;
const WAIT_MS = 120_000;

// quitInstalledApps asks each running non-dev app to quit and returns the
// bundle paths it asked.
function quitInstalledApps() {
	if (process.platform !== "darwin") return [];
	const script = `
		ObjC.import("AppKit");
		const apps = $.NSRunningApplication.runningApplicationsWithBundleIdentifier("${BUNDLE_ID}");
		const quit = [];
		for (let i = 0; i < apps.count; i++) {
			const app = apps.objectAtIndex(i);
			const path = app.bundleURL.path.js;
			if (path.endsWith("-dev.app")) continue;
			app.terminate;
			quit.push(path);
		}
		quit.join("\\n");
	`;
	const out = execFileSync("osascript", ["-l", "JavaScript", "-e", script], {
		encoding: "utf8",
	});
	return out.split("\n").filter(Boolean);
}

function portInUse() {
	return new Promise((resolve) => {
		const socket = connect(NATS_PORT, NATS_HOST);
		socket.once("connect", () => {
			socket.destroy();
			resolve(true);
		});
		socket.once("error", () => resolve(false));
	});
}

function portHolder() {
	try {
		const pid = execFileSync(
			"lsof",
			["-nP", `-iTCP:${NATS_PORT}`, "-sTCP:LISTEN", "-t"],
			{ encoding: "utf8" },
		).trim().split("\n")[0];
		const command = execFileSync("ps", ["-o", "command=", "-p", pid], {
			encoding: "utf8",
		}).trim();
		return `PID ${pid}: ${command}`;
	} catch {
		return "unknown process";
	}
}

for (const path of quitInstalledApps()) console.log(`Quitting ${path}...`);

const deadline = Date.now() + WAIT_MS;
let hinted = false;
while (await portInUse()) {
	if (Date.now() > deadline) {
		console.error(
			`Port ${NATS_PORT} is still held by ${portHolder()}. Stop it and try again.`,
		);
		process.exit(1);
	}
	if (!hinted) {
		console.log(
			`Waiting for port ${NATS_PORT} to free up (if the installed app asks about work in progress, answer it there)...`,
		);
		hinted = true;
	}
	await new Promise((r) => setTimeout(r, 1000));
}
