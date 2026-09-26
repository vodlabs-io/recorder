// Renders a Shot list: starts a Demo daemon (frozen at the list's `now`) and
// the harness dev server, shoots every Scene in Playwright's WebKit, draws
// Window chrome around each Shot, and lays out any compositions.
//
//   bun tools/showcase/harness/render.ts tools/showcase/shots/marketing.yaml
//     [--out DIR] [--only NAME,…] [--daemon-bin PATH]
//
// Outputs, per Scene and theme, in --out (default tools/showcase/out/<list>):
//   <scene>-<theme>.png          the window with chrome and shadow, transparent
//   <scene>-<theme>.content.png  the window's content alone
// and per composition: <name>-<theme>.png, plus <name>-<theme>-backdrop.png
// when the composition asks for a backdrop. With cwebp on PATH, every framed
// PNG also gets a .webp beside it, for the web.

import { spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, extname, join, resolve } from "node:path";
import { parseArgs } from "node:util";
import { pathToFileURL } from "node:url";
import { createServer } from "vite";
import { webkit, type Page } from "playwright";
import { chromeHTML, compositionHTML, type Theme } from "./chrome";

const here = dirname(new URL(import.meta.url).pathname);
const repoRoot = resolve(here, "../../..");

const { values: args, positionals } = parseArgs({
  allowPositionals: true,
  options: {
    out: { type: "string" },
    only: { type: "string" },
    "daemon-bin": { type: "string" },
  },
});

// --- Shot list ---------------------------------------------------------------

type Step =
  | { wait: string | number }
  | { click: string }
  | { hover: string }
  | { press: string }
  | { scroll: { selector: string; y: number } }
  | { eval: string }
  | { script: string };

type Scene = {
  name: string;
  route: string;
  size?: [number, number];
  themes?: Theme[];
  prefs?: Record<string, unknown>;
  steps?: Step[];
};

type Composition = {
  name: string;
  themes?: Theme[];
  // Back to front; `at` is the window's top-left in points.
  windows: { scene: string; at: [number, number] }[];
  backdrop?: boolean;
};

type ShotList = {
  dataset?: string;
  now: string;
  timezone?: string;
  locale?: string;
  app_version?: string;
  defaults?: { size?: [number, number]; scale?: number; themes?: Theme[]; prefs?: Record<string, unknown> };
  scenes: Scene[];
  compositions?: Composition[];
};

const listPath = positionals[0];
if (!listPath) {
  console.error("usage: render.ts <shot-list.yaml> [--out DIR] [--only NAME,…]");
  process.exit(2);
}
const list = Bun.YAML.parse(readFileSync(listPath, "utf8")) as ShotList;
const listName = basename(listPath, extname(listPath));
const outDir = resolve(args.out ?? join(repoRoot, "tools/showcase/out", listName));
const only = args.only ? new Set(args.only.split(",")) : null;
const scale = list.defaults?.scale ?? 2;
const defaultSize = list.defaults?.size ?? [1280, 800];
const defaultThemes = list.defaults?.themes ?? ["light", "dark"];
const datasetDir = resolve(repoRoot, "tools/showcase/datasets", list.dataset ?? "default");

// Title bar height of the drawn macOS chrome; a Scene's size is the whole
// window, so its content is this much shorter.
const TITLEBAR = 32;

// Ports clear of an interactive Demo daemon (14222…) and the dev servers.
const PORTS = { nats: 14322, ws: 14323, http: 14324, vite: 5176 };

// --- Services ----------------------------------------------------------------

function startDaemon(): Promise<ChildProcess> {
  const flags = [
    "--dataset", datasetDir,
    "--port", String(PORTS.nats),
    "--ws-port", String(PORTS.ws),
    "--http-port", String(PORTS.http),
    "--now", new Date(list.now).toISOString(),
    "--freeze",
  ];
  // Built rather than `go run`, which would leave the daemon a grandchild that
  // outlives the kill below.
  let bin = args["daemon-bin"];
  if (!bin) {
    bin = join(mkdtempSync(join(tmpdir(), "showcase-")), "demodaemon");
    const build = Bun.spawnSync(["go", "build", "-o", bin, "./tools/showcase/demodaemon"], { cwd: repoRoot, stderr: "inherit" });
    if (build.exitCode !== 0) throw new Error("go build of the Demo daemon failed");
  }
  const proc = spawn(bin, flags, { cwd: repoRoot, stdio: ["ignore", "inherit", "pipe"] });
  return new Promise((ok, fail) => {
    let log = "";
    proc.stderr!.on("data", (b: Buffer) => {
      log += b.toString();
      if (log.includes("Demo daemon serving")) ok(proc);
    });
    proc.on("exit", (code) => fail(new Error(`demo daemon exited (${code}):\n${log}`)));
  });
}

// --- Rendering ---------------------------------------------------------------

async function runStep(page: Page, step: Step): Promise<void> {
  if ("wait" in step) {
    if (typeof step.wait === "number") await page.waitForTimeout(step.wait);
    else await page.locator(step.wait).first().waitFor({ state: "visible" });
  } else if ("click" in step) await page.locator(step.click).first().click();
  else if ("hover" in step) await page.locator(step.hover).first().hover();
  else if ("press" in step) await page.keyboard.press(step.press);
  else if ("scroll" in step)
    await page.locator(step.scroll.selector).first().evaluate((el, y) => el.scrollBy(0, y), step.scroll.y);
  else if ("eval" in step) await page.evaluate(step.eval);
  else if ("script" in step) {
    const mod = await import(pathToFileURL(resolve(dirname(listPath), step.script)).href);
    await mod.default(page);
  }
}

