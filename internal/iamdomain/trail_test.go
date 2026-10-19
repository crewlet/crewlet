package iamdomain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A GESTURE MADE THROUGH A TOKEN IS RECORDED AS ONE ON THE TRAIL.
//
// `GET /iam/audit` reads iam_history, and the trail named its actor alone. A
// machine token acts as its owner, so its gesture's actor is the owner — and
// what somebody's token did to the directory read there as done by them. The
// record names the credential, and the row carries it beside the actor. The
// gesture a token may still make about the directory is the one that spares
// its own owner — signing them out everywhere, which a leaked token must be
// able to do to itself — so that is the token's row here; an administrator's
// suspension and removal, which no token may write (ADR-0033), name the
// browser session they came through instead, and the token's own attempt at a
// suspension writes no row at all. A writer that acted through nothing — the
// node's own, which writes every sign-in and every duty — names none.
// Mutation: drop the column from the apply, or the field from the record, and
// a row goes red; drop the record-layer lock and the refused row appears.
func TestAGestureMadeThroughATokenIsRecordedAsOneOnTheTrail(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	id := uuid.Must(uuid.NewV7()).String()
	enrolSarah(t, rig, id)

	const token = "pat:0192f00d-0000-7000-8000-00000000000a"
	sarah := principalNamed("sarah.chen", iam.KindPerson,
		[]iam.Grant{iam.GrantWorklet, iam.GrantDoclet})
	sarah.ID = uuid.MustParse(id)
	sarah.Via = token
	herToken := rig.writer.As(sarah)

	const session = "session:0192f00d-0000-7000-8000-00000000000b"
	admin := principalNamed("ana.admin", iam.KindPerson,
		[]iam.Grant{iam.GrantAdmin})
	admin.Via = session
	byBrowser := rig.writer.As(admin)
	admin.Via = "pat:0192f00d-0000-7000-8000-00000000000c"
	adminToken := rig.writer.As(admin)

	for _, step := range []struct {
		reason  string
		gesture func() error
	}{
		{"my laptop was stolen", func() error {
			_, err := herToken.Revoke(t.Context(), id, "op-revoke",
				"my laptop was stolen")
			return err
		}},
		{"left the building", func() error {
			_, err := byBrowser.SetStage(t.Context(), id, iam.StageSuspended,
				"op-suspend", "left the building")
			return err
		}},
		{"came back", func() error {
			_, err := rig.writer.SetStage(t.Context(), id, iam.StageActive,
				"op-restore", "came back")
			return err
		}},
		{"left for good", func() error {
			_, err := byBrowser.Remove(t.Context(), id, "op-remove", "left for good")
			return err
		}},
	} {
		if err := step.gesture(); err != nil {
			t.Fatalf("%s: %v", step.reason, err)
		}
		rig.drain()
	}
	if _, err := adminToken.SetStage(t.Context(), id, iam.StageSuspended,
		"op-suspend-by-token", "suspended by a pipeline"); !errors.Is(err,
		iamdomain.ErrTokenRefused) {
		t.Errorf("a suspension through a token answered %v, want ErrTokenRefused", err)
	}
	rig.drain()

	page, err := reader.History(t.Context(), iamdomain.HistoryQuery{Person: id})
	if err != nil {
		t.Fatalf("read the trail: %v", err)
	}
	rows := map[string]iamdomain.HistoryRow{}
	for _, row := range page.Entries {
		rows[row.Reason] = row
	}
	if row, ok := rows["suspended by a pipeline"]; ok {
		t.Errorf("a suspension refused through a token is on the trail: %+v", row)
	}
	for reason, want := range map[string]struct{ actor, via string }{
		"my laptop was stolen": {"sarah.chen", token},
		"left the building":    {"ana.admin", session},
		"came back":            {"ana.admin", ""},
		"left for good":        {"ana.admin", session},
	} {
		row, ok := rows[reason]
		if !ok {
			t.Errorf("the trail holds no row for %q: %+v", reason, page.Entries)
			continue
		}
		if row.Actor != want.actor || row.OperatorID != want.via {
			t.Errorf("%q is on the trail as %q through %q, want %q through %q",
				reason, row.Actor, row.OperatorID, want.actor, want.via)
		}
	}
}

