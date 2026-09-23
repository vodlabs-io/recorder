// Package useragent holds the daemon's last-resort browser User-Agent. The
// cloud API normally supplies the UA with each scrape request and with a live
// stream's recording headers, so resolving and recording use the same one.
package useragent

// Fallback is used only when the cloud API supplied no UA, so the daemon never
// sends Go's default. Keep it a plausible, recent desktop Chrome.
const Fallback = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
