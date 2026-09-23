package cloud

import (
	"math/rand/v2"
	"time"
)

// jitterFraction is how far a retry wait is moved either side of its nominal
// length: ±20%, enough to spread a population out over a few seconds without
// making any single wait surprising to read in a log.
const jitterFraction = 0.2

// jitter spreads a retry wait so that daemons which failed together do not
// come back together.
//
// Every daemon holds a connection to the same service, and an outage ends for
// all of them at the same instant. Without this they would all re-subscribe on
// the same schedule afterwards — each one recreating a consumer and minting a
// token — and the recovery itself becomes the next outage. It matters more the
// more installs there are, which is to say it matters from now on.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := float64(d) * jitterFraction
	return time.Duration(float64(d) - spread + 2*spread*rand.Float64())
}