// AN ENTRY NAMES THE LOGIN OF WHOEVER IT IS ABOUT, WHILE THEY ARE ENROLLED.
//
// The trail names its subject by id, and a page of id prefixes said whose
// session opened and whose row changed to nobody reading it. The login their
// row holds rides beside it; a removal erases what identified them, so their
// entries name nobody once they are gone — the CONTROL. Mutation: drop the
// join and the suspension's entry names nobody.
func TestATrailEntryNamesTheLoginOfWhoeverItIsAbout(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	id := uuid.Must(uuid.NewV7()).String()
	enrolSarah(t, rig, id)
	if _, err := rig.writer.SetStage(t.Context(), id, iam.StageSuspended,
		"op-suspend", "a leave"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	rig.drain()
	logins := func() map[string]string {
		t.Helper()
		page, err := reader.History(t.Context(), iamdomain.HistoryQuery{Person: id})
		if err != nil {
			t.Fatalf("read the trail: %v", err)
		}
		out := map[string]string{}
		for _, row := range page.Entries {
			out[row.Reason] = row.Login
		}
		return out
	}
	if got := logins()["a leave"]; got != "sarah.chen" {
		t.Errorf("the suspension's entry names %q, want sarah.chen", got)
	}
	if _, err := rig.writer.Remove(t.Context(), id, "op-remove", "gone"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rig.drain()
	if got := logins()["a leave"]; got != "" {
		t.Errorf("after the removal the suspension's entry names %q, want nobody", got)
	}
}

// A GESTURE A PERSON MAKES THROUGH THE NODE IS THEIRS ON THE TRAIL.
//
// The node's own writer writes every sign-in, sign-out, second factor's spend,
// redemption and reset link, because the person holds no grant the record asks
// of its party — and the trail named the node as WHO for every one of them.
// Written [iamdomain.Writer.For] the person, the row names them and the session
// it came through, while what the writer may do is still the node's: a person
// holding no grant suspends nobody, and the node writing for them still may.
// The CONTROL is the node writing for itself, named as the node. Mutation:
// leave the author unchanged in For and the person's row names the node; carry
// the person's grants and the stage change is refused.
func TestAGestureAPersonMakesThroughTheNodeIsTheirsOnTheTrail(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	id := uuid.Must(uuid.NewV7()).String()
	enrolSarah(t, rig, id)
	node := rig.writer.As(principalNamed("node-0", iamdomain.NodeKind,
		[]iam.Grant{iam.GrantAdmin}))

	sarah := principalNamed("sarah.chen", iam.KindPerson, nil)
	sarah.ID = uuid.MustParse(id)
	expires := brokerAt.Add(time.Hour)
	theirs := uuid.Must(uuid.NewV7()).String()
	sarah.Via = iam.SessionName(theirs)
	nodes := uuid.Must(uuid.NewV7()).String()
	for _, open := range []struct {
		by      *iamdomain.Writer
		lineage string
	}{{node.For(sarah), theirs}, {node, nodes}} {
		if _, err := open.by.OpenSession(t.Context(), iamdomain.SessionStart{
			Lineage: open.lineage, Person: id, AbsoluteExpiresAt: expires,
			OpID: "session:" + open.lineage,
		}); err != nil {
			t.Fatalf("open a session: %v", err)
		}
	}
	if _, err := node.For(sarah).SetStage(t.Context(), id, iam.StageSuspended,
		"op-suspend", "a leave"); err != nil {
		t.Errorf("the node writing for a person holding no grant was refused a "+
			"stage change it may make: %v", err)
	}
	rig.drain()

	page, err := reader.History(t.Context(), iamdomain.HistoryQuery{Person: id})
	if err != nil {
		t.Fatalf("read the trail: %v", err)
	}
	type author struct {
		actor    string
		kind     iam.Kind
		operator string
	}
	got := map[string]author{}
	for _, row := range page.Entries {
		got[row.ObjectID] = author{row.Actor, row.ActorKind, row.OperatorID}
	}
	for lineage, want := range map[string]author{
		theirs: {"sarah.chen", iam.KindPerson, iam.SessionName(theirs)},
		nodes:  {"node-0", iamdomain.NodeKind, "node-0"},
	} {
		if got[lineage] != want {
			t.Errorf("the session %s opened on the trail as %+v, want %+v",
				lineage, got[lineage], want)
		}
	}
}
