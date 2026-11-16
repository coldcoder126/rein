// Package history provides the built-in tool history: it lets the model look up the messages of its session that
// compaction replaced with a summary, since a summary keeps the gist but not every detail.
//
// It reads the session store directly, so pass the store the Runner uses. It only finds what the store keeps
// (see session.Store.ListMessages) and nothing that is still in the model context.
package history

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/tool"
)

// ToolName is the name of the history tool.
const ToolName = "history"

const (
	// maxMatches is how many messages a search returns.
	maxMatches = 20
	// snippetRunes is how much text a match shows on each side of the query.
	snippetRunes = 100
	// maxReadBytes is how much of one message a read returns.
	maxReadBytes = 8000
)

type params struct {
	Query string `json:"query,omitempty" jsonschema:"description=Text to search for, case-insensitive. Give either query or id."`
	ID    string `json:"id,omitempty" jsonschema:"description=The id of a message to read in full, as a search returned it. Give either query or id."`
}

const description = `Look up earlier messages of this conversation that were replaced by a summary to save context.
The summary keeps the gist; use this tool when you need an exact detail from before it, such as a value, a name or a tool result.

- query: search those messages for text and get the matching ones with a snippet each.
- id: read one of them in full.

Messages still in the conversation are not searched; read them directly.`

// Tool returns the history tool reading from store; add it to Agent.Tools to use it.
func Tool(store session.Store) tool.Tool {
	return tool.New(ToolName, description, func(ctx *tool.Context, in params) (*tool.Result, error) {
		query, id := strings.TrimSpace(in.Query), strings.TrimSpace(in.ID)
		if (query == "") == (id == "") {
			return nil, errors.New("give exactly one of query and id")
		}
		if ctx.SessionID == "" {
			return nil, errors.New("there is no session history here")
		}
		all, err := store.ListMessages(ctx, ctx.SessionID)
		if err != nil {
			return nil, fmt.Errorf("read session history: %w", err)
		}
		compacted := session.Compacted(all)
		if len(compacted) == 0 {
			return tool.Text("No earlier messages: nothing in this conversation has been replaced by a summary."), nil
		}
		if id != "" {
			return read(compacted, id)
		}
		return tool.Text(search(compacted, query)), nil
	})
}

func read(messages []session.Message, id string) (*tool.Result, error) {
	for _, msg := range messages {
		if msg.ID != id {
			continue
		}
		text := msg.Transcript()
		if len(text) > maxReadBytes {
			text = strings.ToValidUTF8(text[:maxReadBytes], "") + "\n[truncated]"
		}
		return tool.Text(header(msg) + "\n" + text), nil
	}
	return nil, fmt.Errorf("no earlier message has id %q", id)
}

func search(messages []session.Message, query string) string {
	q := strings.Map(unicode.ToLower, query)
	var sb strings.Builder
	found := 0
	for _, msg := range messages {
		snippet, ok := match(msg.Transcript(), q)
		if !ok {
			continue
		}
		if found++; found <= maxMatches {
			fmt.Fprintf(&sb, "%s\n%s\n\n", header(msg), snippet)
		}
	}
	switch {
	case found == 0:
		return fmt.Sprintf("No earlier message contains %q.", query)
	case found > maxMatches:
		fmt.Fprintf(&sb, "%d more messages match; narrow the query to see them.", found-maxMatches)
	}
	return strings.TrimSpace(sb.String())
}

// match reports whether text contains the lowercase query q, ignoring case, and returns the text around the first match.
func match(text, q string) (string, bool) {
	// Lowering rune by rune keeps the rune positions of text, so the match can be cut from the original.
	lower := strings.Map(unicode.ToLower, text)
	i := strings.Index(lower, q)
	if i < 0 {
		return "", false
	}
	runes := []rune(text)
	start := utf8.RuneCountInString(lower[:i])
	end := start + utf8.RuneCountInString(q)
	from, to := max(start-snippetRunes, 0), min(end+snippetRunes, len(runes))
	snippet := string(runes[from:to])
	if from > 0 {
		snippet = "…" + snippet
	}
	if to < len(runes) {
		snippet += "…"
	}
	return snippet, true
}

func header(msg session.Message) string {
	return fmt.Sprintf("[id=%s type=%s time=%s]", msg.ID, msg.Type, msg.CreatedAt.Format("2006-01-02 15:04:05"))
}
