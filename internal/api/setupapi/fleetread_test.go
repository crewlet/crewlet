package setupapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// peerDoc is a revision another node wrote: the CTO's Slack bot token points
// at a credential that write sealed, which this node has never seen.
const peerDoc = `{
  "name": "Acme",
  "providers": {"llm": {"zulu": {"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${K}"]}}},
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"},
    {"name": "CTO", "handle": "cto", "llm": "zulu",
     "integrations": {"slack": {"bot_token": "${SLACK_PEER_BOT}",
       "signing_secret": "${SLACK_PEER_SIGNING}"}}}
  ]
}`

// plantPeerRevision points the fleet at document as another node's write
// does: on the activation pointer alone, a revision this node's history does
// not hold. It answers the revision's id.
func (s *surface) plantPeerRevision(t *testing.T, document string) string {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(document))
	if err != nil {
		t.Fatalf("parse the peer's document: %v", err)
	}
	stored, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Seal(s.cipher, stored)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7()).String()
	at := time.Now().UTC().Add(time.Minute)
	if _, err := s.plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: "a peer's write", Payload: payload, At: at,
		Origin: coord.RevisionOrigin{Author: "operator", AuthorKind: iam.ActorOperator,
			Source: "peer", CreatedAt: at},
	}); err != nil {
		t.Fatalf("activate the peer's revision: %v", err)
	}
	return id
}

// A LISTING NAMES THE REVISION IT DESCRIBES, and a write built on it keeps
// what that revision changed.
//
// The listing used to describe the company this node had applied and name the
// revision the fleet's pointer named. On a node behind the pointer the two
// differ: a client sent the pointer's id back as `if_match`, passed the
// stale-base check, and wrote with requirements read from the older revision —
// so the CTO's bot token, which the newer revision pointed at a credential its
// own write sealed, was pointed at a freshly minted name instead, and the
// peer's change was lost under a 201. Both halves are the fleet's revision
// now. Checked to fail with the listing built from this node's own copy: the
// listing named the peer's revision and described the seed, and the write
// replaced ${SLACK_PEER_BOT}.
func TestASetupWriteBuiltOnAPeersRevisionKeepsWhatItChanged(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	peer := s.plantPeerRevision(t, peerDoc)

	listing := decode(t, s.do(t, http.MethodGet, "/setup/integrations", "", nil))
	if got := listing["revision_id"]; got != peer {
		t.Fatalf("the listing names revision %v; the fleet is on %s", got, peer)
	}
	cto := slackSeatRows(t, s)["cto"]
	if cto == nil {
		t.Fatal("the roster does not list the CTO")
	}
	if !strings.Contains(mustJSON(t, cto), "SLACK_PEER_BOT") {
		t.Fatalf("the CTO's row does not describe the peer's revision, whose bot "+
			"token points at ${SLACK_PEER_BOT}: %s", mustJSON(t, cto))
	}

	res := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs", `{
		"seat": "cto", "if_match": "`+peer+`",
		"values": {"bot_token": "xoxb-rotated", "signing_secret": "sig-cto"}
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	doc := s.do(t, http.MethodGet, "/config", "", nil).Body.String()
	if !strings.Contains(doc, "${SLACK_PEER_BOT}") {
		t.Errorf("the write replaced the peer's bot token pointer: %s", doc)
	}
	if strings.Contains(doc, "${SLACK_BOT_TOKEN_CTO}") {
		t.Errorf("the write minted a new name over the peer's: %s", doc)
	}
	if _, ok := s.vault.get("SLACK_PEER_BOT"); !ok {
		t.Error("the rotated token was not written through the peer's pointer")
	}
}

