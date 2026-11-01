package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// TestPermissionParityAcrossFivePrincipals: THE ASSISTANT SEES WHAT ITS PERSON
// SEES, AND NOTHING ELSE (ADR-0050), for every read tool in the catalogue.
//
// Four people sign in to one company through the guard's real session arm:
// an administrator, a person holding only Worklet, one holding only the
// knowledge base, and one holding nothing but the overview everybody reads.
// For each of them, every catalogue read is made through the assistant, as a
// scripted model calling the tool in a real turn of the real orchestrator,
// whose requests the App's own dispatcher carries on a grant derived from the
// person's cookie through the App's own guard, and EVERY REQUEST THE TURN
// SENDS is made again as the person, with that cookie. Every one, not only the
// read's own: a read's step and its card read through the same grant (a chat
// room a call names by its id is read to say where the read is, and a direct
// conversation is said by its people, so the person's own seat and the seats'
// names are read as well), and each of those reads is the person's too. The
// answers must be the SAME BYTES with the same status: an answer stays that
// answer, and a refusal stays that refusal. The company's sources are real
// enough that the split is real: every tool's question is registered, so a
// refusal is the authority's and never a route this fixture forgot to mount
// (a question the fixture does not serve fails the walk, rather than passing
// as two equal 404s), and the walk fails when any principal sees neither an
// answer nor a refusal anywhere.
//
// The catalogue's offering is a filter and never authority, so every read is
// offered to the turn, including those the curated catalogue keeps from this
// person: a refusal must come from the API, as the same code and permissions
// (exec.RefusalOf) the person's own request is refused with, and shown so on
// the drawer's step in the engine's sentence for it (cards.PersonRefusal). The curated catalogue must never offer a
// read the person is refused.
//
// And two principals may not use the assistant at all, a session exchanged
// from a Tier A token and a machine token: the same requests made with their
// own credential are answered, while no grant can be derived from either and
// no turn is started for either, so through the assistant every tool is a
// refusal.
//
// Zero tolerance: any difference fails the suite, the one exception being an
// instant an answer reads off the wall clock while it is asked ([sameAnswer]).
// Mutation: give the principal an assistant grant resolves to one more
// permission (internal/api/auth's assist arm), and the overview-only person's
// assistant reads Worklet; let iam.Assistable admit everybody, and a
// token's session derives a grant and a machine token is started a turn.
// Give it Chatlet, and the room a chat read names by its id is answered to the
// overview-only person's assistant while their cookie is refused it, before
// the read itself is sent; leave chat out of [parityWorldOf], and every chat
// read is a question nobody serves; send a read other than the one its tool
// builds, and the walk says so.
func TestPermissionParityAcrossFivePrincipals(t *testing.T) {
	t.Parallel()
	sources := parityWorld(t)
	reads := readTools()
	people := []struct {
		name   string
		grants []iam.Grant
	}{
		{"an administrator", []iam.Grant{iam.GrantAdmin}},
		{"a tracker-only person", []iam.Grant{iam.GrantWorklet}},
		{"a knowledge-only person", []iam.Grant{iam.GrantDoclet}},
		{"an overview-only person", []iam.Grant{}},
	}
	for _, who := range people {
		t.Run(who.name, func(t *testing.T) {
			t.Parallel()
			fx := newAssistAppWith(t, who.grants, sources.mount)
			person := fx.principal(t)

			// THROUGH THE ASSISTANT: a turn per catalogue read, the
			// scripted model calling it once and answering. EVERY read is
			// offered to the turn, not only the ones the curated catalogue
			// would offer this person, because the offering is a filter and
			// never authority: a read the person may not make must be
			// refused by the API itself, exactly as their cookie is.
			sent := &recordingDispatcher{inner: fx.disp}
			var rounds []evals.Round
			for _, tool := range reads {
				rounds = append(rounds, evals.Call("c-"+tool.Name, tool.Name,
					assistanttest.ExampleArgs(t, tool.Parameters)), evals.Say("Done."))
			}
			rig := assistanttest.NewRig(t, evals.RigOptions{Rounds: rounds,
				Person: &person, Catalogue: evals.Catalogue(reads), Dispatcher: sent,
				Revoker: fx.d})
			onAppClock(rig)
			curated := catalog.Offered(person, nil)
			answered, refused := 0, 0
			for _, tool := range reads {
				own, err := tool.Request(assistanttest.ExampleArgs(t, tool.Parameters))
				if err != nil {
					t.Fatal(err)
				}
				began := time.Now()
				frames := askOne(t, rig, fx, tool.Name, nil)
				// EVERY REQUEST THE TURN SENT, each made again with the
				// cookie; the read's own answer then decides the step's.
				var status int
				var body []byte
				sentOwn := false
				for _, got := range sent.take() {
					personStatus, personBody := fx.asPerson(t, got.request)
					if got.response.Status != personStatus ||
						!sameAnswer(t, got.response.Body, personBody, time.Since(began)) {
						t.Errorf("a turn calling %s sent %s, answered %d %s through the "+
							"assistant and %d %s to the person", tool.Name,
							requestLine(got.request), got.response.Status, got.response.Body,
							personStatus, personBody)
					}
					if envelope, ok := httpjson.Decode(personBody); ok &&
						envelope.Code == httpjson.CodeUnknownQuery {
						t.Errorf("a turn calling %s sent %s, a question this fixture does not "+
							"serve: give its source to parityWorldOf", tool.Name,
							requestLine(got.request))
					}
					if sameRequest(got.request, own) {
						status, body, sentOwn = personStatus, personBody, true
					}
				}
				if !sentOwn {
					t.Errorf("a turn calling %s never sent %s, the request the tool builds",
						tool.Name, requestLine(own))
					continue
				}
				step := lastStep(t, frames, tool.Name)
				switch {
				case status == http.StatusUnauthorized || status == http.StatusForbidden:
					refused++
					// THE REFUSAL THE PERSON'S OWN REQUEST GETS: its code
					// and the permissions that would admit the person, as
					// the drawer's step shows them, with the engine's
					// sentence for that kind of refusal (cards.PersonRefusal).
					want := cards.PersonRefusal(exec.RefusalOf(exec.Response{Status: status,
						Body: body}), status)
					if step.Refusal == nil || !reflect.DeepEqual(*step.Refusal, want) {
						t.Errorf("%s was refused to the person as %+v and the assistant's "+
							"step shows %+v", tool.Name, want, step.Refusal)
					}
					if step.State != wire.StepRefused {
						t.Errorf("%s was refused to the person (%d) and the assistant's step "+
							"reads %s", tool.Name, status, step.State)
					}
					// AND THE CURATED CATALOGUE NEVER OFFERS IT, so a model
					// is not invited to try what will be refused.
					if slices.ContainsFunc(curated, func(o catalog.Tool) bool {
						return o.Name == tool.Name
					}) {
						t.Errorf("%s is offered to %s, whose own request it refuses",
							tool.Name, who.name)
					}
				case status < 300:
					answered++
					// AND THE CURATED CATALOGUE OFFERS IT, so a read the
					// dashboard answers the person is one the assistant
					// may make for them.
					if !slices.ContainsFunc(curated, func(o catalog.Tool) bool {
						return o.Name == tool.Name
					}) {
						t.Errorf("%s answers %s, and is not offered to them", tool.Name, who.name)
					}
					if step.State != wire.StepDone {
						t.Errorf("%s answered the person and the assistant's step reads %s",
							tool.Name, step.State)
					}
				}
			}
			// THE SPLIT IS REAL: everybody reads something, and everybody but
			// an administrator is refused something, or the fixture is not
			// testing what this principal may read.
			admin := slices.Contains(who.grants, iam.GrantAdmin)
			if answered == 0 || (!admin && refused == 0) {
				t.Errorf("%s was answered %d and refused %d reads: the fixture does not "+
					"split what this principal may read", who.name, answered, refused)
			}
		})
	}

	// THE TWO PRINCIPALS THE ASSISTANT REFUSES: each reads the company with
	// its own credential, and gets no grant and no turn.
	tierA := func(fx *assistApp) func(*http.Request) {
		bearer := fx.tokenSession(t)
		return func(r *http.Request) { r.AddCookie(bearer) }
	}
	machine := func(*assistApp) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+fixtureToken) }
	}
	for _, refusedTo := range []struct {
		name       string
		credential func(*assistApp) func(*http.Request)
	}{
		{"a Tier A token's session", tierA},
		{"a machine token", machine},
	} {
		t.Run(refusedTo.name, func(t *testing.T) {
			t.Parallel()
			fx := newAssistAppWith(t, []iam.Grant{iam.GrantAdmin}, sources.mount)
			present := refusedTo.credential(fx)
			answered := 0
			for _, tool := range reads {
				req, err := tool.Request(assistanttest.ExampleArgs(t, tool.Parameters))
				if err != nil {
					t.Fatal(err)
				}
				if status, _ := fx.serve(t, req, present); status < 300 {
					answered++
				}
			}
			if answered == 0 {
				t.Fatalf("%s reads nothing with its own credential, so its refusal below "+
					"proves nothing about the assistant", refusedTo.name)
			}
			grant, principal := fx.deriveWith(t, present, readTurn())
			if grant != nil {
				t.Fatalf("%s derived an assistant grant", refusedTo.name)
			}
			rig := assistanttest.NewRig(t, evals.RigOptions{Person: &principal,
				Catalogue: catalog.Curated{}, Dispatcher: fx.disp, Revoker: fx.d})
			_, err := rig.AskWith("refused", "What is on my plate?", nil,
				evals.Grant{GrantID: uuid.Must(uuid.NewV7()),
					ExpiresAt: time.Now().Add(time.Minute)})
			if !errors.Is(err, assistant.ErrNotAvailable) {
				t.Errorf("%s was started a turn: %v", refusedTo.name, err)
			}
		})
	}
}

