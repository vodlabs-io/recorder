// Ranking for the stream sniffer (stream-sniff.ts): turns what a page's frames
// reported into the candidates the daemon relays, best first. Pure, so it is
// tested against hit lists recorded on real sites.

// A Hit is one report from the in-page sniffer.
export type Hit = {
  // How the URL was seen: fetch, xhr, resource (the page's resource timing),
  // media (a <video>/<source> src set by script), dom (a media element's
  // current source), playing (the source of a media element that is playing),
  // html (an address in the final document), or response (a playlist body the
  // page read: see master/ended).
  kind: string;
  url: string;
  // The document that saw it, which may be an embedded player's frame.
  frame: string;
  // Set on a response hit: whether the playlist lists variants, and whether
  // it has ended (a VOD or a finished broadcast).
  master?: boolean;
  ended?: boolean;
};

export type Candidate = {
  url: string;
  format: "hls" | "flv";
  frame?: string;
};

// AD_HOSTS serve ad creatives, never the content stream. Server-side ad
// insertion (dai.google.com) IS the content stream and is not listed.
const AD_HOSTS = ["imasdk.googleapis.com", "doubleclick.net", "googlesyndication.com"];

export function formatOf(url: string): "hls" | "flv" | null {
  const l = url.toLowerCase();
  if (l.includes(".m3u8") || l.includes("mpegurl")) return "hls";
  if (l.includes(".flv")) return "flv";
  return null;
}

function isAd(host: string): boolean {
  host = host.toLowerCase();
  return AD_HOSTS.some((ad) => host === ad || host.endsWith("." + ad));
}

type Seen = {
  url: string;
  format: "hls" | "flv";
  frame: string;
  order: number;
  kinds: Set<string>;
  requests: number;
  master: boolean;
  ended: boolean;
};

// collect folds hits into one entry per stream URL.
function collect(hits: Hit[]): Map<string, Seen> {
  const seen = new Map<string, Seen>();
  hits.forEach((h, i) => {
    let u: URL;
    try {
      u = new URL(h.url);
    } catch {
      return;
    }
    if (u.protocol !== "http:" && u.protocol !== "https:") return;
    if (isAd(u.hostname)) return;
    const format = formatOf(h.url);
    if (!format) return;
    let s = seen.get(h.url);
    if (!s) {
      s = {
        url: h.url,
        format,
        frame: h.frame,
        order: i,
        kinds: new Set(),
        requests: 0,
        master: false,
        ended: false,
      };
      seen.set(h.url, s);
    }
    s.kinds.add(h.kind);
    if (h.kind === "fetch" || h.kind === "xhr" || h.kind === "resource") s.requests++;
    if (h.master) s.master = true;
    if (h.ended) s.ended = true;
    // The frame whose player fetched it is the Referer it expects; an html
    // sighting in the top document says less than that.
    if (h.kind !== "html" && h.frame) s.frame = h.frame;
  });
  return seen;
}

// score orders candidates: what a <video> is actually playing first, then a
// playlist the page keeps re-reading (a live media playlist), a master
// playlist (which leaves the quality to choose), anything a player set as its
// source or requested, and last an address merely written in the page.
function score(s: Seen): number {
  let n = 0;
  if (s.kinds.has("playing")) n += 100;
  if (s.requests >= 2) n += 40;
  if (s.master) n += 30;
  if (s.kinds.has("media") || s.kinds.has("dom")) n += 20;
  if (s.kinds.has("fetch") || s.kinds.has("xhr") || s.kinds.has("resource")) n += 10;
  return n;
}

// rank returns the candidates worth relaying, best first. A playlist the page
// read and found ended is dropped. A variant of a master that also made the
// list is dropped too, so the daemon gets the master and keeps the choice of
// quality.
export function rank(hits: Hit[], limit = 10): Candidate[] {
  const seen = [...collect(hits).values()].filter((s) => !s.ended);
  seen.sort((a, b) => score(b) - score(a) || a.order - b.order);
  return seen.slice(0, limit).map((s) => ({ url: s.url, format: s.format, frame: s.frame }));
}

// isLiveEnough reports whether hits already hold a stream worth stopping for:
// something playing, a playlist re-read, or an FLV stream requested.
export function isLiveEnough(hits: Hit[]): boolean {
  for (const s of collect(hits).values()) {
    if (s.ended) continue;
    if (s.kinds.has("playing") || s.requests >= 2) return true;
    if (s.format === "flv" && s.requests >= 1) return true;
  }
  return false;
}
