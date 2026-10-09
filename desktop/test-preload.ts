// bun test preload: unit tests never touch native windows, and Electrobun's SDK
// calls into its native libraries as soon as it is imported, which only exist
// inside a built app. Modules under test get inert stand-ins instead.

import { mock } from "bun:test";

class BrowserView {
	init() {}
}
class BrowserWindow {}

mock.module("electrobun/main", () => ({
	BrowserView,
	BrowserWindow,
	Screen: {},
	Session: {},
	Updater: {},
	Utils: {},
}));
