package hlsingest

import (
	"context"
	"time"
)

// maxParallelFetches bounds how many of a poll's new segments are downloaded at
// once.
//
// One at a time, an Origin is read no faster than one segment per round trip,
// and a far-away Origin answers a one-second segment in one to two seconds
// (Tango's Indian edges, read from Germany, take 1.1-2.4s): the ingest falls
// behind the live edge and the window slides past segments it never reached.
// Overlapping the downloads keeps it at the Origin's pace however far away it
// is; a few at once is enough to cover a round trip several segments long.
const maxParallelFetches = 4

// segmentPrefetch downloads a poll's new segments concurrently. Nothing is
// handed over out of order: the poll collects each one with get, in playlist
// order, and stops at the first that failed exactly as it would have fetching
// them one by one.
type segmentPrefetch struct {
	results map[string]*prefetched
	stop    chan struct{}
}

type prefetched struct {
	done chan struct{}
	body []byte
	err  error
}

// prefetch starts downloading urls, in order, at most maxParallelFetches at a
// time, each with fetchRetry's attempts and budget.
func (o *ingest) prefetch(ctx context.Context, urls []string, budget time.Duration) *segmentPrefetch {
	p := &segmentPrefetch{
		results: make(map[string]*prefetched, len(urls)),
		stop:    make(chan struct{}),
	}
	for _, u := range urls {
		p.results[u] = &prefetched{done: make(chan struct{})}
	}
	go func() {
		slots := make(chan struct{}, maxParallelFetches)
		for _, u := range urls {
			select {
			case slots <- struct{}{}:
			case <-p.stop:
				return
			case <-ctx.Done():
				return
			}
			r := p.results[u]
			go func() {
				defer func() { <-slots }()
				r.body, r.err = o.fetchRetry(ctx, u, budget)
				close(r.done)
			}()
		}
	}()
	return p
}

// get returns u's download, waiting for it to finish. A URL that was not
// prefetched reports false.
func (p *segmentPrefetch) get(ctx context.Context, u string) ([]byte, bool, error) {
	r, ok := p.results[u]
	if !ok {
		return nil, false, nil
	}
	select {
	case <-r.done:
		return r.body, true, r.err
	case <-ctx.Done():
		return nil, true, ctx.Err()
	}
}

// close starts no more downloads. Those under way finish on their own, and
// their bodies are dropped: a poll that stopped early fetches them again on
// the next one, in order.
func (p *segmentPrefetch) close() {
	close(p.stop)
}
