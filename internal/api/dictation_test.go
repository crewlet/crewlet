package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/dictation"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A DICTATION IS ENDED BY WHAT ENDS ITS CREDENTIAL, as an open tab is: the
// relay acts as the person for as long as they are signed in, and no longer.
// Through the whole app, so the route, the guard and the identity feed are
// the engine's own: two people dictate, both sessions end in the rows, and
// the move naming one closes that dictation 4401 while the other's stays
// open. Mutation: serve /ws/dictation without KeepDecided, and jane's
// dictation stays open.
func TestAnIdentityMoveClosesTheDictationItEnded(t *testing.T) {
	t.Parallel()
	node := newSocketNode(t, func() time.Time { return clock })
	jane := node.tabOn(t, "jane.doe", clock.Add(8*time.Hour), dictation.Path)
	omar := node.tabOn(t, "omar.haddad", clock.Add(8*time.Hour), dictation.Path)
	for _, tab := range []signedInTab{jane, omar} {
		startDictation(t, tab.conn)
	}
	node.rows.end(jane.person)
	node.rows.end(omar.person)

	node.feed.fire(t, iamdomain.Moved{Sessions: []string{jane.lineage}})
	if got := closeOf(t, jane.conn); got != stream.CloseUnauthenticated {
		t.Fatalf("the dictation whose session ended closed %d, want %d", got,
			stream.CloseUnauthenticated)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := omar.conn.Write(ctx, websocket.MessageText, []byte(`{"type":"cancel"}`)); err != nil {
		t.Fatalf("the other person's dictation was closed too: %v", err)
	}
	if got := readDictation(t, omar.conn); got["type"] != "done" {
		t.Errorf("the other person's dictation answered %v, want done", got)
	}
}

// startDictation sends a start and reads the recognizer's `ready`.
func startDictation(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := `{"type":"start","lang":"en","rate":16000,"encoding":"pcm16"}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(start)); err != nil {
		t.Fatal(err)
	}
	if got := readDictation(t, conn); got["type"] != "ready" {
		t.Fatalf("the dictation answered %v, want ready", got)
	}
}

func readDictation(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