// askOne asks one question in a conversation of its own (titled, so no title
// round is asked of the model), on a grant derived from the fixture person's
// cookie, and reads the turn to its end.
func askOne(t *testing.T, rig *assistanttest.Rig, fx *assistApp, name string,
	page *wire.PageContext) []wire.Frame {

	t.Helper()
	// ONE QUESTION EVERY THREE SECONDS, a person's sustained turn rate, so
	// the walk is never refused as a burst.
	rig.Clock.Advance(3 * time.Second)
	rig.Conversation = evals.ID("conversation/" + name)
	rig.Store.Seed(rig.Person.ID, rig.Conversation, "Asked about "+name)
	grant, _ := fx.deriveWith(t, fx.withCookie, readTurn())
	if grant == nil {
		t.Fatal("the fixture's person derived no grant")
	}
	accepted, err := rig.AskWith(name, "Use "+name+".", page, grant)
	if err != nil {
		t.Fatalf("ask %s: %v", name, err)
	}
	return rig.Follow(accepted.TurnID, 0)
}

// onAppClock sets the rig's clock to the App's, so a grant the App's
// delegations mint is judged live by the turn that carries it.
func onAppClock(rig *assistanttest.Rig) { rig.Clock.Advance(clock.Sub(rig.Clock.Now())) }

