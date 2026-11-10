// Package mcp exposes the tools of an MCP (Model Context Protocol) server as a toolset; add a Client to Agent.Toolsets to use it.
//
// It is built on the official Go SDK, which negotiates the protocol version (the stateless 2026-07-28 revision, falling back to
// the initialize handshake of older servers). Only tools are supported: resources, prompts, sampling, elicitation and roots are not.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/tool"
)

// maxToolNameLen is the longest tool name model providers accept.
const maxToolNameLen = 64

// statelessVersion is the first protocol revision whose tools/list results carry a cache TTL.
const statelessVersion = "2026-07-28"

// Transport is how a Client reaches its server. A new connection is made from it every time the Client connects.
type Transport struct {
	newTransport func() sdk.Transport
}

// Stdio runs the server as a local subprocess and talks to it over stdin and stdout.
// newCmd is called on every connection, because an exec.Cmd can only be started once.
func Stdio(newCmd func() *exec.Cmd) Transport {
	return Transport{newTransport: func() sdk.Transport { return &sdk.CommandTransport{Command: newCmd()} }}
}

// HTTP connects to a remote server over the Streamable HTTP transport. A nil client uses http.DefaultClient;
// credentials such as a bearer token are added by the client's RoundTripper.
func HTTP(endpoint string, client *http.Client) Transport {
	return Transport{newTransport: func() sdk.Transport {
		return &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}
	}}
}

// Option configures a Client.
type Option func(*Client)

// WithFilter exposes only the tools for which keep returns true; it receives the tool's name on the server.
func WithFilter(keep func(name string) bool) Option {
	return func(c *Client) { c.filter = keep }
}

// WithTimeout sets the timeout of every call to this server's tools, taking precedence over Agent.ToolTimeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithDeferLoading marks every tool of the server as deferred (see chat.ToolDef.Deferred): the model finds them through
// the provider's tool search instead of seeing them all up front. Suited to servers with many tools.
func WithDeferLoading() Option {
	return func(c *Client) { c.deferLoading = true }
}

// WithoutInstructions stops the server's instructions from being appended to the system prompt.
func WithoutInstructions() Option {
	return func(c *Client) { c.noInstructions = true }
}

// Client connects to one MCP server and provides its tools as a tool.Toolset. One Client can be shared by any number of
// sessions and runs; Close it when it is no longer needed.
//
// The connection is made on the first run and checked at the start of every run: when it has dropped, the Client reconnects.
// A server that cannot be reached does not fail the run: it contributes no tools, and the system prompt tells the model it is unavailable.
//
// Tools are named mcp__{server}__{tool}, with characters other than letters, digits, '_' and '-' replaced by '_'. A tool whose
// name is still invalid (longer than 64 characters, or colliding with another tool of the server) is skipped and reported in the system prompt.
// The tool list is cached until the server reports a change or, on servers of the 2026-07-28 revision, the cache TTL it gives expires.
type Client struct {
	name           string
	prefix         string
	transport      Transport
	filter         func(string) bool
	timeout        time.Duration
	noInstructions bool
	deferLoading   bool
	sdk            *sdk.Client

	// session is the current connection, nil when disconnected. It is atomic so that tool calls never wait for mu,
	// which Tools holds while connecting and listing.
	session atomic.Pointer[sdk.ClientSession]
	// stale is set by list-changed notifications, which the SDK may deliver while Tools holds mu.
	stale atomic.Bool

	// mu guards the listing state below and serializes connecting and listing.
	mu      sync.Mutex
	closed  bool
	connErr error
	listed  bool
	expires time.Time // zero means valid until the server reports a change
	tools   []tool.Tool
	skipped []string
}

// New creates a Client for the server reached through transport. name identifies the server in tool names and the
// system prompt; it should be short and unique among the servers of an Agent. New does not connect.
// An empty name is a programming error and panics.
func New(name string, transport Transport, opts ...Option) *Client {
	if strings.TrimSpace(name) == "" {
		panic("mcp: server name must not be empty")
	}
	c := &Client{name: name, prefix: "mcp__" + sanitize(name) + "__", transport: transport}
	for _, opt := range opts {
		opt(c)
	}
	c.sdk = sdk.NewClient(&sdk.Implementation{Name: "rein", Version: version()}, &sdk.ClientOptions{
		// Declare no client capabilities: this client answers no server requests (roots, sampling, elicitation).
		Capabilities:           &sdk.ClientCapabilities{},
		MultiRoundTrip:         &sdk.MultiRoundTripOptions{Disabled: true},
		ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) { c.stale.Store(true) },
	})
	return c
}

