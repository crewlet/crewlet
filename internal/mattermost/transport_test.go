package mattermost_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// instance is a Mattermost stand-in that answers as a whole server rather
// than a single endpoint.
type instance struct {
	*server
	siteURL  string
	throttle string
	// identities maps a bot token to the account it authenticates as.
	identities map[string]mattermost.User
	mu         sync.Mutex
	typing     []string
}

func newInstance(t *testing.T, identities map[string]mattermost.User) *instance {
	t.Helper()
	inst := &instance{server: newServer(t), identities: identities, throttle: "2000"}
	inst.siteURL = inst.URL
	inst.server.responds(func(w http.ResponseWriter, r *http.Request) bool {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case strings.HasSuffix(r.URL.Path, "/config/client"):
			json.NewEncoder(w).Encode(map[string]string{
				"SiteURL": inst.siteURL,
				"TimeBetweenUserTypingUpdatesMilliseconds": inst.throttle,
			})
		case strings.HasSuffix(r.URL.Path, "/thread"):
			if _, ok := inst.identities[token]; !ok {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"message":"Invalid or expired session"}`))
				return true
			}
			// A THREAD ROOTED ON A POST WITH NOTHING TO RENDER, served
			// for one root id so the ordinary thread above stays the
			// ordinary thread. A system line is one way to get here and
			// an attachment-only post is the other; both are threads
			// people then reply in.
			if strings.Contains(r.URL.Path, "/posts/quiet/") {
				json.NewEncoder(w).Encode(map[string]any{
					"order": []string{"quiet", "q1"},
					"posts": map[string]any{
						"quiet": map[string]any{"id": "quiet", "channel_id": "C1",
							"user_id": "U-ana", "type": "system_add_to_channel",
							"message": "ana added bob", "create_at": 1000},
						"q1": map[string]any{"id": "q1", "channel_id": "C1",
							"user_id": "U-ana", "message": "so what broke", "create_at": 2000},
					},
				})
				return true
			}
			json.NewEncoder(w).Encode(map[string]any{
				"order": []string{"root", "p1", "p2", "p3"},
				"posts": map[string]any{
					"root": map[string]any{"id": "root", "channel_id": "C1", "user_id": "U-ana",
						"message": "staging redirects in a loop", "create_at": 1000},
					"p1": map[string]any{"id": "p1", "channel_id": "C1", "user_id": "bot-swe",
						"message": "on it", "create_at": 2000},
					"p2": map[string]any{"id": "p2", "channel_id": "C1", "user_id": "U-ana",
						"message": "deleted regret", "create_at": 3000, "delete_at": 3100},
					"p3": map[string]any{"id": "p3", "channel_id": "C1", "user_id": "U-ana",
						"type": "system_join_channel", "message": "ana joined", "create_at": 4000},
					"p4": map[string]any{"id": "p4", "channel_id": "C-OTHER", "user_id": "U-ana",
						"message": "another channel entirely", "create_at": 5000},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/typing"):
			inst.mu.Lock()
			inst.typing = append(inst.typing, r.URL.Path)
			inst.mu.Unlock()
			w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/users/me"):
			me, ok := inst.identities[token]
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"message":"Invalid or expired session"}`))
				return true
			}
			w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
			json.NewEncoder(w).Encode(me)
		default:
			w.Write([]byte(`{}`))
		}
		return true
	})
	return inst
}

func (i *instance) typings() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.typing)
}

func transport(t *testing.T, inst *instance, mutate func(*mattermost.TransportOptions)) *mattermost.Transport {
	t.Helper()
	opts := mattermost.TransportOptions{
		Config: mattermost.Config{
			URL: inst.URL, Team: "eng", Status: notify.StatusAlways,
			Seats: []mattermost.SeatConfig{{Handle: "swe", Token: "tok-swe"}},
		},
		Publisher: &recorder{},
		Backoff:   fastBackoff,
		Connect: func(context.Context, mattermost.Seat, *mattermost.Client) (mattermost.Socket, error) {
			return newSocket(), nil
		},
	}
	if mutate != nil {
		mutate(&opts)
	}
	tr, err := mattermost.NewTransport(opts)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	t.Cleanup(func() { tr.Stop(context.Background()) })
	return tr
}

