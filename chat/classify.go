package chat

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// overflowPatterns match the context overflow error messages returned by model providers.
var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt (?:is )?too long`),
	regexp.MustCompile(`(?i)request_too_large`),
	regexp.MustCompile(`(?i)input is too long for requested model`),
	regexp.MustCompile(`(?i)exceeds the context window`),
	regexp.MustCompile(`(?i)exceeds (?:the )?(?:model'?s )?maximum context length`),
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),
	regexp.MustCompile(`(?i)reduce the length of the messages`),
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),
	regexp.MustCompile(`(?i)exceeds (?:the )?maximum allowed input length`),
	regexp.MustCompile(`(?i)input \(\d+ tokens\) is longer than the model'?s context length`),
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),
	regexp.MustCompile(`(?i)exceeds the available context size`),
	regexp.MustCompile(`(?i)greater than the context length`),
	regexp.MustCompile(`(?i)context window exceeds limit`),
	regexp.MustCompile(`(?i)exceeded model token limit`),
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`),
	regexp.MustCompile(`(?i)model_context_window_exceeded`),
	regexp.MustCompile(`(?i)range of input length should be`),
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),
	regexp.MustCompile(`(?i)too many tokens`),
	regexp.MustCompile(`(?i)token limit exceeded`),
}

// notOverflowPatterns exclude rate limit messages that would otherwise match the overflow rules, such as "Too many tokens, please wait".
var notOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)rate.?limit`),
	regexp.MustCompile(`(?i)too many requests`),
	regexp.MustCompile(`(?i)throttl`),
	regexp.MustCompile(`(?i)please wait`),
}

var quotaPattern = regexp.MustCompile(`(?i)insufficient_quota|out of budget|quota exceeded|billing|usage limit reached|insufficient balance`)

var overloadedPattern = regexp.MustCompile(`(?i)overloaded|service.?unavailable|server.?error|internal.?error|high demand|provider.?returned.?error`)

var rateLimitPattern = regexp.MustCompile(`(?i)rate.?limit|too many requests`)

var authPattern = regexp.MustCompile(`(?i)authentication_error|permission_error|invalid api key|incorrect api key|unauthorized`)

var networkPattern = regexp.MustCompile(`(?i)connection (?:reset|refused|lost|error)|network.?error|fetch failed|other side closed|stream ended|unexpected eof|timed? ?out|timeout|no such host|broken pipe`)

// IsContextOverflow reports whether an error message means the context window was exceeded.
func IsContextOverflow(message string) bool {
	for _, p := range notOverflowPatterns {
		if p.MatchString(message) {
			return false
		}
	}
	for _, p := range overflowPatterns {
		if p.MatchString(message) {
			return true
		}
	}
	return false
}

// ClassifyStatus classifies an error by its HTTP status code and message.
func ClassifyStatus(status int, message string) ErrorKind {
	if IsContextOverflow(message) || status == http.StatusRequestEntityTooLarge {
		return KindContextLength
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return KindAuth
	case status == http.StatusTooManyRequests:
		if quotaPattern.MatchString(message) {
			return KindQuota
		}
		return KindRateLimit
	case status == http.StatusPaymentRequired:
		return KindQuota
	case status == http.StatusRequestTimeout || status == http.StatusConflict:
		return KindNetwork
	case status >= 500:
		return KindServer
	case status >= 400:
		return KindInvalidRequest
	}
	return ClassifyMessage(message)
}

// ClassifyMessage classifies an error by its message when there is no status code (for example an error event in the middle of a stream).
func ClassifyMessage(message string) ErrorKind {
	switch {
	case IsContextOverflow(message):
		return KindContextLength
	case quotaPattern.MatchString(message):
		return KindQuota
	case rateLimitPattern.MatchString(message):
		return KindRateLimit
	case overloadedPattern.MatchString(message):
		return KindServer
	case authPattern.MatchString(message):
		return KindAuth
	case networkPattern.MatchString(message):
		return KindNetwork
	}
	return KindUnknown
}

// WrapError wraps a provider's underlying error as an *Error.
//
// A finished ctx means the caller canceled, which is classified as KindAborted; when status is 0 the error type and message decide.
func WrapError(ctx context.Context, err error, status int, retryAfter time.Duration) *Error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return &Error{Kind: KindAborted, Message: "request canceled", Err: err}
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	kind := ClassifyStatus(status, err.Error())
	if status == 0 && kind == KindUnknown {
		var netErr net.Error
		if errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			kind = KindNetwork
		}
	}
	return &Error{Kind: kind, Status: status, RetryAfter: retryAfter, Err: err}
}

// ParseRetryAfter parses the server's suggested retry delay from the response headers, returning 0 when it cannot be parsed.
func ParseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	if v := strings.TrimSpace(h.Get("retry-after")); v != "" {
		if sec, err := strconv.ParseFloat(v, 64); err == nil && sec > 0 {
			return time.Duration(sec * float64(time.Second))
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 0
}
