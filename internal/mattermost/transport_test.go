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
	posts      []mattermost.PostRequest
}

func newInstance(t *testing.T, identities map[string]mattermost.User) *instance {
	t.Helper()
	inst := &instance{server: newServer(t), identities: identities, throttle: "2000"}
	inst.siteURL = inst.URL
	inst.server.respond = func(w http.ResponseWriter, r *http.Request) bool {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case strings.HasSuffix(r.URL.Path, "/config/client"):
			json.NewEncoder(w).Encode(map[string]string{
				"SiteURL": inst.siteURL,
				"TimeBetweenUserTypingUpdatesMilliseconds": inst.throttle,
			})
		case strings.HasSuffix(r.URL.Path, "/typing"):
			inst.mu.Lock()
			inst.typing = append(inst.typing, r.URL.Path)
			inst.mu.Unlock()
			w.Write([]byte(`{}`))
		case r.URL.Path == "/api/v4/posts" && r.Method == http.MethodPost:
			var req mattermost.PostRequest
			json.NewDecoder(r.Body).Decode(&req)
			inst.mu.Lock()
			inst.posts = append(inst.posts, req)
			inst.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"id": "sent-1"})
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
	}
	return inst
}

func (i *instance) typings() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.typing)
}

func (i *instance) sent() []mattermost.PostRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]mattermost.PostRequest(nil), i.posts...)
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

// A shared identity would make every agent's message come from one account,
// and a company whose members are indistinguishable is not a company.
func TestASeatPostsAsItsOwnBot(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe"},
	})
	tr := transport(t, inst, nil)
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := tr.Send(t.Context(), "swe", "C1", "root-1", "here you go"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sent := inst.sent()
	if len(sent) != 1 || sent[0].ChannelID != "C1" || sent[0].RootID != "root-1" {
		t.Fatalf("sent %+v", sent)
	}
	// A reply that lost its thread becomes a new conversation in the
	// channel rather than an answer.
	if sent[0].Message != "here you go" {
		t.Fatalf("the message was %q", sent[0].Message)
	}
	if _, err := tr.Send(t.Context(), "nobody", "C1", "", "hi"); err == nil {
		t.Fatal("a seat this node does not run posted anyway")
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
	inst.server.respond = func(w http.ResponseWriter, r *http.Request) bool {
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
	}

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