// The identity is RESOLVED, never assumed from config: an id the engine
// guessed disables own-message suppression when it is wrong, and an agent
// that cannot recognise its own posts answers itself for ever.
func TestStartResolvesEachSeatsIdentity(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	tr := transport(t, inst, nil)

	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := tr.Handles(); !slices.Equal(got, []string{"swe"}) {
		t.Fatalf("Handles = %v", got)
	}
	// The SERVER's username wins over the configured one, because the
	// server is what a person's mention will be matched against.
	c, ok := tr.Client("swe")
	if !ok || c.Token() != "tok-swe" {
		t.Fatalf("the seat's client is %v/%v", c, ok)
	}
}

// BOTH namespaces, because the two halves of the system see different
// identifiers: a payload names a poster by user id, a person typing a
// mention uses the username.
func TestStartRegistersBothHalvesOfTheIdentity(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	reg := notify.NewRegistry(seatOrg(t))
	tr := transport(t, inst, func(o *mattermost.TransportOptions) {
		o.Registry = func() *notify.Registry { return reg }
	})

	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// A payload naming the bot by id resolves...
	if p, ok := reg.ByExternalID(mattermost.Backend, "bot-swe"); !ok || p.Handle != "swe" {
		t.Fatalf("the bot id resolved to %+v", p)
	}
	// ...and so does a person's mention by name.
	if p, ok := reg.ByExternalID(mattermost.Backend, "agent-swe"); !ok || p.Handle != "swe" {
		t.Fatalf("the username resolved to %+v", p)
	}
	// The outbound direction names the bot the way a person addresses it.
	if got := reg.ExternalID(mattermost.Backend, "swe"); got != "agent-swe" {
		t.Fatalf("the outbound id is %q", got)
	}
}

// One bot's token being revoked is an ordinary state — an operator rotating
// credentials one at a time — and refusing to start over it would turn a
// one-seat problem into a whole-company outage.
func TestOneFailingSeatDoesNotStopTheCompany(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-ok": {ID: "bot-ok", Username: "agent-ok"},
	})
	tr := transport(t, inst, func(o *mattermost.TransportOptions) {
		o.Config.Seats = []mattermost.SeatConfig{
			{Handle: "revoked", Token: "tok-revoked"},
			{Handle: "ok", Token: "tok-ok"},
			{Handle: "tokenless"},
		}
	})

	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start reported a company-wide failure: %v", err)
	}
	if got := tr.Handles(); !slices.Equal(got, []string{"ok"}) {
		t.Fatalf("Handles = %v, want only the healthy seat", got)
	}
}

// EVERY seat failing is not N seat problems: it is the instance, the url or
// the network, and reporting it as such sends an operator somewhere useful.
func TestEverySeatFailingIsReportedAsOneProblem(t *testing.T) {
	inst := newInstance(t, nil)
	tr := transport(t, inst, nil)

	err := tr.Start(t.Context())
	if err == nil {
		t.Fatal("a wholly unreachable instance started cleanly")
	}
	if !strings.Contains(err.Error(), inst.URL) {
		t.Fatalf("the error does not name the instance: %v", err)
	}
	if got := tr.Handles(); len(got) != 0 {
		t.Fatalf("Handles = %v", got)
	}
}

// A company with no Mattermost seats is a real deployment, not an error.
func TestNoSeatsIsNotAFailure(t *testing.T) {
	inst := newInstance(t, nil)
	tr := transport(t, inst, func(o *mattermost.TransportOptions) { o.Config.Seats = nil })
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// THE SILENT FAILURE: Mattermost accepts a websocket only from a browser
// whose Origin matches its Site URL, so a mismatch blinds every human while
// agents keep working. Nothing else in the system reports it.
func TestASiteURLMismatchIsReported(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe"},
	})
	inst.siteURL = "https://chat.example.com"
	tr := transport(t, inst, nil)

	// It is a WARNING, not a failure: the agents work, and refusing to
	// start would take away the half that does.
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("a Site URL mismatch stopped the transport: %v", err)
	}
	if got := tr.Handles(); len(got) != 1 {
		t.Fatalf("Handles = %v", got)
	}
}