// sameAnswer reports whether two answers to one request are the same answer:
// the same bytes, or the same JSON in which the only difference is an instant
// the answer read off the wall clock while it was asked (an event store's
// window is "the last thirty days" from the moment it is read). Such an
// instant may differ by no more than elapsed, the time both requests took
// between them; every other member must be equal exactly.
func sameAnswer(t *testing.T, a, b []byte, elapsed time.Duration) bool {
	t.Helper()
	if bytes.Equal(a, b) {
		return true
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return sameValue(x, y, elapsed)
}

func sameValue(x, y any, elapsed time.Duration) bool {
	switch xv := x.(type) {
	case map[string]any:
		yv, ok := y.(map[string]any)
		if !ok || len(xv) != len(yv) {
			return false
		}
		for k, v := range xv {
			w, held := yv[k]
			if !held || !sameValue(v, w, elapsed) {
				return false
			}
		}
		return true
	case []any:
		yv, ok := y.([]any)
		if !ok || len(xv) != len(yv) {
			return false
		}
		for i := range xv {
			if !sameValue(xv[i], yv[i], elapsed) {
				return false
			}
		}
		return true
	case string:
		yv, ok := y.(string)
		if !ok {
			return false
		}
		if xv == yv {
			return true
		}
		xt, errX := time.Parse(time.RFC3339Nano, xv)
		yt, errY := time.Parse(time.RFC3339Nano, yv)
		return errX == nil && errY == nil && yt.Sub(xt).Abs() <= elapsed
	}
	// A number, a bool or null, as JSON decodes each: comparable.
	return x == y
}

// readTools are the catalogue's read tools.
func readTools() []catalog.Tool {
	var out []catalog.Tool
	for _, tool := range catalog.Tools() {
		if tool.Kind == catalog.KindRead {
			out = append(out, tool)
		}
	}
	return out
}

// paritySources are one company's sources every parity App shares: a live
// projection, an event store and a usage estate, a company, a coordination
// store, a tracker, a knowledge base, a work index, the executors, the
// artifacts, the company's own chat and the configuration history, so EVERY
// catalogue read's question is registered and its answer is the authority's
// to give.
type paritySources struct {
	queries.Sources
	state  *livestate.LiveState
	config *configapi.Service
}

// parityWorld builds the sources over one estate on a temporary path.
func parityWorld(t *testing.T) paritySources {
	t.Helper()
	return parityWorldOf(t, rosterCompany)
}

// parityWorldOf is [parityWorld] running the company source describes.
func parityWorldOf(t *testing.T, source string) paritySources {
	t.Helper()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "parity.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	company, err := config.ParseCompany([]byte(source))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": []byte("assistant-parity-key-of-32-bytes")}})
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	plane := coordmemory.NewFleet()
	configs, err := configapi.New(configapi.Options{Store: db, Plane: plane,
		Cipher: cipher, Node: "core-1", Holders: func(context.Context, func(string) bool, time.Time) (
			map[string]configapi.SeatHolder, error) {
			return nil, nil
		}})
	if err != nil {
		t.Fatalf("configapi.New: %v", err)
	}
	// AN ACTIVE REVISION, with principles, so the company document's routes
	// the catalogue reads (the principles) answer as the authority decides
	// rather than 404 for everybody, which no principal could be compared on.
	// Stored, and activated on the fleet: the company is the pointer.
	company.Mission = "Make every invoice add up."
	company.Policies = []string{"Money is an integer number of cents."}
	document, err := json.Marshal(company)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Seal(cipher, document)
	if err != nil {
		t.Fatal(err)
	}
	revision := store.Revision{
		CreatedByKind: iam.ActorOperator, Source: "test", CreatedBy: "operator",
		Summary: "the parity company", Payload: payload, CreatedAt: clock,
	}
	id, err := db.Configs().Insert(t.Context(), revision)
	if err != nil {
		t.Fatalf("store the parity company: %v", err)
	}
	if _, err := plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: revision.Summary, Payload: payload, At: clock,
		Origin: coord.RevisionOrigin{Author: revision.CreatedBy,
			AuthorKind: revision.CreatedByKind, Source: revision.Source, CreatedAt: clock},
		ExpectAbsent: true,
	}); err != nil {
		t.Fatalf("activate the parity company: %v", err)
	}
	state := livestate.New(livestate.WithClock(func() time.Time { return clock }))
	estate := apiArtifacts{}
	return paritySources{state: state, config: configs, Sources: queries.Sources{
		State: state, Events: eventfan.Solo("node-a", db.Events()), Usage: db.Replicated(),
		Company: companySource(t, company), Coord: coordmemory.New(),
		Work: stubWorkReader{}, Pages: posturePages{}, WorkSearch: noWorkHits{},
		Sandbox: noCodingRuns{}, Config: configs,
		Artifacts: &queries.ArtifactsSource{Gate: artifactsGate(estate), Reads: estate},
		Chat:      parityChat{}, ChatSearch: parityChat{},
	}}
}

