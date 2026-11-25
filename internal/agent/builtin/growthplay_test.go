package builtin_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googleplay"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// playTokens mints a seat's token, as the engine's issuer would.
type playTokens struct{}

func (playTokens) GoogleGrowthToken(_ context.Context, handle string, _ []string, _ time.Duration,
	_ *http.Client) (googleauth.SeatToken, error) {
	return googleauth.SeatToken{Token: "ya29.seat-" + handle, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// playLog is the access log a Google Play act records on: every step, in
// order, and each held call's one answer.
type playLog struct {
	mu       sync.Mutex
	steps    []growth.Step
	answered map[string]bool
}

func (l *playLog) add(step growth.Step) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, step)
	return true, nil
}

func (l *playLog) Made(context.Context, growth.Record) (bool, error) { return l.add(growth.StepMade) }
func (l *playLog) Applied(context.Context, growth.Record) (bool, error) {
	return l.add(growth.StepApplied)
}
func (l *playLog) Void(context.Context, growth.Record) (bool, error) { return l.add(growth.StepVoid) }
func (l *playLog) Failed(context.Context, growth.Record) (bool, error) {
	return l.add(growth.StepFailed)
}
func (l *playLog) Verdict(_ context.Context, step growth.Step, r growth.Record) (bool, error) {
	l.mu.Lock()
	if l.answered == nil {
		l.answered = map[string]bool{}
	}
	first := !l.answered[r.HeldCall]
	l.answered[r.HeldCall] = true
	l.mu.Unlock()
	if !first {
		return false, nil
	}
	return l.add(step)
}

// playLeases grants every lease.
type playLeases struct{}

func (playLeases) Hold(context.Context, string) (func(), error) { return func() {}, nil }

// A GOOGLE PLAY REPLY RUNS THROUGH THE REAL TOOL SESSION EXACTLY ONCE, ON A
// PERSON'S ANSWER: the call is held as a public act for an administrator,
// nothing reaches the store before the answer, the allowed reply is posted
// once with the text shown, a second run of the same approval posts nothing,
// and the access log holds the ask, the answer and the outcome in order.
//
// Mutation: drop the held call's approval check (CallForTurn) or Allow's
// one-answer rule, and the reply is posted twice or unasked.
func TestAGooglePlayReplyRunsOnceOnAPersonsAnswer(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/androidpublisher/v3/applications/com.acme.app/reviews/r1":
			_ = json.NewEncoder(w).Encode(map[string]any{"reviewId": "r1", "authorName": "Pat",
				"comments": []any{map[string]any{"userComment": map[string]any{"text": "Crashes on start",
					"starRating": 1}}}})
		case "/androidpublisher/v3/applications/com.acme.app/reviews/r1:reply":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			text, _ := body["replyText"].(string)
			posted = append(posted, text)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"replyText": text}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	company, err := config.ParseCompanyDocument([]byte(`
name: Acme
integrations:
  google_cloud:
    enabled: true
    provisioning:
      identity_project: acme-crewlet-id
      provider: projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine
      federation_key: "${GOOGLE_CLOUD_FEDERATION_KEY}"
  google_play:
    enabled: true
roles:
  - name: SRE
    handle: sre
    integrations:
      google_play: {enabled: true, level: operate, apps: ["com.acme.app"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	env := config.NewResolver(config.MapSource{
		"GOOGLE_CLOUD_FEDERATION_KEY": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	log := &playLog{}
	product := googleplay.New(googleplay.Deps{Company: func() *config.Company { return company },
		Env: func() *config.Resolver { return env }, Tokens: playTokens{}, Audit: log, Leases: playLeases{},
		HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}})
	_, _, chart := sshFixture(t)
	reg := tools.NewRegistry()
	asks := &growthAudit{}
	if _, err := builtin.Register(reg, builtin.Deps{Growth: builtin.GrowthDeps{
		Products: []builtin.GrowthProduct{product}, Requesters: healthRequesters(), Chart: chart,
		Audit: asks}}); err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Lookup("google_play_reply_to_reviews")
	if !ok || !entry.ApprovalFixed {
		t.Fatalf("the reply tool is registered %v, asked before every call %v", ok, entry.ApprovalFixed)
	}
	args := map[string]any{"app": "com.acme.app", "reason": "a one-star review",
		"replies": []any{map[string]any{"review": "r1", "text": "Fixed in 4.2: please update."}}}
	desk := &askDesk{outcome: sandbox.AskRecorded}
	ctx := turnctx.WithJob(asSeat("sre"), "launch-1")
	turn := growthTurn(t, "sre")
	got, held := builtin.NewCallHold(desk, callSeal(t), nil, nil).Hold(ctx, turn, tools.HeldCall{
		Name: "google_play_reply_to_reviews", Entry: entry, Args: args})
	if !held || got.Failed || len(desk.recorded) != 1 {
		t.Fatalf("the reply was not held: %q", got.Output)
	}
	h := desk.recorded[0].Held
	if h.Approvers != sandbox.HeldApproversAdmin || h.Act != "public_post" ||
		!strings.Contains(desk.recorded[0].Context, "Fixed in 4.2: please update.") {
		t.Fatalf("the reply is held as %+v, asked %q", h, desk.recorded[0].Context)
	}
	if len(posted) != 0 {
		t.Fatalf("the store got %v before anybody answered", posted)
	}
	reply := asSeatCallable(t, tools.Innermost(entry.Tool))
	approved := tools.WithApproval(asSeat("sre"), tools.Approval{By: "founder", HeldID: h.ID, Binding: h.Binding})
	for range 2 {
		_, _ = reply.CallForTurn(approved, turn, args)
	}
	if !slices.Equal(posted, []string{"Fixed in 4.2: please update."}) {
		t.Fatalf("the store got %v", posted)
	}
	if len(asks.records()) != 1 || !slices.Equal(log.steps, []growth.Step{growth.StepAllowed, growth.StepApplied}) {
		t.Fatalf("the access log holds asks %d, steps %v", len(asks.records()), log.steps)
	}
}
