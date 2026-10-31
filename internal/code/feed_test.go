package code_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CODE FEED (feed.go, wake.go): a seat hears about a change by a wake the
// code log's own consumer derives from the committed record, and a person
// never does.

// feedRecord is the record at p on n's log as the feed is handed it: the
// verified body, keyed by its subject, at its position.
func feedRecord(t *testing.T, n *node, p statelog.Position) changefeed.Record {
	t.Helper()
	subject, body, _, ok, err := n.records.At(t.Context(), p.Seq)
	if err != nil || !ok {
		t.Fatalf("read the record at %s: %v (held: %v)", p, err, ok)
	}
	return changefeed.Record{Key: subject, Payload: body, Position: p.Seq, Stream: p.Stream,
		Gen: uint64(p.Generation)}
}

// wakesOf is what the feed and the parser make of rec: whether the
// translator relays it, and each wake the parser derives as
// "handle:reason".
func wakesOf(t *testing.T, rec changefeed.Record) (changefeed.Delivery, bool, []notify.Routed) {
	t.Helper()
	d, relayed, err := code.NewTranslator().Translate(t.Context(), rec)
	if err != nil {
		t.Fatalf("translate the record at %d: %v", rec.Position, err)
	}
	if !relayed {
		return d, false, nil
	}
	at := statelog.Position{Stream: rec.Stream, Generation: uint32(rec.Gen), Seq: rec.Position}
	routed, err := code.NewParser(code.ParserOptions{}).Parse(t.Context(),
		types.RawWebhook{Body: d.Body, Trigger: at.String()}, nil)
	if err != nil {
		t.Fatalf("parse the record at %d: %v", rec.Position, err)
	}
	return d, true, routed
}

// woken is each wake as "handle:reason".
func woken(routed []notify.Routed) []string {
	var out []string
	for _, r := range routed {
		out = append(out, r.To.Handle+":"+r.Metadata[code.MetaVia])
	}
	return out
}