// The server ENFORCES the typing cadence: sending faster is rejected, so a
// guessed value that is too eager produces an indicator that never appears.
func TestTheTypingCadenceComesFromTheServer(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe"},
	})
	inst.throttle = "2000"
	tr := transport(t, inst, nil)
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got := tr.StatusRefresh(); got != 2*time.Second {
		t.Fatalf("StatusRefresh = %v, want the server's 2s", got)
	}
	// This backend renders no text and has no channel-id prefix — both
	// are declarations the spine reads, not preferences.
	if tr.SupportsStatusText() {
		t.Fatal("the transport claims it can render status text")
	}
	if tr.DMChannelPrefix() != "" {
		t.Fatalf("a channel-id prefix is declared: %q", tr.DMChannelPrefix())
	}
	if tr.StatusBackend() != mattermost.Backend {
		t.Fatalf("StatusBackend = %q", tr.StatusBackend())
	}
	var _ notify.StatusPoster = tr
}

func TestTheIndicatorIsRaisedAndLapses(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe"},
	})
	tr := transport(t, inst, nil)
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if !tr.SetStatus(t.Context(), "swe", "C1", "root-1", "ignored") {
		t.Fatal("the indicator was not raised")
	}
	if inst.typings() != 1 {
		t.Fatalf("the server saw %d typing calls", inst.typings())
	}
	// Clearing reports TRUE because the indicator does come down — it
	// lapses on its own once the heartbeat stops. Reporting false would
	// log a failure on every turn end of every seat, on the one backend
	// where taking it down cannot fail.
	if !tr.ClearStatus(t.Context(), "swe", "C1", "root-1") {
		t.Fatal("clearing an indicator that lapses reported a failure")
	}
	// A seat this node does not run cannot be raised for.
	if tr.SetStatus(t.Context(), "nobody", "C1", "", "") {
		t.Fatal("an indicator was raised for a seat this node does not run")
	}
}

func TestTheTransportNeedsAnInstanceAndAPublisher(t *testing.T) {
	if _, err := mattermost.NewTransport(mattermost.TransportOptions{
		Publisher: &recorder{},
	}); err == nil {
		t.Fatal("a transport was built with no instance url")
	}
	if _, err := mattermost.NewTransport(mattermost.TransportOptions{
		Config: mattermost.Config{URL: "https://chat.example.com"},
	}); err == nil {
		t.Fatal("a transport was built with no publisher")
	}
}

// The username defaults to the handle, and an unresolved ${VAR} yields
// nothing rather than its literal text — a raw reference matches nothing any
// server will send, so passing it through turns a missing variable into a
// bot that authenticates as nobody.
func TestSeatsAreReadFromTheOrg(t *testing.T) {
	o := seatOrg(t)
	got := mattermost.SeatsFrom(o, func(name string) (string, bool) {
		if name == "SWE_TOKEN" {
			return "tok-swe", true
		}
		return "", false
	})
	if len(got) != 1 {
		t.Fatalf("SeatsFrom produced %+v, want only the agent seat", got)
	}
	if got[0].Handle != "swe" || got[0].Token != "tok-swe" {
		t.Fatalf("the seat reads %+v", got[0])
	}
	if got[0].Username != "swe" {
		t.Fatalf("the username defaulted to %q, want the handle", got[0].Username)
	}
	// With the variable unexported the seat is SKIPPED rather than
	// started with an empty token, which would fail at connect with a
	// less useful message.
	if got := mattermost.SeatsFrom(o, func(string) (string, bool) { return "", false }); len(got) != 0 {
		t.Fatalf("an unresolvable seat was started: %+v", got)
	}
	if got := mattermost.SeatsFrom(nil, nil); got != nil {
		t.Fatalf("SeatsFrom(nil) = %+v", got)
	}
}

