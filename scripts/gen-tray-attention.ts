// Draws the tray icons' alert badge, a circle holding an exclamation mark in the
// corner. On the monochrome icons (macOS, Linux) the badge is the icon's own
// colour with the mark cut out, so it still works when macOS tints the template.
// On Windows' colour icon it is yellow with a dark mark. Also writes the white
// tray silhouette Linux uses. macOS-only, like gen-icons.sh, which runs it: sips
// does the resizing.

import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { deflateSync, inflateSync } from "node:zlib";

const DESK = join(import.meta.dir, "..", "desktop");
const YELLOW = [0xea, 0xb3, 0x08]; // the UI's attention yellow, Tailwind yellow-500
const DARK = [0x1c, 0x19, 0x17];
const WHITE = [0xff, 0xff, 0xff];
const ICO_SIZES = [48, 32, 16];

type Image = { size: number; rgba: Uint8Array };

function readPng(path: string): Image {
  const file = readFileSync(path);
  let pos = 8;
  let size = 0;
  const idat: Buffer[] = [];
  while (pos < file.length) {
    const length = file.readUInt32BE(pos);
    const type = file.toString("latin1", pos + 4, pos + 8);
    const data = file.subarray(pos + 8, pos + 8 + length);
    if (type === "IHDR") {
      size = data.readUInt32BE(0);
      if (data[8] !== 8 || data[9] !== 6 || data[12] !== 0) {
        throw new Error(`${path}: expected 8-bit RGBA, non-interlaced`);
      }
    }
    if (type === "IDAT") idat.push(data);
    pos += length + 12;
  }
  const raw = inflateSync(Buffer.concat(idat));
  const rgba = new Uint8Array(size * size * 4);
  const stride = size * 4;
  for (let y = 0; y < size; y++) {
    const filter = raw[y * (stride + 1)]!;
    for (let x = 0; x < stride; x++) {
      const value = raw[y * (stride + 1) + 1 + x]!;
      const a = x >= 4 ? rgba[y * stride + x - 4]! : 0;
      const b = y > 0 ? rgba[(y - 1) * stride + x]! : 0;
      const c = x >= 4 && y > 0 ? rgba[(y - 1) * stride + x - 4]! : 0;
      const p = a + b - c;
      const [pa, pb, pc] = [Math.abs(p - a), Math.abs(p - b), Math.abs(p - c)];
      const paeth = pa <= pb && pa <= pc ? a : pb <= pc ? b : c;
      const predicted = [0, a, b, (a + b) >> 1, paeth][filter]!;
      rgba[y * stride + x] = (value + predicted) & 0xff;
    }
  }
  return { size, rgba };
}

const crcTable = Array.from({ length: 256 }, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});

