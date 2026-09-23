package vlapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
)

func Request[Req any, Res any](nc *nats.Conn, subject string, req Req) (*Res, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	msg, err := nc.RequestWithContext(
		context.Background(),
		subject,
		b,
	)
	if err != nil {
		return nil, err // transport error
	}

	var resp Response[Res]
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, err
	}

	if !resp.OK {
		code, ok := errByCode[resp.Error.Code]
		if !ok {
			code = errInternal
		}
		return nil, fmt.Errorf("%w: %s", code, resp.Error.Message)
	}

	return resp.Data, nil
}