// mount hands an App the shared sources.
func (s paritySources) mount(opts *api.Options) {
	opts.State, opts.Sources, opts.Config = s.state, s.Sources, s.config
}

// noWorkHits is a work index that finds nothing.
type noWorkHits struct{}

func (noWorkHits) Search(context.Context, worklet.SearchQuery) (worklet.SearchAnswer, error) {
	return worklet.SearchAnswer{Hits: []worklet.Ranked{}}, nil
}

// noCodingRuns is a node running no executor.
type noCodingRuns struct{}

func (noCodingRuns) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	return []sandbox.PendingRun{}, nil
}

func (noCodingRuns) Get(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, nil
}

// parityChat is the company's own chat holding ONE DIRECT CONVERSATION, at the
// id every example call names: the person and the CTO, where the CTO asked the
// person one question nobody has replied to. A DIRECT ONE, because a read that
// names it by its id says it by its people ([catalog.Tool.Room]): the turn
// reads the room, the person's own seat and the seats' names through its grant
// before the read itself, and the walk holds each of those to the person's own
// request as well. Any other room is one nobody holds.
type parityChat struct{ noReaderViews }

// parityDirect is parityChat's one room, which has no name: who is in it is
// all it is said by.
var parityDirect = chatlet.Channel{ID: assistanttest.RecordID, Kind: chatlet.KindDM}

