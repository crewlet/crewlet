package estate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// tappedBroker is a node's broker connection that keeps every byte it carries,
// both ways: what an observer of the broker sees of each question and its
// answer.
type tappedBroker struct {
	q *memory.Queue

	mu      sync.Mutex
	carried [][]byte
}

func (b *tappedBroker) Ask(ctx context.Context, subject string, request []byte,
	want int) ([][]byte, error) {

	replies, err := b.q.Ask(ctx, subject, request, want)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.carried = append(b.carried, slices.Clone(request))
	for _, raw := range replies {
		b.carried = append(b.carried, slices.Clone(raw))
	}
	return replies, err
}

// messages is every message the tap carried, in the order it carried them.
func (b *tappedBroker) messages() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.carried)
}

// routerOver is a router on node self, holding no data, asking the fleet's
// data nodes over q.
func (f *fleet) routerOver(t *testing.T, self string, q Asker) *Router {
	t.Helper()
	r, err := NewRouter(RouterOptions{Queue: q, Self: self, Placement: f.placement,
		Git: f.git, Session: NewSession(), Timing: Production(),
		Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// askMachineToken asks a data node's server, as the wire carries it, for the
// facts about id under the fleet's key entry keyID and nonce — the answer, and
// its result as it crossed.
func askMachineToken(t *testing.T, node *fakeNode, id, keyID string,
	nonce [nonceBytes]byte) (reply, TokenFacts) {

	t.Helper()
	args, err := json.Marshal(machineTokenArgs{ID: id, Nonce: nonce, KeyID: keyID})
	if err != nil {
		t.Fatal(err)
	}
	srv := server{self: node.name, local: node, env: fleetEnvelopes, now: time.Now,
		seen: signedask.NewGuard()}
	req, sealed := signed(t, fleetEnvelopes, request{Op: opIdentityMachineToken.spec.name,
		Args: args})
	rep, why, err := fleetEnvelopes.openReply(srv.answer(t.Context(), laneShared, sealed), req.ID)
	if err != nil || why != "" {
		t.Fatalf("the answer was not believed: %q, %v", why, err)
	}
	var facts TokenFacts
	if len(rep.Result) > 0 {
		if err := json.Unmarshal(rep.Result, &facts); err != nil {
			t.Fatalf("decode the facts: %v", err)
		}
	}
	return rep, facts
}

// NO VERIFIER CROSSES THE BROKER, AND NO SECRET EITHER — and the token still
// resolves.
//
// A node holding no identity rows asks a data node for the row a presented
// machine token is checked against. Every byte an observer of the broker sees
// of that question and its answer holds neither the token's secret, which
// authenticates it, nor the stored verifier, which a guess is tested against.
// Yet the row the asker gets back carries the stored verifier, because the MAC
// that crossed in its place confirms the presented secret implies it — so
// credential.CheckToken decides the token good exactly as a data node would.
// And a wrong secret gets no verifier and is refused, again exactly as a data
// node refuses it. Mutation: answer the row unnarrowed (factsOf keeping the
// verifier) and the tap sees it; ask with the secret and the tap sees that;
// set the verifier whatever the MAC says and the wrong secret is let in.
func TestNoVerifierCrossesTheBroker(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	f.nodes["data-a"].set(func(n *fakeNode) { n.directory = directory{} })
	tap := &tappedBroker{q: f.start(t)}
	router := f.routerOver(t, "git-1", tap)
	stored := liveToken()

	presented := presentedToken()
	row, err := router.Identity().MachineToken(t.Context(), presented)
	if err != nil {
		t.Fatalf("machine token: %v", err)
	}
	if row.Verifier != stored.Verifier {
		t.Fatalf("the row came back with verifier %q, want the stored one, which the "+
			"presented secret implies", row.Verifier)
	}
	if check := credential.CheckToken(presented, row, tokenNow, statelog.StallGrace); check.Answer !=
		credential.TokenValid {
		t.Fatalf("a good token checked %s (%s) on a node without the directory, want valid",
			check.Answer, check.Detail)
	}

	// A WRONG SECRET gets no verifier at all, and is refused as a data node
	// refuses it: the secret does not verify.
	wrong := presented
	wrong.Secret = strings.Repeat("9f", credential.TokenBytes)
	row, err = router.Identity().MachineToken(t.Context(), wrong)
	if err != nil {
		t.Fatalf("machine token: %v", err)
	}
	if row.Verifier != "" {
		t.Fatalf("a wrong secret's row came back with verifier %q, want none", row.Verifier)
	}
	if check := credential.CheckToken(wrong, row, tokenNow, statelog.StallGrace); check.Answer !=
		credential.TokenRefused {
		t.Fatalf("a wrong secret checked %s (%s), want refused", check.Answer, check.Detail)
	}

	carried := tap.messages()
	if len(carried) < 4 {
		t.Fatalf("the tap carried %d messages, want both questions and both answers", len(carried))
	}
	for i, raw := range carried {
		for what, value := range map[string]string{
			"the presented secret": presented.Secret,
			"the wrong secret":     wrong.Secret,
			"the stored verifier":  stored.Verifier,
		} {
			if bytes.Contains(raw, []byte(value)) {
				t.Errorf("message %d on the broker carries %s", i, what)
			}
		}
	}
}

// tokenFieldCrossing is whether one fact a machine token is checked against
// crosses to a node holding no identity rows, and why.
type tokenFieldCrossing struct {
	crosses bool
	why     string
}

// tokenRowFields is every fact a machine token is checked against, and whether
// it crosses.
//
// TWO-SIDED, like [carriedByServer]: a field of credential.TokenRow or of its
// owner that is not listed fails, so whoever adds one decides whether the
// asking node decides on it or an observer of the broker would want it — and
// a listed field that is gone fails too.
var tokenRowFields = map[string]tokenFieldCrossing{
	"TokenRow.Applied": {true, "an absent row is weighed against the mint's position"},
	"TokenRow.Lag":     {true, "the coverage the table judges first"},
	"TokenRow.Deferred": {true, "the coverage the table judges first, for the owner's " +
		"bucket"},
	"TokenRow.Found":     {true, "whether the id names a credential at all"},
	"TokenRow.IsToken":   {true, "a password's id presented as a token is refused"},
	"TokenRow.ExpiresAt": {true, "an expired token is refused"},
	"TokenRow.RevokedAt": {true, "a revoked token is refused"},
	"TokenRow.Grants":    {true, "what the mint carried, cut to the owner's on every request"},
	"TokenRow.Epoch":     {true, "a token minted before the owner's last revocation is refused"},
	"TokenRow.Generation": {true, "a token minted before the company's last " +
		"invalidation is refused"},
	"TokenRow.FleetGeneration": {true, "the company's invalidation, read with the row"},
	"TokenRow.Owner":           {true, "who the token acts as: the owner's facts below"},
	"TokenRow.Verifier": {false, "what a guess at the secret is tested against: the " +
		"asker has no use for it and an observer would, so a MAC over it and the " +
		"asker's nonce crosses in its place"},

	"TokenOwner.Found":  {true, "a token whose owner is gone is refused"},
	"TokenOwner.ID":     {true, "the principal the token acts as"},
	"TokenOwner.Kind":   {true, "the principal's kind"},
	"TokenOwner.Stage":  {true, "only an active owner may act"},
	"TokenOwner.Login":  {true, "the principal's login"},
	"TokenOwner.Grants": {true, "the owner's current permissions, which cut the mint's"},
	"TokenOwner.Epoch":  {true, "the owner's revocation epoch, compared with the mint's"},
	"TokenOwner.Seat":   {true, "the seat a bound owner acts as, resolved on the asker"},
}

// fullToken is a token row with every fact set, none of them zero.
func fullToken() credential.TokenRow {
	return credential.TokenRow{
		Applied: tokenApplied, Lag: 3 * time.Second, Deferred: true, Found: true, IsToken: true,
		Verifier:   credential.TokenVerifier(machineID, machineSecret),
		ExpiresAt:  tokenNow.Add(24 * time.Hour),
		RevokedAt:  tokenNow.Add(-time.Hour),
		Grants:     []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
		Epoch:      2,
		Generation: 1, FleetGeneration: 4,
		Owner: credential.TokenOwner{
			Found: true, ID: "018f3a9c-0000-7000-8000-00000000a11c", Kind: iam.KindPerson,
			Stage: iam.StageSuspended, Login: "sarah.chen", Grants: []iam.Grant{iam.GrantAdmin},
			Epoch: 3, Seat: "platform-lead",
		},
	}
}

// EVERY FACT A TOKEN IS CHECKED ON CROSSES, BUT ITS VERIFIER.
//
// The asking node decides the token by the one table a data node decides it by,
// so every fact that table reads has to arrive — a fact that silently did not
// would be its zero value, and a zero revocation or a zero owner epoch is a
// token the asker lets in that a data node refuses. So each field of the row
// and of its owner is classified, both ways; a row with every one of them set
// is answered through a serving node; and every field that crosses arrives
// with its value while the verifier arrives empty, and is nowhere in the
// answer's bytes. Mutation: narrow a field the table reads (factsOf zeroing
// the owner's epoch) and this fails; let the verifier through and it fails.
func TestEveryFactATokenIsCheckedOnCrossesButItsVerifier(t *testing.T) {
	t.Parallel()
	found := map[string]bool{}
	for _, ty := range []reflect.Type{reflect.TypeFor[credential.TokenRow](),
		reflect.TypeFor[credential.TokenOwner]()} {
		for i := range ty.NumField() {
			found[ty.Name()+"."+ty.Field(i).Name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(found)) {
		if _, listed := tokenRowFields[name]; !listed {
			t.Errorf("%s is a fact a machine token is checked on: say in tokenRowFields "+
				"whether it crosses to a node holding no identity rows, and why", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(tokenRowFields)) {
		if !found[name] {
			t.Errorf("tokenRowFields lists %s, which is no fact a token is checked on any more", name)
		}
	}

	// EVERY FACT SET, so a field that does not cross cannot hide behind its
	// zero value.
	full := fullToken()
	for _, v := range []reflect.Value{reflect.ValueOf(full), reflect.ValueOf(full.Owner)} {
		for i := range v.NumField() {
			if v.Field(i).IsZero() {
				t.Fatalf("fullToken leaves %s.%s zero", v.Type().Name(), v.Type().Field(i).Name)
			}
		}
	}
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.set(func(n *fakeNode) { n.directory = directory{token: &full} })
	rep, facts := askMachineToken(t, node, machineID, fleetRing.ActiveID, [nonceBytes]byte{7})
	if rep.Err != nil || rep.Unserved != "" {
		t.Fatalf("the token was not answered: %+v", rep)
	}
	if bytes.Contains(rep.Result, []byte(full.Verifier)) {
		t.Errorf("the answer carries the verifier: %s", rep.Result)
	}
	if len(facts.VerifierMAC) == 0 {
		t.Error("the answer carries no MAC in the verifier's place")
	}
	want := full
	for name, crossing := range tokenRowFields {
		if crossing.crosses {
			continue
		}
		owner, field, _ := strings.Cut(name, ".")
		target := reflect.ValueOf(&want).Elem()
		if owner == "TokenOwner" {
			target = target.FieldByName("Owner")
		}
		withheld := target.FieldByName(field)
		withheld.Set(reflect.Zero(withheld.Type()))
	}
	if !reflect.DeepEqual(facts.Row, want) {
		t.Errorf("the row arrived as\n%+v\nwant every fact but the withheld ones\n%+v",
			facts.Row, want)
	}
}

// replayingBroker forwards the first question to the fleet and keeps its
// answer, then answers every later question with that answer — sealed by the
// fleet and bound to the question it answers, as a fleet member replaying a
// cached answer would: the one sender the envelope's own binding cannot stop.
type replayingBroker struct {
	q *memory.Queue

	mu   sync.Mutex
	kept json.RawMessage
}

func (b *replayingBroker) Ask(ctx context.Context, subject string, request []byte,
	want int) ([][]byte, error) {

	asked := opened(request)
	b.mu.Lock()
	kept := b.kept
	b.mu.Unlock()
	if kept != nil {
		return [][]byte{fleetEnvelopes.sealReply(reply{Node: "data-a", Request: asked.ID,
			Result: kept})}, nil
	}
	replies, err := b.q.Ask(ctx, subject, request, want)
	if err != nil || len(replies) == 0 {
		return replies, err
	}
	rep, why, err := fleetEnvelopes.openReply(replies[0], asked.ID)
	if err != nil || why != "" {
		return nil, errors.Join(err, errors.New(why))
	}
	b.mu.Lock()
	b.kept = rep.Result
	b.mu.Unlock()
	return replies, nil
}

// A REPLAYED TOKEN ANSWER FAILS ANOTHER NONCE.
//
// Every machine-token question carries a nonce of its own, and the MAC that
// stands in for the verifier is over it: so an answer to one question — even
// one re-sealed by a fleet member and bound to a later question, which the
// envelope alone would believe — confirms no verifier for that later
// question, and the token is refused rather than decided on facts about
// somebody else's question. And at the facts themselves: the same answer
// checked under its own nonce confirms the verifier, and under any other, or
// under another key, confirms nothing. Mutation: leave the nonce out of the
// MAC and the replay is let in.
func TestAReplayedTokenAnswerFailsAnotherNonce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	node.set(func(n *fakeNode) { n.directory = directory{} })
	presented := presentedToken()
	stored := liveToken()

	replay := &replayingBroker{q: f.start(t)}
	router := f.routerOver(t, "git-1", replay)
	first, err := router.Identity().MachineToken(t.Context(), presented)
	if err != nil || first.Verifier != stored.Verifier {
		t.Fatalf("the first question was answered (verifier %q, %v), want the stored "+
			"verifier confirmed", first.Verifier, err)
	}
	second, err := router.Identity().MachineToken(t.Context(), presented)
	if err != nil {
		t.Fatalf("the replayed answer: %v", err)
	}
	if second.Verifier != "" {
		t.Fatalf("a replayed answer confirmed verifier %q for a question it does not answer",
			second.Verifier)
	}
	if check := credential.CheckToken(presented, second, tokenNow, statelog.StallGrace); check.Answer !=
		credential.TokenRefused {
		t.Fatalf("a token decided on a replayed answer checked %s, want refused", check.Answer)
	}

	// AT THE FACTS: confirmed under the nonce they were MAC'd over, and under
	// no other nonce or key.
	asked := [nonceBytes]byte{1, 2, 3}
	_, facts := askMachineToken(t, node, machineID, fleetRing.ActiveID, asked)
	key, held := fleetEnvelopes.verifiers.key(fleetRing.ActiveID)
	if !held {
		t.Fatal("the fleet's envelopes hold no key for its active entry")
	}
	if got := facts.rowFor(presented, key, asked).Verifier; got != stored.Verifier {
		t.Fatalf("the facts under their own nonce confirmed %q, want the stored verifier", got)
	}
	other := asked
	other[0] ^= 1
	if got := facts.rowFor(presented, key, other).Verifier; got != "" {
		t.Errorf("the facts under another nonce confirmed %q", got)
	}
	otherKey := newVerifierKeys(statelog.OneKey("k2", "another fleet's key material"))
	if alien, _ := otherKey.key("k2"); facts.rowFor(presented, alien, asked).Verifier != "" {
		t.Error("the facts under another key confirmed a verifier")
	}
}

// A MACHINE TOKEN NOBODY CAN ANSWER FOR IS UNKNOWN, NEVER "NO SUCH TOKEN": a
// directory that cannot be read crosses as its own error, and a data node
// holding no directory leaves the estate unserved — each an error, which the
// guard answers 503, where the zero row would be a pipeline told its credential
// is broken. A node asked to answer under a key it does not hold runs nothing
// and reads nothing, so the router asks one that holds it. And a data node's
// own router answers it in-process, MAC'd under its own envelopes' keys.
func TestAMachineTokenNobodyCanAnswerForIsUnknown(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	git := f.routerOver(t, "git-1", f.start(t))

	node.set(func(n *fakeNode) { n.directory = directory{err: store.ErrNoEstate} })
	row, err := git.Identity().MachineToken(t.Context(), presentedToken())
	if !errors.Is(err, store.ErrNoEstate) || !reflect.DeepEqual(row, credential.TokenRow{}) {
		t.Fatalf("an unreadable directory = (%+v, %v), want its error and no row", row, err)
	}

	node.set(func(n *fakeNode) { n.directory = nil })
	_, err = git.Identity().MachineToken(t.Context(), presentedToken())
	var unserved *ErrUnserved
	if !errors.As(err, &unserved) || !strings.Contains(unserved.Detail, "no native backend") {
		t.Fatalf("a node with no directory = %v, want the estate unserved", err)
	}

	var reads atomic.Int64
	node.set(func(n *fakeNode) { n.directory = directory{tokenReads: &reads} })
	rep, _ := askMachineToken(t, node, machineID, "k9", [nonceBytes]byte{})
	if rep.Unserved != unservedNoBackend || !strings.Contains(rep.Detail, `"k9"`) {
		t.Fatalf("a key the node does not hold = %+v, want it run nothing, naming the key", rep)
	}
	if n := reads.Load(); n != 0 {
		t.Errorf("a node that could not MAC the answer read the row %d times", n)
	}

	local := f.router(t, "data-a", node)
	row, err = local.Identity().MachineToken(t.Context(), presentedToken())
	if err != nil || row.Verifier != liveToken().Verifier {
		t.Fatalf("a data node's own router answered (verifier %q, %v), want the stored "+
			"verifier confirmed in-process", row.Verifier, err)
	}
}
