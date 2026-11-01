package signedask

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// ask and answer are a channel's request and reply as a test spells them: an
// instruction, bound the way every channel binds one.
type ask struct {
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
	Do       string    `json:"do"`
}

func (a ask) Binding() (string, time.Time) { return a.ID, a.Deadline }

type answer struct {
	Request string `json:"request,omitempty"`
	Did     string `json:"did,omitempty"`
}

func (a answer) Answers() string { return a.Request }

// fleetRing is the keyring every node of a test fleet holds.
var fleetRing = statelog.OneKey("k1", "the signed-ask suite's fleet key material")

func channel(t *testing.T, name string, ring statelog.Keyring) *Channel[ask, answer] {
	t.Helper()
	c, err := New[ask, answer](name, ring)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sealed(t *testing.T, c *Channel[ask, answer], req ask) []byte {
	t.Helper()
	out, err := c.SealRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var (
	instant = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	removal = ask{ID: "r-1", Deadline: instant.Add(time.Minute), Do: "remove node-c"}
)

// A REQUEST THE FLEET SIGNED IS CARRIED OUT ONCE: admitted, decoded whole and
// bound to its id — and the same bytes arriving again are a replay, refused
// however the first copy was answered.
func TestARequestTheFleetSignedIsAdmittedOnce(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	guard := NewGuard()
	sent := sealed(t, c, removal)

	got := c.Admit(sent, guard, instant)
	if got.Disposition != Admitted || got.Request != removal || got.ID != removal.ID || got.Why != "" {
		t.Fatalf("a request the fleet signed = %+v, want it admitted as sent", got)
	}
	again := c.Admit(sent, guard, instant)
	if again.Disposition != Stale || again.ID != removal.ID || !strings.Contains(again.Why, "replay") {
		t.Fatalf("the same request again = %+v, want it refused as a replay", again)
	}
	// A SCATTER HANDS ONE REQUEST TO EVERY NODE, and each takes it once.
	if other := c.Admit(sent, NewGuard(), instant); other.Disposition != Admitted {
		t.Fatalf("another node's first sight of the request = %+v, want it admitted", other)
	}
}

// A REQUEST THE FLEET DID NOT SIGN IS BELIEVED IN NOTHING — not even the id a
// reply would name. Three ways to try: no signature at all, a signature under
// the fleet's key id with somebody else's material, and the fleet's own
// signature over bytes altered after it was made.
func TestARequestTheFleetDidNotSignIsBelievedInNothing(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	unsigned, err := json.Marshal(removal)
	if err != nil {
		t.Fatal(err)
	}
	outsider := sealed(t, channel(t, "test.channel",
		statelog.OneKey("k1", "not the fleet's material")), removal)
	altered := bytes.Replace(sealed(t, c, removal), []byte("node-c"), []byte("node-a"), 1)
	if !bytes.Contains(altered, []byte("node-a")) {
		t.Fatal("the case did not alter the signed request")
	}
	for name, sent := range map[string][]byte{
		"unsigned":                         unsigned,
		"signed under somebody else's key": outsider,
		"altered after it was signed":      altered,
	} {
		got := c.Admit(sent, NewGuard(), instant)
		if got.Disposition != Tampered || got.ID != "" || got.Request != (ask{}) || got.Why == "" {
			t.Errorf("%s = %+v, want it tampered with nothing in it believed", name, got)
		}
	}
}

// A REQUEST UNDER A KEY THIS NODE DOES NOT HOLD IS A FACT ABOUT THIS NODE: it
// is not admitted, and the id it claims is read — untrusted — so the answer
// that it did not run can be bound to the asker's question and the asker moves
// on to a node that holds the key.
func TestARequestUnderAKeyThisNodeDoesNotHoldNamesItsIDUntrusted(t *testing.T) {
	t.Parallel()
	rotated := statelog.Keyring{ActiveID: "k2", Keys: []statelog.Key{
		fleetRing.Keys[0], {ID: "k2", Material: "the key the fleet is rotating to"},
	}}
	ahead := channel(t, "test.channel", rotated)
	behind := channel(t, "test.channel", fleetRing)
	got := behind.Admit(sealed(t, ahead, removal), NewGuard(), instant)
	if got.Disposition != KeyUnknown || got.ID != removal.ID || got.Request != removal {
		t.Fatalf("a request under an unheld key = %+v, want it refused naming its id", got)
	}
	if !strings.Contains(got.Why, `it holds "k1"`) || !strings.Contains(got.Why, "secrets.keys") {
		t.Errorf("the refusal does not say what this node holds or what to do: %q", got.Why)
	}
}

// A REQUEST NOBODY IS WAITING FOR RUNS NOTHING: one whose deadline had passed
// when it arrived, and one that names no deadline or no id — none of which a
// fleet member sends ([Channel.SealRequest] refuses them), and each of which is
// how a replay would outlive the window a node remembers ids for.
func TestARequestNobodyIsWaitingForIsStale(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	for name, req := range map[string]ask{
		"past its deadline": {ID: "r-late", Deadline: instant.Add(-time.Second), Do: removal.Do},
		"at its deadline":   {ID: "r-now", Deadline: instant, Do: removal.Do},
		"no deadline":       {ID: "r-open", Do: removal.Do},
		"no id":             {Deadline: instant.Add(time.Minute), Do: removal.Do},
	} {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		got := c.Admit(c.requests.Seal(raw), NewGuard(), instant)
		if got.Disposition != Stale || got.ID != req.ID || got.Why == "" {
			t.Errorf("%s = %+v, want it stale and bound to %q", name, got, req.ID)
		}
	}
	for _, req := range []ask{{ID: "r-open"}, {Deadline: instant}} {
		if _, err := c.SealRequest(req); err == nil {
			t.Errorf("a request with no id or no deadline was sealed: %+v", req)
		}
	}
}

// A REQUEST THE FLEET SIGNED THAT THIS BUILD CANNOT READ RUNS NOTHING, and no
// reply can be bound to it.
func TestARequestThisBuildCannotReadIsUndecodable(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	got := c.Admit(c.requests.Seal([]byte(`{"id":7}`)), NewGuard(), instant)
	if got.Disposition != Undecodable || got.ID != "" || got.Why == "" {
		t.Fatalf("an undecodable request = %+v", got)
	}
}

// A NODE WITHOUT A GUARD REFUSES EVERY REQUEST rather than taking one whose
// replay it could not tell from it.
func TestANodeWithoutAGuardTakesNothing(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	if got := c.Admit(sealed(t, c, removal), nil, instant); got.Disposition != Stale {
		t.Fatalf("a request admitted with no guard = %+v", got)
	}
}

// A REPLY IS BELIEVED ONLY WHERE IT IS THE FLEET'S ANSWER TO THIS QUESTION:
// one not signed by the fleet, one the fleet signed for another request, one
// altered after it was signed, one under a key this node does not hold and one
// read against no question at all are each no answer.
func TestAReplyIsBelievedOnlyWhenBoundToTheQuestion(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	reply := func(c *Channel[ask, answer], rep answer) []byte {
		out, err := c.SealReply(rep)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	rep, why, err := c.OpenReply(reply(c, answer{Request: "r-1", Did: "removed"}), "r-1")
	if err != nil || why != "" || rep.Did != "removed" {
		t.Fatalf("the fleet's answer to this request = (%+v, %q, %v), want it believed", rep, why, err)
	}
	rotated := statelog.Keyring{ActiveID: "k2", Keys: []statelog.Key{
		{ID: "k2", Material: "a key this node has not been given"},
	}}
	for name, tc := range map[string]struct {
		sent  []byte
		asked string
	}{
		"not signed by the fleet": {reply(channel(t, "test.channel",
			statelog.OneKey("k1", "not the fleet's material")), answer{Request: "r-1"}), "r-1"},
		"the fleet's answer to another request": {reply(c, answer{Request: "r-2"}), "r-1"},
		"altered after it was signed": {bytes.Replace(reply(c, answer{Request: "r-1",
			Did: "refused"}), []byte("refused"), []byte("removed"), 1), "r-1"},
		"under a key this node does not hold": {reply(channel(t, "test.channel", rotated),
			answer{Request: "r-1"}), "r-1"},
		"an unbound refusal read against no question": {reply(c, answer{}), ""},
	} {
		got, why, err := c.OpenReply(tc.sent, tc.asked)
		if err != nil || why == "" || got != (answer{}) {
			t.Errorf("%s = (%+v, %q, %v), want it not believed", name, got, why, err)
		}
	}
	if _, _, err := c.OpenReply(c.replies.Seal([]byte(`{"request":7}`)), "r-1"); err == nil {
		t.Error("a reply the fleet signed that this build cannot read was believed")
	}
}

// THE HALVES NEVER VERIFY AS EACH OTHER, NOR ACROSS CHANNELS, NOR AS A
// STATE-LOG RECORD: the domain is in the key and in the MAC, so a reply
// captured off the wire cannot be sent back as a request, one channel's
// request is nothing on another's subject, and no record of any domain's log
// is an envelope.
func TestEveryChannelSignsInDomainsOfItsOwn(t *testing.T) {
	t.Parallel()
	c := channel(t, "test.channel", fleetRing)
	other := channel(t, "other.channel", fleetRing)
	if req, rep := c.Domains(); req != "test.channel.request" || rep != "test.channel.reply" {
		t.Fatalf("domains (%q, %q)", req, rep)
	}
	record, err := statelog.NewSigner("tracker", fleetRing)
	if err != nil {
		t.Fatal(err)
	}
	asReply, err := c.SealReply(answer{Request: removal.ID})
	if err != nil {
		t.Fatal(err)
	}
	for name, sent := range map[string][]byte{
		"this channel's reply":      asReply,
		"another channel's request": sealed(t, other, removal),
		"a state-log record":        record.Seal([]byte(`{"id":"r-1"}`)),
	} {
		if got := c.Admit(sent, NewGuard(), instant); got.Disposition != Tampered {
			t.Errorf("%s admitted as a request: %+v", name, got)
		}
	}
	if _, why, _ := c.OpenReply(sealed(t, c, removal), removal.ID); why == "" {
		t.Error("a request opened as a reply")
	}
}

// A NODE THAT COULD NEITHER SIGN NOR VERIFY IS NOT BUILT: a channel with no
// name, and one over a keyring that names no active key, are refused where
// they are built rather than one request at a time on every peer.
func TestAChannelThatCannotSignIsNotBuilt(t *testing.T) {
	t.Parallel()
	if _, err := New[ask, answer]("", fleetRing); err == nil {
		t.Error("a channel with no name was built")
	}
	if _, err := New[ask, answer]("test.channel", statelog.Keyring{}); !errors.Is(err, statelog.ErrUnsigned) {
		t.Errorf("a channel over an empty keyring = %v, want %v", err, statelog.ErrUnsigned)
	}
}

// THE GUARD FORGETS AN ID ONLY ONCE ITS DEADLINE HAS PASSED — when the deadline
// refuses it anyway — so what it holds is bounded by the requests still in
// flight, and a replay is refused for as long as it could be taken.
func TestTheGuardForgetsAnIDOnlyAtItsDeadline(t *testing.T) {
	t.Parallel()
	g := NewGuard()
	now := instant
	if g.take("r-1", now.Add(time.Minute), now) != "" || g.take("r-1", now.Add(time.Minute), now) == "" {
		t.Fatal("an id was not taken once and refused after")
	}
	// SHORT-LIVED REQUESTS fill the guard to its floor; the next admission
	// after their deadlines sweeps them out and keeps what is in flight.
	for i := range sweepFloor - 2 {
		g.take("filler-"+strconv.Itoa(i), now.Add(time.Second), now)
	}
	later := now.Add(30 * time.Second)
	g.take("r-2", later.Add(time.Minute), later)
	g.take("r-3", later.Add(time.Minute), later)
	g.mu.Lock()
	held := len(g.seen)
	_, stillHeld := g.seen["r-1"]
	g.mu.Unlock()
	if held != 3 || !stillHeld {
		t.Fatalf("after a sweep the guard holds %d ids (r-1 held: %v), want the three still "+
			"in flight", held, stillHeld)
	}
	if g.take("r-1", now.Add(time.Minute), later) == "" {
		t.Fatal("an id still in flight was forgotten by the sweep")
	}
}
