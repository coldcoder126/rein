package session_test

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coldcoder126/rein/chat"
	"github.com/coldcoder126/rein/session"
	"github.com/coldcoder126/rein/session/storetest"
)

func TestFileStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store {
		store, err := session.NewFileStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return store
	})
}

func openFileStore(t *testing.T, dir string) *session.FileStore {
	t.Helper()
	store, err := session.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func text(id, s string) session.Message {
	msg := session.UserMessage([]chat.ContentBlock{chat.Text(s)}, nil)
	msg.ID = id
	return msg
}

func listIDs(t *testing.T, store session.Store, sessionID string) string {
	t.Helper()
	return ids(listMessages(t, store, sessionID))
}

func messageFile(dir, sessionID string) string {
	return filepath.Join(dir, hex.EncodeToString([]byte(sessionID))+".jsonl")
}

// A crash can leave a partial last line: it is ignored on reading, and the next append cuts it off instead of joining it.
func TestFileStoreRecoversFromAPartialLine(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	store := openFileStore(t, dir)
	if err := store.EnsureSession(ctx, session.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(ctx, "s", text("m1", "one"), text("m2", "two")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(messageFile(dir, "s"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"m3","type":"us`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	store = openFileStore(t, dir)
	if got := listIDs(t, store, "s"); got != "m1,m2" {
		t.Fatalf("the partial line should be ignored: %s", got)
	}
	if err := store.AppendMessages(ctx, "s", text("m4", "four")); err != nil {
		t.Fatal(err)
	}
	if got := listIDs(t, store, "s"); got != "m1,m2,m4" {
		t.Fatalf("appending should continue after the last complete line: %s", got)
	}
}

func TestFileStoreRejectsACorruptLine(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	store := openFileStore(t, dir)
	if err := store.EnsureSession(ctx, session.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(messageFile(dir, "s"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListMessages(ctx, "s"); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("a corrupt complete line should be an error naming the line: %v", err)
	}
}

func TestFileStoreSessionIDs(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	store := openFileStore(t, dir)
	for _, id := range []string{"", strings.Repeat("x", 121)} {
		if err := store.EnsureSession(ctx, session.Session{ID: id}); err == nil {
			t.Fatalf("a session ID of %d bytes should be rejected", len(id))
		}
	}
	// IDs differing only in case, or holding path characters, are separate sessions on any file system.
	ids := []string{"A", "a", "../x", "x/y", "会话"}
	for _, id := range ids {
		if err := store.EnsureSession(ctx, session.Session{ID: id}); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessages(ctx, id, text("m-"+id, id)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2*len(ids) {
		t.Fatalf("every session should have its own two files in the directory: %d %v", len(entries), err)
	}
	reopened := openFileStore(t, dir)
	for _, id := range ids {
		if got := listIDs(t, reopened, id); got != "m-"+id {
			t.Fatalf("session %q should keep its own messages after reopening: %s", id, got)
		}
		if s, err := reopened.GetSession(ctx, id); err != nil || s.ID != id || s.CreatedAt.IsZero() {
			t.Fatalf("session %q metadata: %+v %v", id, s, err)
		}
	}
}

// A line holding inline media can be far longer than bufio.Scanner's 64 KiB limit.
func TestFileStoreLongLines(t *testing.T) {
	ctx := context.Background()
	store := openFileStore(t, t.TempDir())
	if err := store.EnsureSession(ctx, session.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	msg := session.UserMessage([]chat.ContentBlock{{Type: chat.ContentTypeMedia, MIMEType: "image/png", Data: make([]byte, 1<<20)}}, nil)
	msg.ID = "big"
	if err := store.AppendMessages(ctx, "s", msg); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListMessages(ctx, "s")
	if err != nil || len(got) != 1 || len(got[0].Content()[0].Data) != 1<<20 {
		t.Fatalf("a long line should read back whole: %v", err)
	}
}

// A fork that crashed after writing its messages but before its metadata leaves a message file without a session;
// the session created later under that ID must not take those messages.
func TestFileStoreDropsAMessageFileWithoutMetadata(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	store := openFileStore(t, dir)
	if err := os.WriteFile(messageFile(dir, "s"), []byte(`{"id":"stale","type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListMessages(ctx, "s"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("messages without metadata are not a session: %v", err)
	}
	if err := store.EnsureSession(ctx, session.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	if got := listIDs(t, store, "s"); got != "" {
		t.Fatalf("a new session should not take a leftover message file: %s", got)
	}
}

func TestFileStoreForkSurvivesReopening(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	store := openFileStore(t, dir)
	if err := store.EnsureSession(ctx, session.Session{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessages(ctx, "s", text("m1", "one"), text("m2", "two")); err != nil {
		t.Fatal(err)
	}
	two := text("m2", "two")
	if err := session.Fork(ctx, store, "s", session.Session{ID: "f"}, &two); err != nil {
		t.Fatal(err)
	}
	reopened := openFileStore(t, dir)
	if got := listIDs(t, reopened, "f"); got != "m1" {
		t.Fatalf("the fork should read back after reopening: %s", got)
	}
	if s, err := reopened.GetSession(ctx, "f"); err != nil || s.ForkOf != "s" || s.ForkAt != "m2" {
		t.Fatalf("the fork's metadata should read back: %+v %v", s, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("a fork should leave no temporary file: %s", e.Name())
		}
	}
}
