package vlapi

import (
	"encoding/json"
	"log"

	"github.com/nats-io/nats.go/micro"
)

type Response[T any] struct {
	OK    bool `json:"ok"`
	Data  *T   `json:"data,omitempty"`
	Error *Err `json:"error,omitempty"`
}

func OK[T any](data T) Response[T] {
	return Response[T]{
		OK:   true,
		Data: &data,
	}
}

func fail(code error, msg string) Response[Err] {
	return Response[Err]{
		OK: false,
		Error: &Err{
			Code:    code.Error(),
			Message: msg,
		},
	}
}

// respond sends a handler's reply, falling back to an error envelope when the
// reply itself cannot be delivered (e.g. a payload over the NATS max payload).
// Without a reply the caller would wait out its timeout, unable to tell a dead
// daemon from an undeliverable answer.
func respond[T any](req micro.Request, resp Response[T]) {
	b, err := json.Marshal(resp)
	if err != nil {
		log.Printf("Failed to encode response: %v", err)
		respondUndeliverable(req, "failed to encode response")
		return
	}
	if err := req.Respond(b); err != nil {
		log.Printf("Failed to respond (%d bytes): %v", len(b), err)
		respondUndeliverable(req, err.Error())
	}
}

// respondUndeliverable is the last-resort reply after respond's own send
// failed. It never calls respond, so a broken reply subject cannot recurse.
func respondUndeliverable(req micro.Request, msg string) {
	b, err := json.Marshal(fail(errInternal, msg))
	if err != nil {
		return
	}
	if err := req.Respond(b); err != nil {
		log.Printf("Failed to send error response: %v", err)
	}
}