// parityAsked is the one message said in parityChat's room, which names the
// person it asks.
func parityAsked(viewer string) chatlet.MessageView {
	return chatlet.MessageView{ChannelSeq: 1, Message: chatlet.Message{
		ID: assistanttest.RecordID, ChannelID: assistanttest.RecordID, Author: "cto",
		AuthorKind: chatlet.AuthorAgent, Body: "@" + viewer + " can the invoice export ship?",
		Mentions: []string{viewer}, CreatedAt: clock}}
}

func (parityChat) Channels(_ context.Context, viewer string, _ chatlet.ChannelsQuery,
	_ statelog.Freshness) (chatlet.ChannelListing, error) {
	asked := parityAsked(viewer)
	return chatlet.ChannelListing{Channels: []chatlet.ChannelSummary{{
		Channel: parityDirect, Unread: 1, Participants: []string{viewer, "cto"},
		Last: &chatlet.MessagePreview{MessageID: asked.Message.ID, Author: asked.Message.Author,
			AuthorKind: asked.Message.AuthorKind, Excerpt: asked.Message.Body,
			ChannelSeq: asked.ChannelSeq, At: asked.Message.CreatedAt}}}}, nil
}

func (parityChat) Channel(_ context.Context, viewer, id string, _ statelog.Freshness) (
	chatlet.ChannelDetail, error) {
	if id != assistanttest.RecordID {
		return chatlet.ChannelDetail{}, chatlet.ErrNotFound
	}
	return chatlet.ChannelDetail{Channel: parityDirect, MessageSeq: 1, Member: true,
		Members: []chatlet.ChannelMember{{Handle: viewer, JoinedAt: clock},
			{Handle: "cto", JoinedAt: clock}}}, nil
}

// ChannelNamed finds no room: a direct conversation has no name to be found
// by, and it is the only room here.
func (parityChat) ChannelNamed(context.Context, string, string, statelog.Freshness) (
	chatlet.ChannelDetail, error) {
	return chatlet.ChannelDetail{}, chatlet.ErrNotFound
}

// RoomNames names no room, for the same reason: a direct conversation has no
// name, and it is the only room here.
func (parityChat) RoomNames(context.Context, string, statelog.Freshness) (
	chatlet.RoomNames, error) {
	return chatlet.RoomNames{}, nil
}

func (parityChat) Directory(context.Context, string, statelog.Freshness) (
	chatlet.Directory, error) {
	return chatlet.Directory{}, nil
}

func (parityChat) Messages(_ context.Context, viewer string, q chatlet.TranscriptQuery,
	_ statelog.Freshness) (chatlet.Transcript, error) {
	if q.ChannelID != assistanttest.RecordID {
		return chatlet.Transcript{}, chatlet.ErrNotFound
	}
	return chatlet.Transcript{ChannelID: q.ChannelID,
		Messages: []chatlet.MessageView{parityAsked(viewer)}}, nil
}

