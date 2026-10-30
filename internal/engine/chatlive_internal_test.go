package engine

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE CHAT APPLIER A RUNNING NODE BUILDS TELLS THE ATTACHED HUB WHAT IT WROTE.
//
// The chat applier says what each committed batch wrote; the hub pushes it to
// the sockets each room admits. Between them is exactly this package — the
// register that builds the applier, the hook it is handed and the setter the
// hub is attached by — and a nil observer handed to the applier there compiles,
// runs and passes every chat case, while every open room on every node goes
// quiet. So the applier here is the REGISTER's, built with the hooks a running
// node hands it, and the hub is attached AFTER the applier exists, which is the
// order a running node attaches it in.
//
// Mutation: hand the register's applier a nil observer, or drop chatApplied
// from applyHooks, and the hub hears nothing.
func TestTheChatApplierTheRegisterBuildsFeedsTheAttachedHub(t *testing.T) {
	t.Parallel()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })

	e := &Engine{}
	entry, ok := registrationFor(chatlet.Domain{}.Name())
	if !ok {
		t.Fatal("the register has no chat domain")
	}
	applier, err := entry.NewApplier(&stateLog{nodeID: "node-a", applyHooks: e.applyHooks()})
	if err != nil {
		t.Fatalf("build the applier: %v", err)
	}
	hub := newHeardLive()
	e.SetChatLive(t.Context(), hub)

	at := time.Unix(1_700_000_000, 0).UTC()
	create, err := json.Marshal(chatlet.ChannelCreate{
		V: chatlet.DocumentVersion, ChannelID: "c-1", Kind: chatlet.KindPublic,
		Name: "launch", CreatedBy: "ada", CreatedByKind: chatlet.AuthorHuman,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: "op-create", Op: chatlet.OpCreate,
			Subject: chatlet.ChannelNameSubject("launch"), CreatedAt: at, Gen: 1,
			Writer: "node-a", Scope: chatlet.ScopeSet{Terms: []chatlet.ScopeTerm{
				{Kind: chatlet.TermName, ID: chatlet.ChannelToken("launch")},
				{Kind: chatlet.TermChannel, ID: "c-1"},
			}},
		},
		Mutation: create, Actor: "ada", ActorKind: chatlet.AuthorHuman,
	}
	body, err := chatlet.Encode(rec)
	if err != nil {
		t.Fatal(err)
	}
	record := statelog.Record{
		Envelope: statelog.Envelope{
			V: rec.V, Kind: string(rec.Subject.Kind),
			Subject: statelog.Subject{Kind: string(rec.Subject.Kind), ID: rec.Subject.ID},
			Op:      string(rec.Op), OpID: rec.OpID, Gen: rec.Gen, Writer: rec.Writer,
			Scope: rec.Scope.Resolve(rec.Subject),
		},
		Position: statelog.Position{Stream: topics.ChatLogStream, Generation: 1, Seq: 1},
		Payload:  body, StoredAt: at,
	}
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := applier.Apply(t.Context(), tx, record, statelog.ApplyOptions{
			Now: at, StoredAt: at, MaxVariables: db.Caps().MaxVariables,
		})
		return err
	}); err != nil {
		t.Fatalf("apply the create: %v", err)
	}
	applier.Committed(t.Context())

	got := hub.snapshot()
	if len(got.chat) != 1 || got.chat[0].ChannelID != "c-1" || got.chat[0].Op != chatlet.OpCreate ||
		got.chat[0].Position != record.Position {
		t.Fatalf("the hub heard %+v, want the create of c-1 at %v", got.chat, record.Position)
	}
}