// A WRITE BUILT ON A REVISION THE FLEET HAS MOVED PAST IS REFUSED before
// anything is sealed: the listing a client read named the seed, and the
// pointer moved on before it wrote.
func TestASetupWriteOnARevisionTheFleetMovedPastIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	seen, _ := decode(t, s.do(t, http.MethodGet, "/setup/integrations", "", nil))["revision_id"].(string)
	peer := s.plantPeerRevision(t, peerDoc)

	res := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs", `{
		"seat": "cto", "if_match": "`+seen+`",
		"values": {"bot_token": "xoxb-one", "signing_secret": "sig-one"}
	}`, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "revision_advanced" || body["current_revision_id"] != peer {
		t.Errorf("refusal = %v, want revision_advanced naming %s", body, peer)
	}
	if _, sealed := s.vault.get("SLACK_BOT_TOKEN_CTO"); sealed {
		t.Error("a refused write sealed a credential")
	}
}

// blinkingPlane is the fleet's activation pointer, which a case can make
// unreadable: a coordination store that stopped answering, at once or after
// some more reads.
type blinkingPlane struct {
	coord.Plane
	// answers is how many more reads of the pointer are answered; negative
	// is every one.
	answers atomic.Int64
}

func newBlinkingPlane(p coord.Plane) *blinkingPlane {
	b := &blinkingPlane{Plane: p}
	b.answers.Store(-1)
	return b
}

// blindAfter makes every read of the pointer after the next n fail.
func (p *blinkingPlane) blindAfter(n int64) { p.answers.Store(n) }

func (p *blinkingPlane) Target(ctx context.Context) (coord.Activation, bool, error) {
	for {
		left := p.answers.Load()
		if left < 0 {
			return p.Plane.Target(ctx)
		}
		if left == 0 {
			return coord.Activation{}, false, coord.ErrUnavailable
		}
		if p.answers.CompareAndSwap(left, left-1) {
			return p.Plane.Target(ctx)
		}
	}
}

// A SETUP WRITE THE FLEET'S POINTER CANNOT BE READ FOR IS A CONDITION, told as
// PATCH /config tells it: a setup write reads the pointer again as it writes,
// and a coordination store that stopped answering between the route's read of
// the company and the write's own was a `500 internal_error` saying the engine
// had broken, with nothing about trying again, while PATCH /config on the
// same moment answered `503 unavailable` with a Retry-After. Checked to fail
// with the condition arm taken out of the write's refusal.
func TestASetupWriteOnAnUnreadablePointerIsAConditionAsAConfigWriteIs(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	seen, _ := decode(t, s.do(t, http.MethodGet, "/setup/integrations", "", nil))["revision_id"].(string)
	plane := s.plane.(*blinkingPlane)
	plane.blindAfter(0)

	patch := s.do(t, http.MethodPatch, "/config", `{"name": "Acme Two"}`,
		map[string]string{"Content-Type": "application/merge-patch+json"})
	if patch.Code != http.StatusServiceUnavailable {
		t.Fatalf("PATCH /config: status = %d, want 503: %s", patch.Code, patch.Body)
	}
	want := decode(t, patch)["detail"]

	for name, body := range map[string]string{
		"with if_match": `{"seat": "cto", "if_match": "` + seen + `",
			"values": {"bot_token": "xoxb-one", "signing_secret": "sig-one"}}`,
		"without": `{"seat": "cto",
			"values": {"bot_token": "xoxb-one", "signing_secret": "sig-one"}}`,
	} {
		// The route's read of the company is answered; the write's own is
		// not.
		plane.blindAfter(1)
		res := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs", body, nil)
		if res.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503: %s", name, res.Code, res.Body)
			continue
		}
		if res.Header().Get("Retry-After") == "" {
			t.Errorf("%s: a condition a wait clears carries no Retry-After", name)
		}
		if got := decode(t, res)["detail"]; got != want {
			t.Errorf("%s: detail = %v, want PATCH /config's %v", name, got, want)
		}
	}
	plane.blindAfter(0)
	listing := s.do(t, http.MethodGet, "/setup/integrations", "", nil)
	if listing.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /setup/integrations: status = %d, want 503: %s",
			listing.Code, listing.Body)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