// THE CODE FEED DERIVES ONLY WAKES, AND ONLY SEATS': a record about a change
// is relayed exactly when its routing tells a seat something, and the parser
// wakes exactly those seats — each once, under the reason the record's routing
// gives it, with an id derived from the record and the seat, the change's
// name, and who wrote what it announces. A person the same record tells is
// never queue-woken: their notice row is written all the same, and is where
// they read it. A record that tells nobody but its actor, or only persons, is
// acknowledged with no delivery, as is a push that moves no change and a
// record of a newer build.
//
// Mutation: drop the person filter in the parser → the open wakes founder;
// relay a record whatever its routing tells (drop wakesASeat) → the ask of a
// person alone, and the aside nobody hears, are relayed; derive the wake id
// from the change rather than the record → the comment's wakes collapse into
// the ask's.
func TestTheCodeFeedDerivesOnlyWakes(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	// AN HOUR OLD at the harness's batch instant, inside every notice
	// horizon, so the person's notice row is written and can be read.
	storedBefore(n, time.Hour)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder", "cto"}
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	change := opened.ChangeID
	w, _ := writerOn(t, n, n.pub)

	if _, _, routed := wakesOf(t, feedRecord(t, n, opened.Position)); !slices.Equal(woken(routed),
		[]string{"cto:mention"}) {
		t.Errorf("an open naming a person and a seat wakes %v, want the seat alone", woken(routed))
	}
	if got := noticesOf(t, n, opened.Position); !slices.Equal(got,
		[]string{"cto:mention:agent", "founder:mention:person"}) {
		t.Errorf("the open wrote the notices %v, want one each, the person's read in their inbox", got)
	}
	aside := converse(t, w, saying(repo, change, dev, "", "Noted for later."))
	if _, relayed, _ := wakesOf(t, feedRecord(t, n, aside.Position)); relayed {
		t.Error("a comment that starts a thread nobody else is in is relayed")
	}

	askSeat := converse(t, w, asking(repo, change, dev, "plat-lead"))
	d, relayed, routed := wakesOf(t, feedRecord(t, n, askSeat.Position))
	if !relayed || !slices.Equal(woken(routed), []string{"plat-lead:review_requested"}) {
		t.Fatalf("an ask of plat-lead wakes %v (relayed %v), want plat-lead asked to review", woken(routed), relayed)
	}
	rec := n.recordAt(askSeat.Position)
	if d.ID != rec.OpID || d.Actor != "dev" {
		t.Errorf("the delivery is %q by %q, want the record's own op id %q by dev", d.ID, d.Actor, rec.OpID)
	}
	wake := routed[0]
	switch {
	case wake.WakeID != changefeed.WakeID(rec.OpID, "plat-lead"):
		t.Errorf("the wake's id is %s, want the one derived from the record and the seat", wake.WakeID)
	case !wake.WakeAt.Equal(rec.CreatedAt):
		t.Errorf("the wake is stamped %v, want the record's own instant %v", wake.WakeAt, rec.CreatedAt)
	case wake.Source != code.Source || wake.Metadata[code.MetaChangeRef] != "web!1" ||
		wake.Metadata[code.MetaChangeID] != change || wake.Body != "the retry loop":
		t.Errorf("the wake is %+v, want web!1 by its id on the code source, carrying the ask's note", wake.Inbound)
	case wake.Writer == nil || wake.Writer.Kind != iam.ActorAgent || wake.Writer.Seat != "dev":
		t.Errorf("the wake says %+v wrote it, want the seat dev", wake.Writer)
	}

	askPerson := converse(t, w, asking(repo, change, dev, "founder"))
	if _, relayed, routed := wakesOf(t, feedRecord(t, n, askPerson.Position)); relayed || len(routed) != 0 {
		t.Errorf("an ask of a person alone is relayed (%v) and wakes %v, want no delivery", relayed, woken(routed))
	}
	if got := noticesOf(t, n, askPerson.Position); !slices.Equal(got, []string{"founder:review_requested:person"}) {
		t.Fatalf("the ask of a person wrote the notices %v, want the person's own, which their inbox reads", got)
	}

	say := saying(repo, change, ada, "", "Is the retry bounded, @plat-lead and @cto?")
	say.Comment.Mentions = []string{"plat-lead", "cto"}
	said := converse(t, w, say)
	_, _, routed = wakesOf(t, feedRecord(t, n, said.Position))
	if got := woken(routed); !slices.Equal(got, []string{"plat-lead:mention", "cto:mention"}) {
		t.Fatalf("a person's comment naming two seats wakes %v, want both mentioned", got)
	}
	for _, r := range routed {
		if r.Writer == nil || r.Writer.Kind != iam.ActorHuman || r.Writer.Seat != "founder" {
			t.Errorf("%s's wake says %+v wrote it, want the person, as the seat founder they hold",
				r.To.Handle, r.Writer)
		}
		if r.WakeID == wake.WakeID {
			t.Errorf("%s's wake of the comment has the ask's id, so it would collapse into it", r.To.Handle)
		}
	}

	reviewed := converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil,
		raising(code.SeverityBlocker, 7, "the retry races")))
	_, _, routed = wakesOf(t, feedRecord(t, n, reviewed.Position))
	if got := woken(routed); !slices.Equal(got, []string{"dev:changes_requested"}) {
		t.Fatalf("a review asking for changes wakes %v, want its author", got)
	}
	answered := converse(t, w, answering(repo, change, dev, code.FindingDisputed, reviewed.FindingIDs...))
	_, _, routed = wakesOf(t, feedRecord(t, n, answered.Position))
	if got := woken(routed); !slices.Equal(got, []string{"plat-lead:finding_replied"}) {
		t.Fatalf("an answer to plat-lead's finding wakes %v, want its raiser", got)
	}

	moved, err := c.Propose(t.Context(), proposal(repo, update(releaseRef, oidOf("R"), oidOf("R2"))))
	if err != nil || moved.Position.IsZero() {
		t.Fatalf("a push to a branch no change uses answered %+v, %v", moved, err)
	}
	if _, relayed, _ := wakesOf(t, feedRecord(t, n, moved.Position)); relayed {
		t.Error("a push that moves no change is relayed")
	}

	// A NEWER BUILD'S RECORD: the envelope reads, the payload's rules are not
	// this build's, and it is handled rather than returned for ever.
	future := feedRecord(t, n, askSeat.Position)
	var body map[string]any
	if err := json.Unmarshal(future.Payload, &body); err != nil {
		t.Fatal(err)
	}
	body["v"] = code.RecordVersion + 1
	if future.Payload, err = json.Marshal(body); err != nil {
		t.Fatal(err)
	}
	if _, relayed, err := code.NewTranslator().Translate(t.Context(), future); relayed || err != nil {
		t.Errorf("a newer build's record is relayed (%v) or refused (%v), want it acknowledged", relayed, err)
	}
	// AND BYTES THAT ARE NO RECORD are an error, which the feed returns
	// rather than acknowledges: a record it cannot read is never decided.
	if _, _, err := code.NewTranslator().Translate(t.Context(),
		changefeed.Record{Key: "x", Payload: []byte("not a record")}); err == nil {
		t.Error("bytes that are no record were acknowledged")
	}
}

