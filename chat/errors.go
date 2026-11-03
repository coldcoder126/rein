package chat

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrContextLengthExceeded means the request exceeded the model's context window.
	ErrContextLengthExceeded = errors.New("context length exceeded")
	// ErrRateLimited means the model service rate limited the request.
	ErrRateLimited = errors.New("rate limited")
	// ErrUnauthorized means authentication with the model service failed.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrAborted means the caller canceled the request.
	ErrAborted = errors.New("aborted")
)

// ErrorKind classifies a failed model call, to decide whether to retry or to compact the context.
type ErrorKind string

const (
	KindUnknown        ErrorKind = "unknown"
	KindContextLength  ErrorKind = "context_length"
	KindRateLimit      ErrorKind = "rate_limit"
	KindQuota          ErrorKind = "quota"
	KindServer         ErrorKind = "server"
	KindNetwork        ErrorKind = "network"
	KindAuth           ErrorKind = "auth"
	KindInvalidRequest ErrorKind = "invalid_request"
	KindRefusal        ErrorKind = "refusal"
	// KindInvalidOutput means the model output does not have the required format (invalid JSON or not matching the schema).
	KindInvalidOutput ErrorKind = "invalid_output"
	KindAborted       ErrorKind = "aborted"
)

// Retryable reports whether this kind of error is usually transient, so a retry may succeed.
func (k ErrorKind) Retryable() bool {
	switch k {
	case KindRateLimit, KindServer, KindNetwork:
		return true
	default:
		return false
	}
}

// Error is the chat layer's unified model call error.
type Error struct {
	Kind ErrorKind
	// Status is the HTTP status code, or 0 when unknown.
	Status int
	// RetryAfter is the delay suggested by the server, or 0 when not provided.
	RetryAfter time.Duration
	Message    string
	// Err is the underlying original error.
	Err error
}

func (e *Error) Error() string {
	switch {
	case e.Message != "" && e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	case e.Message != "":
		return e.Message
	case e.Err != nil:
		return e.Err.Error()
	default:
		return string(e.Kind)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// Is lets errors.Is match sentinel errors by kind.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrContextLengthExceeded:
		return e.Kind == KindContextLength
	case ErrRateLimited:
		return e.Kind == KindRateLimit
	case ErrUnauthorized:
		return e.Kind == KindAuth
	case ErrAborted:
		return e.Kind == KindAborted
	}
	return false
}

// Retryable reports whether the error is worth retrying.
func (e *Error) Retryable() bool { return e.Kind.Retryable() }

// AsError converts any error to an *Error, returning an *Error unchanged.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	return &Error{Kind: KindUnknown, Err: err}
}