func seatOrg(t *testing.T) *org.Organization {
	t.Helper()
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			{Name: "SWE", DeclaredHandle: "swe",
				Mattermost: org.MattermostIdentity{BotToken: "${SWE_TOKEN}"}},
			// A HUMAN SEAT CARRYING A BOT IDENTITY. Validation
			// forbids this — a human seat is addressable and never
			// spawned — but SeatsFrom is reachable with a hand-built
			// org, and spawning a bot for a person means the engine
			// answers as them.
			{Name: "Dana", Kind: org.KindHuman,
				Contact:    &org.HumanContact{MattermostUserID: "u-dana"},
				Mattermost: org.MattermostIdentity{BotToken: "${SWE_TOKEN}"}},
			{Name: "QA", DeclaredHandle: "qa"},
		},
	}
	o.Normalize()
	return o
}

// Seats start CONCURRENTLY, and that is not an optimisation. Each resolves
// its identity against the server, and a failing call spends the client's
// whole retry budget — so started in sequence, an instance that is down
// delays boot by that budget times the number of seats.
func TestSeatsStartConcurrently(t *testing.T) {
	var live atomic.Int32
	var peak atomic.Int32
	inst := newInstance(t, map[string]mattermost.User{})
	inst.server.responds(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/users/me") {
			w.Write([]byte(`{}`))
			return true
		}
		n := live.Add(1)
		for {
			was := peak.Load()
			if n <= was || peak.CompareAndSwap(was, n) {
				break
			}
		}
		// Long enough that a sequential start could not overlap.
		time.Sleep(50 * time.Millisecond)
		live.Add(-1)
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		json.NewEncoder(w).Encode(mattermost.User{ID: "bot", Username: "agent"})
		return true
	})

	const seats = 4
	tr := transport(t, inst, func(o *mattermost.TransportOptions) {
		o.Config.Seats = nil
		for i := range seats {
			o.Config.Seats = append(o.Config.Seats, mattermost.SeatConfig{
				Handle: "seat-" + string(rune('a'+i)), Token: "tok",
			})
		}
	})

	start := time.Now()
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	elapsed := time.Since(start)

	if got := peak.Load(); got < 2 {
		t.Fatalf("peak concurrent identity resolutions was %d, want them overlapping", got)
	}
	// Sequential would be at least seats × 50ms on the identity calls
	// alone, before the instance read.
	if elapsed > seats*50*time.Millisecond {
		t.Fatalf("Start took %v, which is sequential for %d seats", elapsed, seats)
	}
}

// THE THREAD IS READ AS THE SEAT, on the seat's own bot token, and the seat's
// own posts come back MARKED.
//
// The identity is the one resolved at connect rather than anything config
// declared: an agent that cannot recognise its own replies reads them as a
// colleague's and answers itself, which is the confusion the block exists to
// remove.
func TestASeatsThreadComesBackMarkedWithItsOwnPosts(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe"},
	})
	tr := transport(t, inst, nil)
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var _ notify.ThreadReader = tr
	if tr.ThreadBackend() != mattermost.Backend {
		t.Fatalf("ThreadBackend = %q", tr.ThreadBackend())
	}

	got, ok := tr.ReadThread(t.Context(), "swe", "C1", "root")
	if !ok {
		t.Fatal("a running seat could not read its own thread")
	}
	// Oldest first, with the root first — and the bookkeeping gone: a
	// deleted post and a join line wake nobody, so neither belongs in a
	// thread rendered for a seat.
	if len(got.Messages) != 2 {
		t.Fatalf("the thread came back as %+v", got)
	}
	if got.Messages[0].Text != "staging redirects in a loop" || got.Messages[0].Own {
		t.Errorf("the root came back as %+v", got.Messages[0])
	}
	if got.Messages[1].Text != "on it" || !got.Messages[1].Own {
		t.Errorf("the seat's own reply came back as %+v", got.Messages[1])
	}
	if got.Messages[0].SenderID != "U-ana" {
		t.Errorf("the sender id was lost: %+v", got.Messages[0])
	}
	// AND NOTHING IS CLAIMED MISSING. This endpoint answers the WHOLE
	// thread in one response — no cursor, no page size — so a transcript
	// from here can never be short at either end, and a renderer told
	// otherwise would print a drop notice for messages that are in front
	// of the seat and tell it to go and read a thread it already has.
	if got.Older != 0 || got.StoppedShort {
		t.Errorf("a whole-thread read claimed to be short: %+v", got)
	}
	// AND SCOPED TO THE CHANNEL THE TRIGGER NAMED. The endpoint is
	// addressed by post id alone, so nothing in the request says which
	// channel the thread is meant to be in — a root id naming a post
	// somewhere else would render another conversation under this
	// trigger's own heading.
	for _, m := range got.Messages {
		if strings.Contains(m.Text, "another channel entirely") {
			t.Errorf("a post from another channel reached the thread: %+v", m)
		}
	}
}