function crc32(data: Buffer): number {
  let c = 0xffffffff;
  for (const byte of data) c = crcTable[(c ^ byte) & 0xff]! ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function pngChunk(type: string, data: Buffer): Buffer {
  const body = Buffer.concat([Buffer.from(type, "latin1"), data]);
  const out = Buffer.alloc(body.length + 8);
  out.writeUInt32BE(data.length, 0);
  body.copy(out, 4);
  out.writeUInt32BE(crc32(body), body.length + 4);
  return out;
}

function writePng({ size, rgba }: Image, path: string): void {
  const header = Buffer.alloc(13);
  header.writeUInt32BE(size, 0);
  header.writeUInt32BE(size, 4);
  header.set([8, 6, 0, 0, 0], 8);
  const raw = Buffer.alloc((size * 4 + 1) * size);
  for (let y = 0; y < size; y++) {
    raw.set(rgba.subarray(y * size * 4, (y + 1) * size * 4), y * (size * 4 + 1) + 1);
  }
  writeFileSync(
    path,
    Buffer.concat([
      Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
      pngChunk("IHDR", header),
      pngChunk("IDAT", deflateSync(raw)),
      pngChunk("IEND", Buffer.alloc(0)),
    ]),
  );
}

const SUBPIXELS = 4;

function recolor({ rgba }: Image, color: number[]): void {
  for (let i = 0; i < rgba.length; i += 4) rgba.set(color, i);
}

// addBadge draws the badge over the icon's lower right, clearing a ring around it
// so it stays distinct from what it covers. mark is the exclamation mark's colour,
// or null to cut it out.
function addBadge({ size, rgba }: Image, disc: number[], mark: number[] | null): void {
  const r = size * 0.29;
  const ring = size * 0.05;
  const centre = size - r - size * 0.01;
  const stemHalf = r * 0.15;
  const inMark = (dx: number, dy: number): boolean =>
    (Math.abs(dx) <= stemHalf && dy >= -r * 0.62 && dy <= r * 0.18) ||
    Math.hypot(dx, dy - r * 0.52) <= r * 0.17;

  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      let [inDisc, inRing, onMark] = [0, 0, 0];
      for (let sy = 0; sy < SUBPIXELS; sy++) {
        for (let sx = 0; sx < SUBPIXELS; sx++) {
          const dx = x + (sx + 0.5) / SUBPIXELS - 0.5 - centre;
          const dy = y + (sy + 0.5) / SUBPIXELS - 0.5 - centre;
          const d = Math.hypot(dx, dy);
          if (d <= r + ring) inRing++;
          if (d <= r) {
            inDisc++;
            if (inMark(dx, dy)) onMark++;
          }
        }
      }
      const samples = SUBPIXELS * SUBPIXELS;
      const i = (y * size + x) * 4;
      const under = (rgba[i + 3]! / 255) * (1 - inRing / samples);
      const discPart = (inDisc - onMark) / samples;
      const markPart = mark ? onMark / samples : 0;
      const alpha = Math.min(1, under + discPart + markPart);
      for (let c = 0; c < 3; c++) {
        const painted = rgba[i + c]! * under + disc[c]! * discPart + (mark?.[c] ?? 0) * markPart;
        rgba[i + c] = alpha > 0 ? Math.round(painted / alpha) : 0;
      }
      rgba[i + 3] = Math.round(alpha * 255);
    }
  }
}

// One ICO frame in the classic form, a bottom-up BGRA bitmap with an empty AND
// mask, which is what Windows' LoadImage reads at these sizes.
function icoFrame({ size, rgba }: Image): Buffer {
  const maskStride = Math.ceil(size / 32) * 4;
  const frame = Buffer.alloc(40 + size * size * 4 + maskStride * size);
  frame.writeUInt32LE(40, 0);
  frame.writeInt32LE(size, 4);
  frame.writeInt32LE(size * 2, 8);
  frame.writeUInt16LE(1, 12);
  frame.writeUInt16LE(32, 14);
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      const from = (y * size + x) * 4;
      const to = 40 + ((size - 1 - y) * size + x) * 4;
      frame.set([rgba[from + 2]!, rgba[from + 1]!, rgba[from]!, rgba[from + 3]!], to);
    }
  }
  return frame;
}

function writeIco(images: Image[], path: string): void {
  const frames = images.map(icoFrame);
  const header = Buffer.alloc(6 + 16 * frames.length);
  header.writeUInt16LE(1, 2);
  header.writeUInt16LE(frames.length, 4);
  let offset = header.length;
  frames.forEach((frame, i) => {
    const entry = 6 + 16 * i;
    header[entry] = images[i]!.size;
    header[entry + 1] = images[i]!.size;
    header.writeUInt16LE(1, entry + 4);
    header.writeUInt16LE(32, entry + 6);
    header.writeUInt32LE(frame.length, entry + 8);
    header.writeUInt32LE(offset, entry + 12);
    offset += frame.length;
  });
  writeFileSync(path, Buffer.concat([header, ...frames]));
}

const silhouette = () => readPng(join(DESK, "tray.png"));

const mac = silhouette();
addBadge(mac, DARK.map(() => 0), null);
writePng(mac, join(DESK, "tray-attention.png"));

const linux = silhouette();
recolor(linux, WHITE);
writePng(linux, join(DESK, "tray-linux.png"));
addBadge(linux, WHITE, null);
writePng(linux, join(DESK, "tray-linux-attention.png"));

const work = mkdtempSync(join(tmpdir(), "tray-attention-"));
try {
  const images = ICO_SIZES.map((size) => {
    const png = join(work, `${size}.png`);
    execFileSync("sips", ["-s", "format", "png", "-z", `${size}`, `${size}`, join(DESK, "logo-src.png"), "--out", png], {
      stdio: "ignore",
    });
    const image = readPng(png);
    addBadge(image, YELLOW, DARK);
    return image;
  });
  writeIco(images, join(DESK, "icon-attention.ico"));
} finally {
  rmSync(work, { recursive: true, force: true });
}
