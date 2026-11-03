package chat

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// HTTPHooks are debugging hooks at the raw HTTP layer. They sit below each provider's SDK and see the request body actually sent.
// Use them to inspect requests, read rate limit headers, or patch parameters that are not wrapped yet; for regular extensions use Middleware.
type HTTPHooks struct {
	// OnPayload receives the raw request body before it is sent; a non-nil return value replaces the body, nil leaves it unchanged.
	OnPayload func(ctx context.Context, body []byte) []byte
	// OnResponse is called after the response headers arrive, before the body is read.
	OnResponse func(ctx context.Context, status int, header http.Header)
}

// HTTPClient returns an http.Client with the hooks installed; without hooks it returns base unchanged. A nil base uses the default configuration.
func (h HTTPHooks) HTTPClient(base *http.Client) *http.Client {
	if h.OnPayload == nil && h.OnResponse == nil {
		return base
	}
	client := &http.Client{}
	if base != nil {
		*client = *base
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = hookTransport{base: transport, hooks: h}
	return client
}

type hookTransport struct {
	base  http.RoundTripper
	hooks HTTPHooks
}

func (t hookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.hooks.OnPayload != nil && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if replaced := t.hooks.OnPayload(req.Context(), body); replaced != nil {
			body = replaced
		}
		// A RoundTripper must not modify the request it receives, so replacing the body requires a copy first.
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	resp, err := t.base.RoundTrip(req)
	if err == nil && t.hooks.OnResponse != nil {
		t.hooks.OnResponse(req.Context(), resp.StatusCode, resp.Header)
	}
	return resp, err
}