// Tools connects when needed and returns the server's tools. When the server cannot be reached it returns no tools and no error.
func (c *Client) Tools(ctx context.Context, _ tool.RunInfo) ([]tool.Tool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("mcp: client of server %q is closed", c.name)
	}
	c.connErr = nil
	cs := c.session.Load()
	if cs == nil {
		var err error
		if cs, err = c.connect(ctx); err != nil {
			c.connErr = err
			return nil, nil
		}
	}
	if c.listed && !c.stale.Load() && (c.expires.IsZero() || time.Now().Before(c.expires)) {
		return c.tools, nil
	}
	// A failed refresh keeps the tools listed before; only a first listing that fails makes the server unavailable.
	if err := c.list(ctx, cs); err != nil && !c.listed {
		c.connErr = err
		return nil, nil
	}
	return c.tools, nil
}

// Instructions returns the server's instructions together with notes on skipped tools, or explains why the server is unavailable.
func (c *Client) Instructions(context.Context, tool.RunInfo) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connErr != nil {
		return fmt.Sprintf("<mcp_server name=%q status=\"unavailable\">\nThe server could not be reached, so its tools are not available in this run: %v\n</mcp_server>",
			c.name, c.connErr), nil
	}
	var parts []string
	if cs := c.session.Load(); cs != nil && !c.noInstructions {
		if init := cs.InitializeResult(); init != nil && strings.TrimSpace(init.Instructions) != "" {
			parts = append(parts, strings.TrimSpace(init.Instructions))
		}
	}
	parts = append(parts, c.skipped...)
	if len(parts) == 0 {
		return "", nil
	}
	return fmt.Sprintf("<mcp_server name=%q>\n%s\n</mcp_server>", c.name, strings.Join(parts, "\n")), nil
}

// Close disconnects from the server, stopping the subprocess of a Stdio transport. The Client cannot be used afterwards.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	cs := c.session.Swap(nil)
	if cs == nil {
		return nil
	}
	return cs.Close()
}

// connect opens a session and watches it, so that the next run reconnects once it ends. Called with mu held.
func (c *Client) connect(ctx context.Context) (*sdk.ClientSession, error) {
	cs, err := c.sdk.Connect(ctx, c.transport.newTransport(), nil)
	if err != nil {
		return nil, err
	}
	c.session.Store(cs)
	c.listed = false
	go func() {
		_ = cs.Wait()
		c.session.CompareAndSwap(cs, nil)
	}()
	return cs, nil
}

// list fetches every page of tools/list from cs and rebuilds the tools. Called with mu held.
func (c *Client) list(ctx context.Context, cs *sdk.ClientSession) error {
	c.stale.Store(false)
	var remote []*sdk.Tool
	ttl, cursor := -1, ""
	for {
		res, err := cs.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return err
		}
		remote = append(remote, res.Tools...)
		if ttl < 0 || res.TTLMs < ttl {
			ttl = res.TTLMs
		}
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	c.expires = time.Time{}
	if init := cs.InitializeResult(); init != nil && init.ProtocolVersion >= statelessVersion {
		c.expires = time.Now().Add(time.Duration(ttl) * time.Millisecond)
	}
	c.tools, c.skipped = nil, nil
	seen := make(map[string]bool, len(remote))
	for _, t := range remote {
		if c.filter != nil && !c.filter(t.Name) {
			continue
		}
		name := c.prefix + sanitize(t.Name)
		schema, err := json.Marshal(t.InputSchema)
		var reason string
		switch {
		case len(name) > maxToolNameLen:
			reason = fmt.Sprintf("its name %s is longer than %d characters", name, maxToolNameLen)
		case seen[name]:
			reason = fmt.Sprintf("its name %s collides with another tool of this server", name)
		case err != nil:
			reason = fmt.Sprintf("its input schema is invalid: %v", err)
		}
		if reason != "" {
			c.skipped = append(c.skipped, fmt.Sprintf("Tool %q of this server is unavailable: %s.", t.Name, reason))
			continue
		}
		seen[name] = true
		c.tools = append(c.tools, &Tool{
			client:      c,
			remoteName:  t.Name,
			def:         chat.ToolDef{Name: name, Description: t.Description, InputSchema: schema, Deferred: c.deferLoading},
			annotations: annotationsOf(t.Annotations),
		})
	}
	c.listed = true
	return nil
}

