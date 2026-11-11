package anthropic

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	goanthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/coldcoder126/rein/chat"
)

// toolSearchVariant returns the tool search to add to this request, or "" when the request goes without one:
// it needs deferred tools, a model that supports tool search, and a client that has not turned it off.
func (c *Client) toolSearchVariant(spec modelSpec, tools []chat.ToolDef) ToolSearch {
	if !spec.toolSearch || c.cfg.ToolSearch == ToolSearchOff {
		return ""
	}
	if !slices.ContainsFunc(tools, func(def chat.ToolDef) bool { return def.Deferred }) {
		return ""
	}
	if c.cfg.ToolSearch == ToolSearchBM25 {
		return ToolSearchBM25
	}
	return ToolSearchRegex
}

// searchTool is the definition of the server-side tool search. It goes after the request's own tools, so turning it on
// leaves their order unchanged; it is never deferred, which also satisfies the API's rule that some tool must load up front.
func searchTool(variant ToolSearch) goanthropic.ToolUnionParam {
	if variant == ToolSearchBM25 {
		return goanthropic.ToolUnionParam{OfToolSearchToolBm25_20251119: &goanthropic.ToolSearchToolBm25_20251119Param{
			Type: goanthropic.ToolSearchToolBm25_20251119TypeToolSearchToolBm25_20251119,
		}}
	}
	return goanthropic.ToolUnionParam{OfToolSearchToolRegex20251119: &goanthropic.ToolSearchToolRegex20251119Param{
		Type: goanthropic.ToolSearchToolRegex20251119TypeToolSearchToolRegex20251119,
	}}
}

// isServerToolBlock reports whether a response block is a record of a tool the server ran: its call or its result.
func isServerToolBlock(blockType string) bool {
	return blockType == "server_tool_use" || (strings.HasSuffix(blockType, "_tool_result") && blockType != "tool_result")
}

// serverToolRecord completes a server tool block from content_block_start with its streamed input and returns the block
// to store, plus its name: the tool name for a call, the block type for a result.
func serverToolRecord(start, input string) (json.RawMessage, string, error) {
	var block map[string]json.RawMessage
	if err := json.Unmarshal([]byte(start), &block); err != nil {
		return nil, "", errors.New("invalid server tool block: " + err.Error())
	}
	var kind, name string
	_ = json.Unmarshal(block["type"], &kind)
	name = kind
	if kind == "server_tool_use" {
		_ = json.Unmarshal(block["name"], &name)
		if input != "" {
			if !json.Valid([]byte(input)) {
				return nil, "", errors.New("server tool " + name + " has invalid input")
			}
			block["input"] = json.RawMessage(input)
		}
	}
	raw, err := json.Marshal(block)
	if err != nil {
		return nil, "", err
	}
	return raw, name, nil
}

// serverToolParam replays a stored server tool block exactly as the API returned it.
func serverToolParam(raw json.RawMessage) goanthropic.ContentBlockParamUnion {
	return param.Override[goanthropic.ContentBlockParamUnion](raw)
}
