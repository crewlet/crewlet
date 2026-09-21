package pages_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE WAKE PATH, END TO END: a committed record becomes a notification for the
// people who asked to hear about it.
//
// The feed and the parser are two halves of one contract and neither can be
// checked alone: the feed decides WHAT to relay and puts a record on the
// inbound topic, and the parser decides WHO it is for by reading that same
// record. A test that built the parser's input by hand would have agreed with
// itself while the two halves spoke different shapes — which is precisely what
// they did. The feed relayed the [pages.MutationRecord] it encodes, the parser
// decoded that body as a change document (the shape the coordination family
// this domain replaced put on the wire), and the two share a version field and
// almost no key: every delivery decoded cleanly into a change with no page id,
// and every page notification in the company was dropped one line later, with
// no error anywhere.
func TestACommittedRecordWakesThePeopleWatchingThePage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{
		Title: "Runbook", Body: "prose", Watchers: []string{"carla"},
	})
	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("the deploy steps changed")}); err != nil {
		t.Fatalf("save: %v", err)
	}

	routed := r.route(r.lastRecord(), pages.Leads{"ENG": "lead"})
	if len(routed) != 1 {
		t.Fatalf("a save on a page carla watches woke %d seats: %+v",
			len(routed), routed)
	}
	got := routed[0]
	if got.To.Handle != "carla" {
		t.Errorf("the wake went to %q, want carla", got.To.Handle)
	}
	if got.Inbound.Metadata[pages.MetaPageID] != page.Page.ID {
		t.Errorf("the wake names page %q, want %s — a recipient with no page "+
			"id has nothing to open", got.Inbound.Metadata[pages.MetaPageID],
			page.Page.ID)
	}
	if got.Inbound.Metadata[pages.MetaChangeKind] != string(pages.ChangeSaved) {
		t.Errorf("the wake says %q happened",
			got.Inbound.Metadata[pages.MetaChangeKind])
	}
	if got.Inbound.Metadata[pages.MetaVersion] != "2" {
		t.Errorf("the wake reports version %q, want 2 — its reader passes that "+
			"number back as `base_version`",
			got.Inbound.Metadata[pages.MetaVersion])
	}
	if got.Inbound.Metadata[pages.MetaTitle] != "Runbook" ||
		got.Inbound.Metadata[pages.MetaContainer] != "ENG" {
		t.Errorf("the wake's address is %q/%q",
			got.Inbound.Metadata[pages.MetaContainer],
			got.Inbound.Metadata[pages.MetaTitle])
	}
	if got.WakeID == uuid.Nil {
		t.Error("the wake carries no id, so a redelivery is not recognisable " +
			"as one")
	}
	// AND THE AUTHOR IS NOT WOKEN BY HER OWN WRITE.
	for _, c := range routed {
		if c.To.Handle == "jane" {
			t.Error("jane was woken about her own save")
		}
	}
}

// A MENTION REACHES SOMEBODY WHO DOES NOT WATCH THE PAGE, and a CREATE with
// nobody named falls back to the container's lead.
//
// The two are the parser's other arms, and they read the same relayed record:
// one a set the writer computed (the mentions), one a decision about which
// kinds are worth a lead's attention at all.
func TestAMentionAndTheLeadFallbackBothRouteOffTheRelayedRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})

	// A CREATE nobody was named in reaches the container's lead — a page
	// appearing in a team's space is a fact a lead has a reason to know.
	created := r.route(r.recordAt(1), pages.Leads{"ENG": "lead"})
	if len(created) != 1 || created[0].To.Handle != "lead" {
		t.Fatalf("a create nobody watches woke %+v, want the container's lead",
			created)
	}
	if created[0].Inbound.Metadata[pages.RoutedViaField] != pages.ViaLeadFallback {
		t.Errorf("the lead's copy is routed via %q",
			created[0].Inbound.Metadata[pages.RoutedViaField])
	}

	if _, _, err := r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		pages.NewComment{Body: "@bob is this current?", Mentions: []string{"bob"}}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	routed := r.route(r.lastRecord(), pages.Leads{"ENG": "lead"})
	handles := make([]string, 0, len(routed))
	for _, c := range routed {
		handles = append(handles, c.To.Handle)
	}
	if !slices.Contains(handles, "bob") {
		t.Fatalf("a mention woke %v, and bob is not among them — a directed "+
			"ask that reaches nobody is the one wake that cannot be missed",
			handles)
	}
	for _, c := range routed {
		if c.To.Handle != "bob" {
			continue
		}
		if c.Inbound.Metadata[pages.RoutedViaField] != pages.ViaMention {
			t.Errorf("bob's copy is routed via %q, want a mention",
				c.Inbound.Metadata[pages.RoutedViaField])
		}
		if !pages.Addressed(c.Inbound.Metadata) {
			t.Error("a mention is not marked as an ask, so nothing is waiting " +
				"for bob to answer")
		}
	}
}

