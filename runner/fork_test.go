package runner

import (
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat/chattest"
	"github.com/coldcoder126/rein/session"
)

// Editing a message: fork before it, then run the fork with the new input. The context stored with the old input goes with it.
func TestForkThenStartEditsAMessage(t *testing.T) {
	client := newClient(chattest.Say("答一"), chattest.Say("答二"), chattest.Say("答二改"))
	r, store := newRunner(t, client)
	if _, err := runSync(r, bg, "s", Text("问一")); err != nil {
		t.Fatal(err)
	}
	if _, err := runSync(r, bg, "s", Text("问二"), WithContext(ContextInput{Text: "页面：设置"})); err != nil {
		t.Fatal(err)
	}
	all := saved(t, store, "s")
	if got := sessionRoles(all); got != "user,assistant,context,user,assistant" {
		t.Fatalf("unexpected history: %s", got)
	}
	edited := all[3]
	if edited.InputStartID == "" || edited.InputStartID != all[2].ID || all[0].InputStartID != "" {
		t.Fatalf("an input stored with context should point at its first message: %+v", all)
	}

	if err := session.Fork(bg, r.Store(), "s", session.Session{ID: "f"}, &edited); err != nil {
		t.Fatal(err)
	}
	if _, err := runSync(r, bg, "f", Text("问二改")); err != nil {
		t.Fatal(err)
	}
	req := client.Requests()[2].Messages
	if len(req) != 3 || req[0].TextContent() != "问一" || req[1].TextContent() != "答一" || req[2].TextContent() != "问二改" {
		t.Fatalf("the fork should continue from before the edited input, without its context: %+v", req)
	}
	if got := sessionRoles(saved(t, store, "s")); got != "user,assistant,context,user,assistant" {
		t.Fatalf("the source should be unchanged: %s", got)
	}
	for _, msg := range req {
		if strings.Contains(msg.TextContent(), "页面") {
			t.Fatalf("the context of the edited input should be left out: %+v", req)
		}
	}
}