// settle waits for the UI to stop changing: every image decoded, then a beat
// for transitions.
async function settle(page: Page): Promise<void> {
  await page.waitForFunction(() => [...document.images].every((i) => i.complete && i.naturalWidth > 0), null, {
    timeout: 15_000,
  }).catch(() => console.warn("  some images never loaded"));
  await page.waitForTimeout(600);
}

async function main(): Promise<void> {
  mkdirSync(outDir, { recursive: true });
  const daemon = await startDaemon();
  const vite = await createServer({
    configFile: join(here, "vite.config.ts"),
    server: { port: PORTS.vite, strictPort: true },
    logLevel: "warn",
  });
  await vite.listen();
  const browser = await webkit.launch();
  const now = new Date(list.now);
  const contentShots = new Map<string, string>(); // "<scene>-<theme>" -> png path

  try {
    const wanted = (name: string) => !only || only.has(name);
    const composed = new Set((list.compositions ?? []).filter((c) => wanted(c.name)).flatMap((c) => c.windows.map((w) => w.scene)));

    for (const scene of list.scenes) {
      if (!wanted(scene.name) && !composed.has(scene.name)) continue;
      const [w, h] = scene.size ?? defaultSize;
      for (const theme of scene.themes ?? defaultThemes) {
        const ctx = await browser.newContext({
          viewport: { width: w, height: h - TITLEBAR },
          deviceScaleFactor: scale,
          colorScheme: theme,
          timezoneId: list.timezone ?? "America/Los_Angeles",
          locale: list.locale ?? "en-US",
        });
        const page = await ctx.newPage();
        page.on("pageerror", (e) => console.warn(`  ${scene.name}: ${e.message}`));
        const prefs = { welcome_seen: true, ...list.defaults?.prefs, ...scene.prefs, theme };
        await page.addInitScript((p) => localStorage.setItem("vodlabs-recorder-preferences", p), JSON.stringify(prefs));
        await page.clock.setFixedTime(now);
        const q = new URLSearchParams({ nats: `ws://127.0.0.1:${PORTS.ws}`, app_version: list.app_version ?? "1.0.0" });
        await page.goto(`http://127.0.0.1:${PORTS.vite}/?${q}#${scene.route}`);
        await page.waitForTimeout(1500); // connect and receive the first snapshot
        for (const step of scene.steps ?? []) await runStep(page, step);
        await settle(page);

        const base = `${scene.name}-${theme}`;
        const content = join(outDir, `${base}.content.png`);
        await page.screenshot({ path: content });
        contentShots.set(base, content);
        await ctx.close();

        if (wanted(scene.name)) {
          await renderHTML(chromeHTML({ content, width: w, height: h, theme }), join(outDir, `${base}.png`));
          console.log(`shot ${base}`);
        }
      }
    }

    for (const comp of list.compositions ?? []) {
      if (!wanted(comp.name)) continue;
      for (const theme of comp.themes ?? defaultThemes) {
        const windows = comp.windows.map((win) => {
          const scene = list.scenes.find((s) => s.name === win.scene);
          if (!scene) throw new Error(`composition ${comp.name}: no scene ${win.scene}`);
          const content = contentShots.get(`${win.scene}-${theme}`);
          if (!content) throw new Error(`composition ${comp.name}: ${win.scene} has no ${theme} shot`);
          const [w, h] = scene.size ?? defaultSize;
          return { content, width: w, height: h, at: win.at };
        });
        const base = `${comp.name}-${theme}`;
        await renderHTML(compositionHTML({ windows, theme, backdrop: false }), join(outDir, `${base}.png`));
        if (comp.backdrop) {
          await renderHTML(compositionHTML({ windows, theme, backdrop: true }), join(outDir, `${base}-backdrop.png`));
        }
        console.log(`composition ${base}`);
      }
    }
  } finally {
    await browser.close();
    await vite.close();
    daemon.kill("SIGTERM");
  }

  // renderHTML shoots a static page sized by its #stage element.
  async function renderHTML(html: string, path: string): Promise<void> {
    const page = await browser.newPage({ deviceScaleFactor: scale });
    const tmp = path.replace(/\.png$/, ".stage.html");
    writeFileSync(tmp, html);
    await page.goto(pathToFileURL(tmp).href);
    await page.waitForFunction(() => [...document.images].every((i) => i.complete));
    const stage = page.locator("#stage");
    await stage.screenshot({ path, omitBackground: true });
    await page.close();
    await Bun.file(tmp).delete();
    const cwebp = Bun.which("cwebp");
    if (cwebp) {
      const res = Bun.spawnSync([cwebp, "-quiet", "-q", "88", "-alpha_q", "100", path, "-o", path.replace(/\.png$/, ".webp")]);
      if (res.exitCode !== 0) console.warn(`  cwebp failed for ${path}`);
    }
  }
}

await main();
console.log(`wrote ${outDir}`);
if (!existsSync(outDir)) process.exit(1);
