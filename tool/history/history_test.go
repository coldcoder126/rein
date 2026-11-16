package history_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/tool"
	"github.com/coldcoder126/rein/tool/history"
)

func message(id, text string) session.Message {
	msg := session.UserMessage([]chat.ContentBlock{chat.Text(text)}, nil)
	msg.ID = id
	return msg
}

func summary(id, firstKept string) session.Message {
	msg := session.SummaryMessage("summary of "+firstKept, session.Compaction{FirstKeptID: firstKept})
	msg.ID = id
	return msg
}

func newStore(t *testing.T, sessionID string, messages ...session.Message) session.Store {
	t.Helper()
	store := session.NewMemoryStore()
	ctx := context.Background()
	if err := store.EnsureSession(ctx, session.Session{ID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(ctx, sessionID, messages...); err != nil {
		t.Fatal(err)
	}
	return store
}

// call runs the tool in session sessionID and returns its text, or the error text.
func call(t *testing.T, store session.Store, sessionID, args string) (string, error) {
	t.Helper()
	ctx := tool.NewContext(context.Background(), tool.RunInfo{SessionID: sessionID}, chat.ToolCall{}, nil)
	result, err := history.Tool(store).Execute(ctx, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	return result.Content[0].Text, nil
}

func TestArgumentsAndAvailability(t *testing.T) {
	store := newStore(t, "s", message("m1", "hello"))
	for _, args := range []string{`{}`, `{"query":"a","id":"m1"}`, `{"query":"  "}`} {
		if _, err := call(t, store, "s", args); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("%s: should ask for exactly one of query and id: %v", args, err)
		}
	}
	if _, err := call(t, store, "", `{"query":"hello"}`); err == nil {
		t.Fatal("without a session there is no history to read")
	}
	if got, err := call(t, store, "s", `{"query":"hello"}`); err != nil || !strings.Contains(got, "nothing in this conversation has been replaced") {
		t.Fatalf("an uncompacted session has no earlier messages: %q %v", got, err)
	}
}

// After two compactions only the messages the latest one replaced can be found, without the earlier summary.
func TestFindsOnlyWhatCompactionReplaced(t *testing.T) {
	store := newStore(t, "s",
		message("m1", "The invoice number is INV-2041."),
		message("m2", "Deploy to staging first."),
		summary("s1", "m2"),
		message("m3", "The invoice was paid."),
		summary("s2", "m3"),
		message("m4", "Which invoice?"),
	)
	got, err := call(t, store, "s", `{"query":"INVOICE"}`)
	if err != nil || !strings.Contains(got, "[id=m1 ") || !strings.Contains(got, "INV-2041") || strings.Contains(got, "m3") || strings.Contains(got, "m4") {
		t.Fatalf("the search should find m1 only, ignoring case and what is still in context: %q %v", got, err)
	}
	if got, _ := call(t, store, "s", `{"query":"summary of"}`); !strings.Contains(got, "No earlier message contains") {
		t.Fatalf("earlier summaries should not be searched: %q", got)
	}
	if got, err := call(t, store, "s", `{"id":"m2"}`); err != nil || !strings.HasSuffix(got, "Deploy to staging first.") {
		t.Fatalf("reading by id should return the whole message: %q %v", got, err)
	}
	for _, id := range []string{"m3", "s1", "missing"} {
		if _, err := call(t, store, "s", fmt.Sprintf(`{"id":%q}`, id)); err == nil {
			t.Fatalf("%s is not an earlier message and should not be read", id)
		}
	}
	if _, err := call(t, newStore(t, "other", message("x", "invoice")), "s", `{"query":"invoice"}`); err == nil {
		t.Fatal("a session the store does not have should fail")
	}
}

func TestResultsAreBounded(t *testing.T) {
	var messages []session.Message
	for i := range 25 {
		messages = append(messages, message(fmt.Sprintf("m%d", i), "note "+strings.Repeat("字", 300)+" needle "+strings.Repeat("字", 300)))
	}
	messages = append(messages, message("long", strings.Repeat("x", 20_000)), message("last", "now"), summary("s", "last"))
	store := newStore(t, "s", messages...)

	got, err := call(t, store, "s", `{"query":"needle"}`)
	if err != nil || strings.Count(got, "[id=") != 20 || !strings.Contains(got, "5 more messages match") {
		t.Fatalf("a search should return 20 matches and say how many more there are: %v\n%s", err, got)
	}
	if first := strings.Split(got, "\n")[1]; !strings.HasPrefix(first, "…") || !strings.HasSuffix(first, "…") || len([]rune(first)) != 2*100+len("needle")+2 {
		t.Fatalf("a snippet should show the text around the match: %q", first)
	}
	got, err = call(t, store, "s", `{"id":"long"}`)
	if err != nil || !strings.HasSuffix(got, "[truncated]") || len(got) > 8100 {
		t.Fatalf("reading a long message should be truncated: %d %v", len(got), err)
	}
}
