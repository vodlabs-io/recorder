import type { VODListPage, VOD } from "../types";

// Sessions per vods.list request, matching the daemon's per-page cap.
// Fewer, larger pages mean fewer round-trips, which matters most on the
// Dashboard's high-latency cloud hop.
//
// This is an upper bound, not a promise: the daemon also caps a page by its
// serialized size (a session carries every file it holds, so sizes vary
// enormously) and will return fewer sessions than asked for. The paging loop
// below advances by how many sessions it has actually received, never by this
// limit, so a short page simply costs another request.
const PAGE_LIMIT = 1000;

// Hard stop for the paging loop so a daemon bug (e.g. a total that keeps
// growing) can't spin us forever. 100k sessions is far beyond design scale.
const MAX_PAGES = 100;

// One retry pass when the listing's rev moves mid-pull (a VOD landed between
// pages, shifting offsets); beyond that we return what we have — the daemon
// announces the new rev anyway, so the caller's next refresh reconciles.
const MAX_PULL_ATTEMPTS = 3;

// fetchPage requests one page of the daemon's VOD Archive listing. Implementations
// differ only in how the request reaches the daemon (a NATS session or the
// desktop app's bun RPC). rescan (honoured only at offset 0) forces the daemon
// to reconcile against the filesystem first.
export type RecordingPageFn = (
  offset: number,
  limit: number,
  rescan?: boolean,
) => Promise<VODListPage>;

// VODListSnapshot is the aggregated listing state after one or more pages.
export type VODListSnapshot = {
  sessions: VOD[];
  unavailableRoots: string[];
  scannedAt: string;
  // Content generation of the listing ("" when the daemon sent none).
  // Pass it back as knownRev to skip re-downloading an unchanged listing. It's
  // a stringified int64 (unix ns), so compare it for equality, never ordering.
  rev: string;
  scanning: boolean;
  // Daemon-reported session count across all pages.
  total: number;
};

export type RecordingFetchResult = VODListSnapshot & {
  // True when the daemon's rev matched knownRev: the VOD Archive content is
  // unchanged and sessions were NOT re-downloaded (the array is empty; keep
  // your current copy). The other fields — scanning, unavailableRoots,
  // scannedAt — are fresh either way.
  unchanged: boolean;
};

function snapshotOf(page: VODListPage, sessions: VOD[]): VODListSnapshot {
  return {
    sessions,
    unavailableRoots: page.unavailable_roots ?? [],
    scannedAt: page.scanned_at,
    rev: page.rev ?? "",
    scanning: page.scanning ?? false,
    total: page.total,
  };
}

// fetchRecordings pulls the VOD Archive listing page by page.
//
// - Progressive: onPage fires after every page with the aggregate so far
//   (done marks the last one), so callers can render the first page while the
//   rest downloads.
// - Conditional: with knownRev set, a one-session probe checks the daemon's
//   current rev first and skips the whole download when it hasn't moved
//   (unchanged: true) — the common case for focus/nudge refreshes.
// - Consistent: if the rev moves between pages, the pull restarts so offsets
//   can't skip or duplicate sessions.
// - rescan forces the daemon to reconcile against the filesystem first; it
//   implies a full re-download (knownRev is ignored), and is sent only on the
//   very first request.
export async function fetchRecordings(
  fetchPage: RecordingPageFn,
  opts: {
    knownRev?: string;
    rescan?: boolean;
    onPage?: (partial: VODListSnapshot & { done: boolean }) => void;
  } = {},
): Promise<RecordingFetchResult> {
  // A rescan always re-downloads, so a conditional probe would be pointless
  // (the walk is async — the rev hasn't moved yet by the time we'd probe).
  const knownRev = opts.rescan ? "" : opts.knownRev;
  if (knownRev) {
    const probe = await fetchPage(0, 1);
    if (probe.rev && probe.rev === knownRev) {
      return { ...snapshotOf(probe, []), unchanged: true };
    }
  }

  let last: VODListSnapshot | null = null;
  for (let attempt = 0; attempt < MAX_PULL_ATTEMPTS; attempt++) {
    const sessions: VOD[] = [];
    let pullRev: string | null = null;
    let restart = false;

    for (let i = 0; i < MAX_PAGES; i++) {
      // Force a reconcile on the very first request only (offset 0, first
      // attempt); later pages/attempts must serve from the same walk.
      const rescan = opts.rescan === true && attempt === 0 && i === 0;
      const page = await fetchPage(sessions.length, PAGE_LIMIT, rescan);
      if (pullRev === null) {
        pullRev = page.rev ?? "";
      } else if ((page.rev ?? "") !== pullRev) {
        restart = true; // content changed mid-pull; offsets are now unreliable
        break;
      }
      sessions.push(...page.sessions);
      last = snapshotOf(page, sessions);
      const done = sessions.length >= page.total || page.sessions.length === 0;
      opts.onPage?.({ ...last, done });
      if (done) {
        return { ...last, unchanged: false };
      }
    }
    if (!restart) {
      break; // MAX_PAGES safety stop: serve what we have
    }
  }
  // Ran out of attempts (or pages). Return the last aggregate; the daemon's
  // rev announcement will trigger the reconciling refresh.
  if (last) {
    return { ...last, unchanged: false };
  }
  return {
    sessions: [],
    unavailableRoots: [],
    scannedAt: "",
    rev: "",
    scanning: false,
    total: 0,
    unchanged: false,
  };
}
