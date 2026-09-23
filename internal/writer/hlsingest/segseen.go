package hlsingest

// segSeen remembers which segments have already been handed to the Sink.
//
// What identifies a segment depends on whether the Origin's URLs are stable.
// Normally the absolute URL names it, and a set of URLs is the whole answer. An
// Origin that re-signs its playlist (Config.RefreshURL) hands out a different
// URL for the same content after every refresh, so there the URL names the
// grant rather than the segment; the playlist's media sequence number is what
// survives, and a cursor over it also bounds what has to be remembered.
type segSeen struct {
	// bySeq selects the media-sequence cursor over the URL set.
	bySeq bool
	urls  map[string]bool
	// next is the lowest media sequence not yet taken. positioned reports
	// whether anything has set it, so sequence zero is not read as "seen".
	next       uint64
	positioned bool
}

func newSegSeen(bySeq bool) *segSeen {
	return &segSeen{bySeq: bySeq, urls: make(map[string]bool)}
}

// has reports whether this segment has already been handed over.
func (s *segSeen) has(seq uint64, url string) bool {
	if s.bySeq {
		return s.positioned && seq < s.next
	}
	return s.urls[url]
}

// mark records a segment as handed over (or deliberately dropped).
func (s *segSeen) mark(seq uint64, url string) {
	if s.bySeq {
		s.startAt(seq + 1)
		return
	}
	s.urls[url] = true
}

// startAt moves the cursor forward to seq, never backwards: a playlist that
// re-publishes older segments must not rewind what has been recorded.
func (s *segSeen) startAt(seq uint64) {
	if !s.positioned || seq > s.next {
		s.next, s.positioned = seq, true
	}
}

// prune drops URLs the Origin no longer publishes, so a long Session does not
// accumulate a set the size of everything it has ever seen. Nothing to do in
// sequence mode, where the cursor is already the whole memory.
func (s *segSeen) prune(current map[string]bool) {
	if s.bySeq {
		return
	}
	for u := range s.urls {
		if !current[u] {
			delete(s.urls, u)
		}
	}
}

// len reports how many URLs are remembered, for deciding when to prune.
func (s *segSeen) len() int { return len(s.urls) }