func (parityChat) Thread(_ context.Context, viewer string, q chatlet.ThreadQuery,
	_ statelog.Freshness) (chatlet.Thread, error) {
	if q.ChannelID != assistanttest.RecordID || q.RootID != assistanttest.RecordID {
		return chatlet.Thread{}, chatlet.ErrNotFound
	}
	return chatlet.Thread{ChannelID: q.ChannelID, Root: parityAsked(viewer)}, nil
}

// Activity answers the one message as the place the viewer was named, which
// is how the assistant reads where a person was named.
func (parityChat) Activity(_ context.Context, viewer string, _ chatlet.ActivityQuery,
	_ statelog.Freshness) (chatlet.Activity, error) {
	asked := parityAsked(viewer)
	return chatlet.Activity{Items: []chatlet.ActivityItem{{
		Key: "mention:" + asked.Message.ID, Kind: chatlet.ActivityMention,
		MessageID: asked.Message.ID, ChannelID: asked.Message.ChannelID,
		ChannelKind: chatlet.KindDM, Actor: asked.Message.Author,
		ActorKind: asked.Message.AuthorKind, Excerpt: asked.Message.Body,
		At: asked.Message.CreatedAt}}}, nil
}

// Search finds the one message whatever it is asked, said in the room the
// answer names, as the serving node names every room its hits were said in.
func (parityChat) Search(_ context.Context, q estate.ChatQuery) (estate.ChatSearchAnswer, error) {
	asked := parityAsked(q.Viewer)
	return estate.ChatSearchAnswer{Searched: 1, Hits: []search.ChatHit{{
		MessageID: asked.Message.ID, ChannelID: asked.Message.ChannelID,
		Author: asked.Message.Author, Snippet: asked.Message.Body,
		CreatedAt: asked.Message.CreatedAt, Score: 1}},
		Rooms: map[string]chatlet.ReadableRoom{asked.Message.ChannelID: {
			ID: asked.Message.ChannelID, Kind: chatlet.KindDM}}}, nil
}

// withCookie presents the fixture person's session cookie.
func (a *assistApp) withCookie(r *http.Request) { r.AddCookie(a.cookie) }

