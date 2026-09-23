package vlerror

import "errors"

var (
	// Writer
	ErrWriteFailed = errors.New("failed to write to file")
	// ErrConnectFailed ends a part whose stream could not be (re)opened.
	ErrConnectFailed = errors.New("failed to connect")

	// Cloud
	ErrGetConsumerFailed = errors.New("failed to get consumer")
)