// Tool is a tool of an MCP server. Middlewares can type-assert a tool.Tool to *Tool to see which server it belongs to
// and the behavior hints the server declared.
type Tool struct {
	client      *Client
	remoteName  string
	def         chat.ToolDef
	annotations Annotations
}

// Annotations are the behavior hints a server declares for a tool, with the protocol defaults filled in.
// They are untrusted unless the server is trusted, so the framework does not act on them.
type Annotations struct {
	// Title is a human-readable name of the tool.
	Title string
	// ReadOnly reports that the tool does not modify its environment.
	ReadOnly bool
	// Destructive reports that the tool may perform destructive updates; meaningful only when ReadOnly is false.
	Destructive bool
	// Idempotent reports that repeating a call with the same arguments has no additional effect.
	Idempotent bool
	// OpenWorld reports that the tool interacts with external entities.
	OpenWorld bool
}

func (t *Tool) Def() chat.ToolDef { return t.def }

// Server returns the name of the server the tool belongs to, as given to New.
func (t *Tool) Server() string { return t.client.name }

// RemoteName returns the tool's name on the server.
func (t *Tool) RemoteName() string { return t.remoteName }

// Annotations returns the behavior hints the server declared for the tool.
func (t *Tool) Annotations() Annotations { return t.annotations }

// Timeout implements tool.Timeouter; zero leaves the Agent's default in place.
func (t *Tool) Timeout() time.Duration { return t.client.timeout }

// Execute calls the tool on the server. A protocol or connection error is returned as an error, which the Agent gives to the model.
func (t *Tool) Execute(ctx *tool.Context, args json.RawMessage) (*tool.Result, error) {
	cs := t.client.session.Load()
	if cs == nil {
		return nil, fmt.Errorf("MCP server %q is disconnected", t.client.name)
	}
	params := &sdk.CallToolParams{Name: t.remoteName}
	if len(args) > 0 {
		params.Arguments = args
	}
	res, err := cs.CallTool(ctx, params)
	if err != nil {
		if errors.Is(err, sdk.ErrConnectionClosed) {
			t.client.session.CompareAndSwap(cs, nil)
		}
		return nil, err
	}
	if res.NeedsInput() {
		return nil, errors.New("the tool asked for user input, which this client does not support")
	}
	return convert(res), nil
}

// convert turns an MCP tool result into a tool result: text and media become content blocks, and structured content goes to Details.
func convert(res *sdk.CallToolResult) *tool.Result {
	out := &tool.Result{IsError: res.IsError, Details: res.StructuredContent}
	for _, content := range res.Content {
		switch c := content.(type) {
		case *sdk.TextContent:
			out.Content = append(out.Content, chat.Text(c.Text))
		case *sdk.ImageContent:
			out.Content = append(out.Content, media(c.Data, c.MIMEType))
		case *sdk.AudioContent:
			out.Content = append(out.Content, media(c.Data, c.MIMEType))
		case *sdk.ResourceLink:
			out.Content = append(out.Content, chat.Text(fmt.Sprintf("[resource link: %s %s]", c.Name, c.URI)))
		case *sdk.EmbeddedResource:
			if r := c.Resource; r != nil && r.Blob != nil {
				out.Content = append(out.Content, media(r.Blob, r.MIMEType))
			} else if r != nil {
				out.Content = append(out.Content, chat.Text(r.Text))
			}
		}
	}
	// Servers should mirror structured content as text, but when one does not the model still needs to see it.
	if len(out.Content) == 0 && res.StructuredContent != nil {
		if data, err := json.Marshal(res.StructuredContent); err == nil {
			out.Content = append(out.Content, chat.Text(string(data)))
		}
	}
	return out
}

func media(data []byte, mimeType string) chat.ContentBlock {
	block := chat.Media(data)
	if mimeType != "" {
		block = block.WithMIME(mimeType)
	}
	return block
}

func annotationsOf(a *sdk.ToolAnnotations) Annotations {
	out := Annotations{Destructive: true, OpenWorld: true}
	if a == nil {
		return out
	}
	out.Title, out.ReadOnly, out.Idempotent = a.Title, a.ReadOnlyHint, a.IdempotentHint
	if a.DestructiveHint != nil {
		out.Destructive = *a.DestructiveHint
	}
	if a.OpenWorldHint != nil {
		out.OpenWorld = *a.OpenWorldHint
	}
	return out
}

// sanitize replaces the characters model providers do not accept in tool names with '_'.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, name)
}

// version returns the version of this module for the client info sent to servers.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/coldcoder126/rein" {
				return dep.Version
			}
		}
	}
	return "(devel)"
}