// lastRecord is the newest record on the log, as the feed would receive it.
func (r *roundTrip) lastRecord() []byte {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	return r.recordAt(last)
}

// recordAt is one record's BODY, frame opened.
//
// The wake feed is handed the body rather than the bytes off the stream: the
// framework verifies before any domain decodes, so what travels on a change
// record is what the domain published. A harness handing on the framed bytes
// would be testing a translator against something no translator is ever given.
func (r *roundTrip) recordAt(seq uint64) []byte {
	r.t.Helper()
	_, payload, _, ok, err := r.log.At(r.t.Context(), seq)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v (present %v)", seq, err, ok)
	}
	body, verdict := r.verifier.Open(payload)
	if verdict != statelog.Verified {
		r.t.Fatalf("record %d did not verify: %s", seq, verdict)
	}
	return body
}

// A LEAD WHO IS NO LONGER A SEAT IS NOT THE ANSWER TO "nobody was named".
//
// The lead map is built from one epoch's org and a parser outlives that epoch,
// so a lead whose seat was deleted or whose handle was renamed is a handle the
// company no longer employs. Routing to one produces a wake that resolves to
// nobody three layers away, in the notification service, where the log line
// names a recipient rather than the container whose page activity reached no
// one — and [pages.Parser] already drops exactly these handles when they are
// mentions or watchers.
func TestTheLeadFallbackDropsAHandleTheCompanyNoLongerEmploys(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "prose"})
	create := r.recordAt(1)

	// THE CONTROL FIRST: the same record, the same lead map, and a roster
	// that still has the lead on it. Without this the case below passes
	// identically if the record simply stopped waking anybody.
	employed := r.routeVia(create, pages.Leads{"ENG": "lead"}, registry(t, "jane", "lead"))
	if len(employed) != 1 || employed[0].To.Handle != "lead" {
		t.Fatalf("control: a create in a container whose lead is on the roster "+
			"woke %+v, want the lead", employed)
	}

	gone := r.routeVia(create, pages.Leads{"ENG": "lead"}, registry(t, "jane"))
	if len(gone) != 0 {
		t.Errorf("a create fell back to %+v, and that handle is not a seat — "+
			"the wake can only be reported as undeliverable somewhere that "+
			"cannot name the container it came from", gone)
	}
}

// registry is the company roster, holding exactly these handles.
func registry(t *testing.T, handles ...string) *notify.Registry {
	t.Helper()
	o := &org.Organization{Name: "nimbus"}
	for _, handle := range handles {
		o.Roles = append(o.Roles, &org.Role{
			Name: strings.ToUpper(handle), DeclaredHandle: handle,
		})
	}
	o.Normalize()
	return notify.NewRegistry(o)
}

// route runs one committed record through BOTH halves — the feed's translator
// and the parser — because the contract under test is that they agree.
//
// A NIL REGISTRY admits every handle, which is the honest answer for a
// deployment with no organization loaded — and keeps these cases about the two
// halves agreeing rather than about who is on the roster.
func (r *roundTrip) route(payload []byte, leads pages.Leads) []notify.Routed {
	r.t.Helper()
	return r.routeVia(payload, leads, nil)
}

// routeVia is the same path against a named roster.
func (r *roundTrip) routeVia(payload []byte, leads pages.Leads,
	reg *notify.Registry) []notify.Routed {

	r.t.Helper()
	delivery, wake, err := pages.NewTranslator(nil).Translate(r.t.Context(),
		changefeed.Record{Payload: payload, Key: "k"})
	if err != nil {
		r.t.Fatalf("translate: %v", err)
	}
	if !wake {
		r.t.Fatal("the feed declined to relay a record that carries a " +
			"notification")
	}
	parser := pages.NewParser(pages.ParserOptions{Leads: leads})
	routed, err := parser.Parse(r.t.Context(), types.RawWebhook{Body: delivery.Body}, reg)
	if err != nil {
		r.t.Fatalf("parse: %v", err)
	}
	return routed
}
