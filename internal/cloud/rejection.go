package cloud

// Rejection codes returned by the cloud API's add preflight and subscribe
// endpoints.
//
// codeUnavailable means the question could not be put, not that the answer was
// no, so no caller may treat it as a verdict about the streamer.
const (
	codeUnsupportedSite = "unsupported_site"
	codeNotALiveURL     = "not_a_live_url"
	codeNotFound        = "not_found"
	codeUnavailable     = "unavailable"
	// codeOverCapacity is also not a verdict: the streamer is real but the
	// subscription is full, so an entry rejected for it must be kept.
	codeOverCapacity = "over_capacity"
	// codeDailyCapReached is not one either: the client has added as many new
	// streamers as it may today, and the quota resets the next UTC day.
	codeDailyCapReached = "daily_cap_reached"
)