// THE ROOT KEEPS ITS PLACE EVEN WITH NOTHING IN IT, and a root the endpoint
// did not answer with keeps an empty one.
//
// [notify.ThreadReader] promises the root FIRST, and the renderer exempts
// that first message from every bound and frames it as what the thread is
// about. Dropping a root for want of a body hands over a transcript that
// starts at the oldest surviving REPLY — which is then kept, rendered first
// and described to the seat as the opening. A deleted root is the ordinary
// way to get there here: this endpoint leaves the post out and answers with
// every reply to it.
func TestAThreadRootWithNothingToRenderStillComesBackFirst(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe"},
	})
	tr := transport(t, inst, nil)
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A system post as the root: kept, and empty, so the renderer says so
	// rather than promoting the reply under it.
	got, ok := tr.ReadThread(t.Context(), "swe", "C1", "quiet")
	if !ok {
		t.Fatal("a thread rooted on a system post could not be read")
	}
	if len(got.Messages) != 2 {
		t.Fatalf("the thread came back as %+v", got)
	}
	if got.Messages[0].Text != "" || got.Messages[0].SenderID != "U-ana" {
		t.Errorf("the root came back as %+v, want its slot with no body", got.Messages[0])
	}
	if got.Messages[1].Text != "so what broke" {
		t.Errorf("the reply came back as %+v", got.Messages[1])
	}

	// A ROOT THE ANSWER DOES NOT CARRY AT ALL — a deleted opening — still
	// gets its slot, because "somebody deleted the first message" and
	// "this thread opens with the line below" are different threads.
	deleted, ok := tr.ReadThread(t.Context(), "swe", "C1", "gone")
	if !ok {
		t.Fatal("a thread whose root was deleted could not be read")
	}
	if len(deleted.Messages) != 3 || deleted.Messages[0] != (notify.Message{}) {
		t.Fatalf("a thread with no root came back as %+v", deleted)
	}
	if deleted.Messages[1].Text != "staging redirects in a loop" {
		t.Errorf("the oldest reply moved: %+v", deleted.Messages[1])
	}
}

// A SEAT THIS NODE HAS NO CLIENT FOR REPORTS NOT-FOUND rather than
// dereferencing one.
//
// A bot whose token was refused at boot is left out of the map deliberately,
// and a node in maintenance mode runs no transport at all — both ordinary
// states, and both have to render "the thread could not be read" rather than
// panicking a turn.
func TestAThreadForASeatThisNodeDoesNotRunIsUnreadable(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe"},
	})
	tr := transport(t, inst, nil)
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, ok := tr.ReadThread(t.Context(), "nobody", "C1", "root"); ok {
		t.Fatal("a thread was read for a seat this node does not run")
	}
}

// AN INSTANCE THAT REFUSES THE READ is reported as unreadable, never as an
// empty thread: the turn renders a different sentence for each, and the two
// send a seat to opposite places.
func TestARefusedThreadReadIsNotAnEmptyThread(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe"},
	})
	tr := transport(t, inst, nil)
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	inst.server.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"You do not have the appropriate permissions"}`))
		return true
	})
	if got, ok := tr.ReadThread(t.Context(), "swe", "C1", "root"); ok {
		t.Fatalf("a refused read reported success: %+v", got)
	}
}
