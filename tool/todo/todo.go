// Package todo provides the built-in checklist tool todo_write: the model breaks a multi-step task into a checklist and updates its progress as it goes.
//
// Each call passes the complete list, replacing the previous one. The tool keeps no state: the current list is the result of the
// latest successful call in the session history, and the normalized list is in the result's Details for the UI.
// For different behavior, leave it out and register your own tool instead.
package todo

import (
	"fmt"
	"strings"

	"github.com/coldcoder126/rein/tool"
)

// ToolName is the name of the checklist tool.
const ToolName = "todo_write"

// Status is the status of a checklist item.
type Status string

const (
	Pending    Status = "pending"
	InProgress Status = "in_progress"
	Completed  Status = "completed"
)

// Item is one item of the checklist.
type Item struct {
	Content string `json:"content" jsonschema:"description=A short imperative step, e.g. Run the tests."`
	Status  Status `json:"status" jsonschema:"enum=pending,enum=in_progress,enum=completed"`
}

type params struct {
	Todos []Item `json:"todos" jsonschema:"description=The complete updated list. It replaces the previous one; send an empty list to clear it."`
}

const description = `Create and update a todo list for the current task, so you and the user can track progress.
Send the COMPLETE list on every call; it replaces the previous one.

Use it when the task has 3 or more distinct steps, or the user gives several things to do.
Skip it for a single simple step or a purely conversational reply.

- Write each item as a short imperative, e.g. "Run the tests".
- Keep exactly one item in_progress while working. Mark it completed as soon as it is fully done, then start the next.
- If the plan changes, rewrite the list: add new steps, remove ones that no longer apply.
- Do not repeat the list in your reply; the user already sees it.`

// Tool returns the checklist tool; add it to Agent.Tools to use it. The result's Details is the normalized []Item.
func Tool() tool.Tool {
	return tool.New(ToolName, description, func(_ *tool.Context, in params) (*tool.Result, error) {
		items := normalize(in.Todos)
		result := tool.Text(summary(items))
		result.Details = items
		return result, nil
	})
}

// normalize fixes small problems that can be fixed, saving a round trip: it trims whitespace, drops empty items, and keeps only the first in-progress item.
func normalize(items []Item) []Item {
	out := make([]Item, 0, len(items))
	active := false
	for _, it := range items {
		it.Content = strings.TrimSpace(it.Content)
		if it.Content == "" {
			continue
		}
		if it.Status == InProgress {
			if active {
				it.Status = Pending
			}
			active = true
		}
		out = append(out, it)
	}
	return out
}

// summary keeps the result for the model short, since it stays in the context.
func summary(items []Item) string {
	if len(items) == 0 {
		return "Todo list cleared."
	}
	done, current := 0, ""
	for _, it := range items {
		switch it.Status {
		case Completed:
			done++
		case InProgress:
			current = it.Content
		}
	}
	text := fmt.Sprintf("Todo list updated: %d/%d completed.", done, len(items))
	if current != "" {
		text += " In progress: " + current + "."
	}
	return text
}