// tokenSession is a session cookie exchanged from the fixture's Tier A token:
// the shape `crewlet` hands a browser that signed in with an operator's
// bootstrap token, which acts as that token and never as a person.
func (a *assistApp) tokenSession(t *testing.T) *http.Cookie {
	t.Helper()
	signer, err := session.New(session.Options{Material: a.keys,
		Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	login := iam.TokenLogin("fixture")
	bearer, err := signer.Mint(session.Mint{Lineage: uuid.Must(uuid.NewV7()),
		Person: login, Epoch: 1, Generation: 1, StartPosition: 1,
		AbsoluteExpiresAt: clock.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// THE SESSION'S OWN ROW, as `POST /auth/token` writes one; who it acts
	// as is composed from the Tier A entry by the guard, never from a row.
	a.rows[login] = session.Identity{Applied: cookieApplied, Generation: 1,
		Session: session.LineageRow{Found: true, Epoch: 1, ProvedAt: time.Now()},
		Person:  session.PersonRow{Found: true, Epoch: 1, Stage: iam.StageActive, Login: login}}
	return &http.Cookie{Name: a.cookie.Name, Value: bearer}
}

// deriveWith derives a grant through the App's own guard from the credential
// present sets, as the assistant's turn route does, and answers the principal
// the guard resolved beside it: nil and that principal when Derive refuses.
func (a *assistApp) deriveWith(t *testing.T, present func(*http.Request),
	req auth.AssistRequest) (exec.Grant, iam.Principal) {

	t.Helper()
	var g exec.Grant
	var p iam.Principal
	var how iam.Resolution
	var err error
	reached := false
	h := a.app.Guard().Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		p, how = iam.From(r.Context())
		g, err = a.d.Derive(r.Context(), req)
	}))
	r := httptest.NewRequest(http.MethodPost, "/assistant/conversations", nil)
	present(r)
	h.ServeHTTP(httptest.NewRecorder(), r)
	switch {
	case !reached:
		t.Fatal("the guard refused the credential before Derive was asked")
	case how != iam.Resolved:
		t.Fatalf("the guard passed the request with its principal %v, not resolved", how)
	case err != nil && !errors.Is(err, auth.ErrAssistRefused):
		t.Fatalf("Derive: %v", err)
	}
	return g, p
}

// principal is the person the fixture's cookie resolves to.
func (a *assistApp) principal(t *testing.T) iam.Principal {
	t.Helper()
	_, p := a.deriveWith(t, a.withCookie, readTurn())
	if p.Kind != iam.KindPerson {
		t.Fatalf("the fixture's cookie resolved %+v", p)
	}
	return p
}

// serve makes req as present's caller over the App's own handler.
func (a *assistApp) serve(t *testing.T, req exec.Request, present func(*http.Request)) (
	int, []byte) {

	t.Helper()
	target := req.Path
	if len(req.Query) > 0 {
		target += "?" + req.Query.Encode()
	}
	r := httptest.NewRequest(req.Method, target, bytes.NewReader(req.Body))
	present(r)
	rec := httptest.NewRecorder()
	a.app.ServeHTTP(rec, r)
	return rec.Code, rec.Body.Bytes()
}

// asPerson makes req as the fixture's person, with their cookie.
func (a *assistApp) asPerson(t *testing.T, req exec.Request) (int, []byte) {
	t.Helper()
	return a.serve(t, req, a.withCookie)
}

// recordingDispatcher is the App's dispatcher with every exchange the
// assistant made through it kept, so a suite compares the answer the turn
// received with the one the person gets.
type recordingDispatcher struct {
	inner exec.Dispatcher

	mu   sync.Mutex
	sent []exchange
}

// exchange is one request a turn sent and what it was answered.
type exchange struct {
	request  exec.Request
	response exec.Response
}

func (d *recordingDispatcher) Do(ctx context.Context, g exec.Grant, r exec.Request) (
	exec.Response, error) {

	resp, err := d.inner.Do(ctx, g, r)
	if err == nil {
		d.mu.Lock()
		d.sent = append(d.sent, exchange{request: r, response: resp})
		d.mu.Unlock()
	}
	return resp, err
}

// take is every exchange a turn made, in the order it made them, removed
// from the record. A read is NOT ONE REQUEST: its step and its card read
// through the same grant (a chat room the call names by its id, the person's
// own seat, the seats' names), and so may the turn's name resolution.
func (d *recordingDispatcher) take() []exchange {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.sent
	d.sent = nil
	return out
}

// sameRequest reports whether a and b are one request: its method, its path,
// its query and its body.
func sameRequest(a, b exec.Request) bool {
	return requestLine(a) == requestLine(b) && bytes.Equal(a.Body, b.Body)
}

// requestLine is req as a log line names it: its method, its path and its
// query.
func requestLine(req exec.Request) string {
	line := req.Method + " " + req.Path
	if len(req.Query) > 0 {
		line += "?" + req.Query.Encode()
	}
	return line
}

// writes are the exchanges whose method is not a read.
func (d *recordingDispatcher) writes() []exec.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []exec.Request
	for _, e := range d.sent {
		// A CHECK STORES NOTHING (ADR-0055): a read grant sends one only to
		// a route declaring it reads the flag (internal/authz's guard,
		// configapi's TestEveryWriteThatReadsDryRunDeclaresIt), so it is the
		// one request with a write's method that is not a write.
		check := e.request.Query.Get(authz.DryRunQuery) == "true"
		if e.request.Method != http.MethodGet && e.request.Method != http.MethodHead && !check {
			out = append(out, e.request)
		}
	}
	return out
}

// lastStep is the last step frame of tool's in frames.
func lastStep(t *testing.T, frames []wire.Frame, tool string) wire.StepEvent {
	t.Helper()
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].Kind == wire.EventStep && frames[i].Step.Tool == tool {
			return *frames[i].Step
		}
	}
	t.Fatalf("no step of %s in the turn", tool)
	return wire.StepEvent{}
}
