import type { VODPreviews } from "@/lib/daemon"

// Hover scrubbing on an archive card: the cursor's position across the card
// is a position across the whole VOD, earlier on the left. Pure functions
// here; scrub-overlay.tsx renders them.

// The three-up thumbnail's geometry, as the daemon composes it (480x270,
// panels edge to edge, each frame cropped to fill): panel p covers the p-th
// third of the VOD.
export const TRIPTYCH_W = 480
export const TRIPTYCH_H = 270
export const TRIPTYCH_PANEL_W = TRIPTYCH_W / 3

export type ScrubFile = { name: string; durationMs: number }

// locate maps a fraction of the card's width (0 = left edge) onto the VOD's
// files, laid end to end by duration: which file, and how far into it.
export function locate(files: ScrubFile[], fraction: number): { index: number; ms: number } | null {
  const total = files.reduce((sum, f) => sum + Math.max(0, f.durationMs), 0)
  if (files.length === 0 || total <= 0) return null
  let at = clamp(fraction, 0, 1) * total
  for (let i = 0; i < files.length; i++) {
    const d = Math.max(0, files[i].durationMs)
    if (at < d || i === files.length - 1) return { index: i, ms: Math.min(at, d) }
    at -= d
  }
  return null
}

// frameAt is the hover-strip tile showing ms into a file: the strip's tiles
// stand for equal shares of the file, each showing its share's middle.
export function frameAt(p: Pick<VODPreviews, "duration_ms" | "scrub">, ms: number): number {
  const n = p.scrub.count
  if (n <= 0 || p.duration_ms <= 0) return 0
  return clamp(Math.floor((ms / p.duration_ms) * n), 0, n - 1)
}

// panelAt is the triptych panel under a fraction of the card's width.
export function panelAt(fraction: number): number {
  return clamp(Math.floor(clamp(fraction, 0, 1) * 3), 0, 2)
}

// panelBox is panel p's left edge and width as percentages of the card.
export function panelBox(p: number): { left: number; width: number } {
  const width = (TRIPTYCH_PANEL_W / TRIPTYCH_W) * 100
  return { left: p * width, width }
}

// coverBox sizes a tile-shaped frame to fill a panel, as the triptych crops
// it: width and height as percentages of the panel, overflow centred.
export function coverBox(tileW: number, tileH: number): { width: number; height: number } {
  const tile = tileW / tileH
  const panel = TRIPTYCH_PANEL_W / TRIPTYCH_H
  if (!(tile > 0)) return { width: 100, height: 100 }
  return tile < panel ? { width: 100, height: (panel / tile) * 100 } : { width: (tile / panel) * 100, height: 100 }
}

// spriteStyle crops tile i out of the strip with percentage background
// sizing, so it scales with whatever box it is drawn in.
export function spriteStyle(
  p: Pick<VODPreviews, "scrub">,
  i: number,
  url: string,
): { backgroundImage: string; backgroundSize: string; backgroundPosition: string } {
  const cols = Math.max(1, p.scrub.cols)
  const rows = Math.max(1, Math.ceil(p.scrub.count / cols))
  const col = i % cols
  const row = Math.floor(i / cols)
  const x = cols > 1 ? (col / (cols - 1)) * 100 : 0
  const y = rows > 1 ? (row / (rows - 1)) * 100 : 0
  return {
    backgroundImage: `url("${url}")`,
    backgroundSize: `${cols * 100}% ${rows * 100}%`,
    backgroundPosition: `${x}% ${y}%`,
  }
}

function clamp(n: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, n))
}
