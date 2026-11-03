package chat

import (
	"context"
	"errors"
	"iter"
	"time"
)

// UsageContext holds business attribution for one model call.
// These fields change per request, so they travel in context.Context rather than in the Client configuration.
type UsageContext struct {
	RequestID string
	UserID    string
	SessionID string
	TraceID   string
	Source    string
	Metadata  map[string]string
}

type usageContextKey struct{}

// WithUsageContext stores business attribution in ctx for the usage recorder.
func WithUsageContext(ctx context.Context, usageCtx UsageContext) context.Context {
	usageCtx.Metadata = cloneStringMap(usageCtx.Metadata)
	return context.WithValue(ctx, usageContextKey{}, usageCtx)
}

// UsageContextFromContext reads business attribution from ctx.
func UsageContextFromContext(ctx context.Context) UsageContext {
	if usageCtx, ok := ctx.Value(usageContextKey{}).(UsageContext); ok {
		usageCtx.Metadata = cloneStringMap(usageCtx.Metadata)
		return usageCtx
	}
	return UsageContext{}
}

// UsageEvent is the usage event emitted after a model request completes.
type UsageEvent struct {
	Provider  string
	Model     string
	Method    string
	Usage     Usage
	Error     string
	StartedAt time.Time
	EndedAt   time.Time

	RequestID string
	UserID    string
	SessionID string
	TraceID   string
	Source    string
	Metadata  map[string]string
}

// UsageRecorder is implemented by the caller to persist or report model usage.
// Recorder errors do not affect the model call.
type UsageRecorder func(ctx context.Context, event UsageEvent) error

// UsageOption configures the usage recorder wrapper.
type UsageOption func(*usageRecorderClient)

// WithUsageProviderName sets the provider name in usage events.
func WithUsageProviderName(name string) UsageOption {
	return func(c *usageRecorderClient) {
		c.providerName = name
	}
}

// WithUsageRecorder adds usage recording to a Client.
func WithUsageRecorder(base Client, recorder UsageRecorder, opts ...UsageOption) Client {
	if base == nil || recorder == nil {
		return base
	}
	c := usageRecorderClient{
		base:     base,
		recorder: recorder,
	}
	for _, opt := range opts {
		opt(&c)
	}
	return &c
}

type usageRecorderClient struct {
	base         Client
	recorder     UsageRecorder
	providerName string
}

func (c *usageRecorderClient) Stream(ctx context.Context, req Request) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		startedAt := time.Now()
		var usage Usage
		var streamErr error
		completed := false

		defer func() {
			if !completed && streamErr == nil {
				streamErr = errors.New("stream stopped before completion")
			}
			c.record(ctx, req, "stream", usage, startedAt, streamErr)
		}()

		for event := range c.base.Stream(ctx, req) {
			if event.Message != nil {
				usage = event.Message.Usage
			}
			switch event.Type {
			case EventDone:
				completed = true
			case EventError:
				completed = true
				streamErr = event.Err
				if streamErr == nil {
					streamErr = errors.New(event.Message.ErrorMessage)
				}
			}
			if !yield(event) {
				return
			}
		}
	}
}

func (c *usageRecorderClient) Info() ModelInfo {
	return c.base.Info()
}

func (c *usageRecorderClient) record(ctx context.Context, req Request, method string, usage Usage, startedAt time.Time, err error) {
	info := c.base.Info()
	provider := c.providerName
	if provider == "" {
		provider = info.Provider
	}
	model := req.Options.Model
	if model == "" {
		model = info.Model
	}
	usageCtx := UsageContextFromContext(ctx)
	event := UsageEvent{
		Provider:  provider,
		Model:     model,
		Method:    method,
		Usage:     usage,
		StartedAt: startedAt,
		EndedAt:   time.Now(),

		RequestID: usageCtx.RequestID,
		UserID:    usageCtx.UserID,
		SessionID: usageCtx.SessionID,
		TraceID:   usageCtx.TraceID,
		Source:    usageCtx.Source,
		Metadata:  mergeMetadata(usageCtx.Metadata, req.Metadata),
	}
	if err != nil {
		event.Error = err.Error()
	}
	_ = c.recorder(ctx, event)
}

func mergeMetadata(base, override map[string]string) map[string]string {
	merged := cloneStringMap(base)
	for k, v := range override {
		if merged == nil {
			merged = make(map[string]string, len(override))
		}
		merged[k] = v
	}
	return merged
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
