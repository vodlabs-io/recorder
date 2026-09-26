// Window chrome drawn around Shots. macOS only for now, measured from the real
// app's window on macOS 26: a 32pt title bar with a left-aligned title, 14pt
// window controls 9pt apart, and 16pt corner radius.

export type Theme = "light" | "dark";

type Window = { content: string; width: number; height: number; at?: [number, number] };

const palette = {
  dark: {
    bar: "#1b1b1b",
    separator: "#232323",
    title: "#979797",
    edge: "rgba(0,0,0,0.85)",
    highlight: "rgba(255,255,255,0.2)",
    controls: ["#ed6765", "#f3c944", "#65c366"],
    shadow: "0 30px 70px rgba(0,0,0,0.55), 0 10px 24px rgba(0,0,0,0.35)",
    backdrop: "radial-gradient(120% 90% at 20% 10%, #2a2f45 0%, #121420 55%, #08090e 100%)",
  },
  light: {
    bar: "#ebebeb",
    separator: "#d6d6d6",
    title: "#6e6e6e",
    edge: "rgba(0,0,0,0.22)",
    highlight: "rgba(255,255,255,0.7)",
    controls: ["#ff5f57", "#febc2e", "#28c840"],
    shadow: "0 30px 70px rgba(0,0,0,0.28), 0 10px 24px rgba(0,0,0,0.16)",
    backdrop: "radial-gradient(120% 90% at 20% 10%, #eef1ff 0%, #dfe4f5 55%, #c9d0e8 100%)",
  },
};

// Room around the windows for their shadow to fall into.
const MARGIN = 80;

function windowHTML(w: Window, theme: Theme): string {
  const p = palette[theme];
  const pos = w.at ? `position:absolute;left:${w.at[0] + MARGIN}px;top:${w.at[1] + MARGIN}px;` : "";
  const controls = p.controls
    .map((c, i) => `<span style="left:${9 + i * 23}px;background:${c}"></span>`)
    .join("");
  return `
<div class="window" style="${pos}width:${w.width}px;height:${w.height}px;box-shadow:0 0 0 0.5px ${p.edge},${p.shadow}">
  <div class="bar" style="background:${p.bar};border-bottom:1px solid ${p.separator}">
    <div class="controls">${controls}</div>
    <div class="title" style="color:${p.title}">VodLabs Recorder</div>
  </div>
  <img src="${w.content}" width="${w.width}" height="${w.height - 32}">
  <div class="rim" style="box-shadow:inset 0 0.5px 0 ${p.highlight}"></div>
</div>`;
}

const css = `
html, body { margin: 0; background: transparent; }
#stage { position: relative; display: inline-block; }
.window { position: relative; border-radius: 16px; overflow: hidden; }
.bar { position: relative; height: 31px; }
.controls span { position: absolute; top: 9px; width: 14px; height: 14px; border-radius: 50%;
  box-shadow: inset 0 0 0 0.5px rgba(0,0,0,0.12); }
.title { position: absolute; left: 82.5px; top: 0; font: 700 13px/32px -apple-system, system-ui, sans-serif; }
.window img { display: block; }
.rim { position: absolute; inset: 0; border-radius: 16px; pointer-events: none; }
`;

function page(body: string, style: string): string {
  return `<!doctype html><html><head><meta charset="utf-8"><style>${css}</style></head>
<body><div id="stage" style="${style}">${body}</div></body></html>`;
}

// chromeHTML is one Shot's window, with margin for its shadow.
export function chromeHTML(w: Window & { theme: Theme }): string {
  return page(windowHTML(w, w.theme), `padding:${MARGIN}px`);
}

// compositionHTML lays several windows out on one stage, back to front,
// optionally over a backdrop.
export function compositionHTML(o: { windows: Window[]; theme: Theme; backdrop: boolean }): string {
  const width = Math.max(...o.windows.map((w) => (w.at?.[0] ?? 0) + w.width)) + 2 * MARGIN;
  const height = Math.max(...o.windows.map((w) => (w.at?.[1] ?? 0) + w.height)) + 2 * MARGIN;
  const bg = o.backdrop ? `background:${palette[o.theme].backdrop};` : "";
  return page(
    o.windows.map((w) => windowHTML(w, o.theme)).join(""),
    `width:${width}px;height:${height}px;${bg}`,
  );
}
