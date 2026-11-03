// Package webhook implements notify.Notifier with an HTTP POST of a JSON body.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/coldcoder126/rein/x/notify"
)

// WithTimeout overrides the default HTTP client timeout (10 seconds).
func WithTimeout(d time.Duration) Option {
	return func(n *Notifier) {
		n.client = &http.Client{Timeout: d}
	}
}

// Option configures a Notifier.
type Option func(*Notifier)

// WithHeader adds a static HTTP header to every request.
func WithHeader(key, value string) Option {
	return func(n *Notifier) {
		if n.headers == nil {
			n.headers = make(map[string]string)
		}
		n.headers[key] = value
	}
}

// WithBearerToken sets the Authorization: Bearer <token> header.
func WithBearerToken(token string) Option {
	return WithHeader("Authorization", "Bearer "+token)
}

// Notifier sends notifications as HTTP POST requests with a JSON body of the form:
//
//	{"title":"…","body":"…","level":"info|warn|error"}
type Notifier struct {
	url     string
	headers map[string]string
	client  *http.Client
}

// New creates a webhook Notifier for the given URL.
func New(webhookURL string, opts ...Option) *Notifier {
	n := &Notifier{
		url:    webhookURL,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// Notify sends msg as a JSON POST request to the configured URL.
func (n *Notifier) Notify(ctx context.Context, msg notify.Message) error {
	payload := map[string]string{
		"title": msg.Title,
		"body":  msg.Body,
		"level": levelStr(msg.Level),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webhook: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range n.headers {
		req.Header.Set(k, v)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook: HTTP %d from %s", resp.StatusCode, n.url)
	}
	return nil
}

func levelStr(l notify.Level) string {
	switch l {
	case notify.LevelWarn:
		return "warn"
	case notify.LevelError:
		return "error"
	default:
		return "info"
	}
}
