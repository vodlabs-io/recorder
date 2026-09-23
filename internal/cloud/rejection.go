package cloud

// Rejection codes returned by the cloud API's add preflight and subscribe
// endpoints.
//
// codeUnavailable means the question could not be put, not that the answer was
// no, so no caller may treat it as a verdict about the streamer. Every other
// code is final, including one this build does not recognise.
const (
	codeUnsupportedSite = "unsupported_site"
	codeNotALiveURL     = "not_a_live_url"
	codeNotFound        = "not_found"
	codeUnavailable     = "unavailable"
	// codeOverCapacity is also not a verdict: the streamer is real but the
	// subscription is full, so an entry rejected for it must be kept.
	codeOverCapacity = "over_capacity"
)