// A REPLAY OF THE RECORDS FROM BEFORE CHANGES WAKES NOBODY: the code feed's
// consumer is new, and a new consumer starts from the first record its log
// still holds — every repository created, every push, every setting from
// before a change existed. Not one of them carries a routing, so the replay
// that turning the feed on makes is acknowledged record by record and wakes
// no seat about history.
//
// Mutation: relay every record about a repository (drop the routing test in
// Translate) → the pushes are relayed.
func TestAReplayOfPreChangeRecordsWakesNobody(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	repoOn(t, n, "api")
	c, _ := committerOn(t, n, n.pub)
	head := oidOf("A")
	for _, next := range []string{"B", "C", "D"} {
		res, err := c.Propose(t.Context(), proposal(repo, update(mainRef, head, oidOf(next))))
		if err != nil || res.Position.IsZero() {
			t.Fatalf("the push to %s answered %+v, %v", next, res, err)
		}
		head = oidOf(next)
	}
	first, last, err := n.log.Bounds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if last < first+5 {
		t.Fatalf("the log holds %d..%d, want the two repositories' creates and pushes", first, last)
	}
	stream := code.Domain{}.Stream().Name
	for seq := first; seq <= last; seq++ {
		rec := feedRecord(t, n, statelog.Position{Stream: stream, Seq: seq})
		if d, relayed, err := code.NewTranslator().Translate(t.Context(), rec); relayed || err != nil {
			t.Errorf("the record at %d (%s) is relayed as %+v (%v), want it acknowledged", seq, rec.Key, d, err)
		}
	}
}

// THE PROMPT IS THE CHANGE'S: a code wake keys its conversation on the
// change's id, never its name, which a repository's rename moves; it is
// always a pointer at the change; and it asks for an answer exactly under the
// reasons that ask something.
func TestACodeWakeIsAboutItsChange(t *testing.T) {
	t.Parallel()
	p := code.Prompt{}
	before := map[string]string{code.MetaChangeID: "c-1", code.MetaChangeRef: "web!42"}
	after := map[string]string{code.MetaChangeID: "c-1", code.MetaChangeRef: "site!42"}
	for _, meta := range []map[string]string{before, after} {
		if got := p.ConversationIdentity(meta, ""); got != "change:c-1" {
			t.Errorf("the conversation is %q, want the change by its id", got)
		}
		if got := p.PartitionKey(meta, ""); got != "change:c-1" {
			t.Errorf("the partition is %q, want the change by its id", got)
		}
	}
	if p.ConversationIdentity(map[string]string{code.MetaChangeRef: "web!42"}, "") != "" {
		t.Error("a wake naming no change keys a conversation")
	}
	for _, r := range code.Reasons() {
		n := notify.Inbound{Source: code.Source, Subject: "web!42: Retry the upload",
			Metadata: map[string]string{code.MetaVia: string(r), code.MetaChangeRef: "web!42"}}
		if p.Addressed(n) != r.Addressed() {
			t.Errorf("a wake under %s is addressed %v, want %v", r, p.Addressed(n), r.Addressed())
		}
		if !p.RequiresRecon(n) {
			t.Errorf("a wake under %s is not a pointer at the change", r)
		}
		if text := p.Build(n, nil); !strings.Contains(text, "web!42") ||
			!strings.Contains(text, "`code_get_change`") {
			t.Errorf("a wake under %s does not send its seat to the change:\n%s", r, text)
		}
		if p.WakesActor(string(r)) {
			t.Errorf("%s wakes whoever made the change", r)
		}
	}
}
