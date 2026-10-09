// Set the Windows window icon at runtime (title bar, Alt+Tab, taskbar button).
//
// Why this exists:
//   A window's icon is a *runtime* property of the window, not the icon baked
//   into the .exe: Windows reads it from the window's WM_SETICON icons, falls
//   back to its window class's icon, and only then to the icon of the process
//   that owns it. Electrobun sets neither. `setWindowIcon` in its native
//   wrapper is implemented on Linux only — on Windows the whole body is
//   "// TODO: Implement using SetWindowIcon/LoadImage APIs" — and it registers
//   its top-level window class as a zero-initialised WNDCLASSA, so hIcon and
//   hIconSm are null too. What's left is the fallback: a generic placeholder in
//   the title bar, and on the taskbar button the icon of the process that owns
//   the window, which is bun.exe.
//
// How this fixes it:
//   Call user32 ourselves over bun:ffi, which needs nothing from Electrobun's
//   native layer beyond the window handle it already hands us — its own
//   nativeWrapper.cpp says it plainly ("On Windows, NSWindow* is actually
//   HWND"), so a BrowserWindow's `ptr` is the HWND and WM_SETICON takes it
//   directly. We set both sizes because Windows asks for them separately:
//   ICON_SMALL is the title bar and Alt+Tab, ICON_BIG the taskbar button.
//
//   We also set them as the *class* icons, which covers every other window
//   Electrobun opens on the same class (the platform sign-in window, an update
//   prompt) without each one having to ask.
//
//   The icon path must be a real filesystem path (LoadImageW reads it from
//   disk, it is not a views:// URL). Electrobun copies win.icon to
//   Resources/app.ico in the bundle; we locate it relative to bun.exe
//   (Resources/ is a sibling of the bin/ dir the interpreter runs from) so it's
//   independent of the process working directory.
//
//   Best-effort throughout: the icon is cosmetic and must never crash the app,
//   so we guard everything and warn rather than throw.
//
// Remove this once Electrobun implements setWindowIcon on Windows.

import { dirname, join } from "node:path";
import { existsSync } from "node:fs";
import { dlopen, FFIType, ptr, type Pointer } from "bun:ffi";
import type { BrowserWindow } from "electrobun/main";

const WM_SETICON = 0x0080;
const ICON_SMALL = 0;
const ICON_BIG = 1;
const IMAGE_ICON = 1;
const LR_LOADFROMFILE = 0x0010;
// GetSystemMetrics indices for the two sizes Windows draws icons at.
const SM_CXICON = 11;
const SM_CYICON = 12;
const SM_CXSMICON = 49;
const SM_CYSMICON = 50;
// SetClassLongPtr indices for a class's large and small icon.
const GCLP_HICON = -14;
const GCLP_HICONSM = -34;

// WPARAM, LPARAM and HANDLE are all 64-bit here: a handle rides as FFIType.ptr
// (which is what the calls that produce one hand back), a plain integer as
// u64_fast (the same register, taking a JS number).
const USER32 = {
	LoadImageW: {
		args: [
			FFIType.ptr, // HINSTANCE (null: load from file)
			FFIType.ptr, // LPCWSTR name
			FFIType.u32, // image type
			FFIType.i32, // desired width
			FFIType.i32, // desired height
			FFIType.u32, // load flags
		],
		returns: FFIType.ptr, // HICON, null on failure
	},
	SendMessageW: {
		args: [
			FFIType.ptr, // HWND
			FFIType.u32, // message
			FFIType.u64_fast, // WPARAM: which of the two icons
			FFIType.ptr, // LPARAM: the HICON
		],
		returns: FFIType.ptr,
	},
	SetClassLongPtrW: {
		args: [FFIType.ptr, FFIType.i32, FFIType.ptr],
		returns: FFIType.ptr,
	},
	GetSystemMetrics: { args: [FFIType.i32], returns: FFIType.i32 },
} as const;

const warn = (msg: string) => console.warn(`win-window-icon: ${msg}`);

type Icons = { small: Pointer | null; big: Pointer | null };
type User32 = ReturnType<typeof dlopen<typeof USER32>>["symbols"];

// The HICONs outlive every window that uses them (a closed window is reopened
// from the tray, and the class holds them for the life of the process), so they
// are loaded once and never destroyed — resolved lazily so a machine that
// cannot load them costs nothing and warns once.
let resolved: { user32: User32; icons: Icons } | null | undefined;

function findIcon(): string | null {
	// bun.exe runs from <bundle>/bin; the icon lives at <bundle>/Resources/app.ico
	// (Electrobun's copy of win.icon). Fall back to our explicit views/ copy.
	const resources = join(dirname(process.execPath), "..", "Resources");
	return (
		[join(resources, "app.ico"), join(resources, "app", "views", "icon.ico")].find(
			existsSync,
		) ?? null
	);
}

function load(): { user32: User32; icons: Icons } | null {
	if (resolved !== undefined) return resolved;
	resolved = null;

	const icon = findIcon();
	if (!icon) {
		warn("no bundled .ico found; skipping");
		return resolved;
	}

	try {
		const { symbols } = dlopen("user32.dll", USER32);
		// A NUL-terminated UTF-16 copy, which is what LPCWSTR expects. It only has
		// to outlive the synchronous call it is passed to.
		const path = Buffer.from(`${icon}\0`, "utf16le");
		const loadAt = (cx: number, cy: number) =>
			symbols.LoadImageW(null, ptr(path), IMAGE_ICON, cx, cy, LR_LOADFROMFILE);
		const icons: Icons = {
			small: loadAt(
				symbols.GetSystemMetrics(SM_CXSMICON),
				symbols.GetSystemMetrics(SM_CYSMICON),
			),
			big: loadAt(symbols.GetSystemMetrics(SM_CXICON), symbols.GetSystemMetrics(SM_CYICON)),
		};
		if (!icons.small && !icons.big) {
			warn(`could not load any icon from ${icon}`);
			return resolved;
		}
		console.log(`win-window-icon: loaded window icon from ${icon}`);
		resolved = { user32: symbols, icons };
	} catch (err) {
		warn(`failed to load the window icon: ${err}`);
	}
	return resolved;
}

export function applyWindowIcon(win: BrowserWindow): void {
	if (process.platform !== "win32") return;
	if (!win.ptr) return;

	const loaded = load();
	if (!loaded) return;
	const { user32, icons } = loaded;

	try {
		for (const [icon, which, classIndex] of [
			[icons.small, ICON_SMALL, GCLP_HICONSM],
			[icons.big, ICON_BIG, GCLP_HICON],
		] as const) {
			if (!icon) continue;
			user32.SendMessageW(win.ptr, WM_SETICON, which, icon);
			// Every Electrobun window shares one class, so this is the same icon for
			// the ones nobody calls us for.
			user32.SetClassLongPtrW(win.ptr, classIndex, icon);
		}
	} catch (err) {
		warn(`failed to set the window icon: ${err}`);
	}
}
