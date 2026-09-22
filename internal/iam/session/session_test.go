package session_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/google/uuid"
)

// --- the rig ----------------------------------------------------------------- //

// signedIn is one live session, everything about it consistent, at a clock the
// test moves.
type signedIn struct {
	t       *testing.T
	signer  *session.Signer
	clock   *clock
	lineage uuid.UUID
	cookie  string
	person  string
	dir     *directory
	chart   *chartView
}

const (
	personID = "018f3a9c-0000-7000-8000-0000000000aa"
	startPos = 4096
	absolute = 8 * time.Hour
)

// keyring is a two-key keyring, so the rotation arms are exercised by material
// that actually carries two rather than by one key wearing two names.
func keyring() runtoken.Material {
	return runtoken.Material{
		ActiveID: "k2",
		Keys: []runtoken.KeyMaterial{
			{ID: "k1", Material: "the-previous-key-material"},
			{ID: "k2", Material: "the-active-key-material"},
		},
	}
}

func newSignedIn(t *testing.T) *signedIn {
	t.Helper()
	// A FIXED INSTANT, so a rotation boundary is a value the test names
	// rather than something it has to wait for.
	c := &clock{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	signer, err := session.New(session.Options{
		Material: keyring(), RotateAfter: time.Hour, Now: c.now,
	})
	if err != nil {
		t.Fatalf("build a signer: %v", err)
	}
	// THE LINEAGE IS THE CLOCK. A uuid7 minted at the rig's own instant is
	// what makes "this session is 90 minutes old" a fact the test states.
	lineage := lineageAt(t, c.at)
	cookie, err := signer.Mint(session.Mint{
		Lineage: lineage, Person: personID, Epoch: 3, Generation: 1,
		StartPosition: startPos, AbsoluteExpiresAt: c.at.Add(absolute),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return &signedIn{
		t: t, signer: signer, clock: c, lineage: lineage, cookie: cookie,
		person: personID,
		dir: &directory{identity: session.Identity{
			Applied:    startPos,
			Generation: 1,
			Session:    session.SessionRow{Found: true, Epoch: 3},
			Person: session.PersonRow{
				Found: true, Epoch: 3, Stage: iam.StageActive,
				Login: "sarah.chen", Seat: "platform-lead", SeatAt: 900,
			},
		}},
		chart: &chartView{
			position: 1000,
			seats: map[string]session.Seat{
				"platform-lead": {Handle: "platform-lead", Kind: "human"},
			},
		},
	}
}

func (s *signedIn) validate() session.Validation {
	s.t.Helper()
	return s.signer.Validate(s.t.Context(), s.dir, s.cookie)
}

// lineageAt mints a uuid7 whose embedded instant is at, by hand: the library
// reads the clock, and a test that needs a session to be exactly ninety
// minutes old cannot wait ninety minutes.
func lineageAt(t *testing.T, at time.Time) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("mint a lineage: %v", err)
	}
	millis := at.UnixMilli()
	for i := range 6 {
		id[i] = byte(millis >> (8 * (5 - i)))
	}
	return id
}

type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

type directory struct {
	identity session.Identity
	err      error
	calls    int
	mu       sync.Mutex
}

func (d *directory) Resolve(context.Context, string, string) (session.Identity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.err != nil {
		return session.Identity{}, d.err
	}
	return d.identity, nil
}

type chartView struct {
	position uint64
	lag      time.Duration
	seats    map[string]session.Seat
	err      error
}

func (c *chartView) Seat(_ context.Context, ref string) (session.Seat, bool, error) {
	if c.err != nil {
		return session.Seat{}, false, c.err
	}
	seat, found := c.seats[ref]
	return seat, found, nil
}

func (c *chartView) Position(context.Context) (uint64, time.Duration, error) {
	if c.err != nil {
		return 0, 0, c.err
	}
	return c.position, c.lag, nil
}

// --- the keyring ------------------------------------------------------------- //

// A DEPLOYMENT WITH NO FLEET KEYRING IS REFUSED, NEVER GIVEN A PER-PROCESS KEY.
//
// internal/runtoken falls back to a random key per process, which is correct
// for an endpoint only that process verifies. Here it is catastrophic: every
// ingress node would derive a different key and reject every other node's
// cookies, so a browser would be signed in on whichever node its request
// happened to reach and signed out on the next one — with nothing in the
// config looking wrong.
func TestANilKeyIsRefused(t *testing.T) {
	t.Parallel()
	for name, material := range map[string]runtoken.Material{
		"an empty keyring": {},
		"an active id naming nothing": {
			ActiveID: "k9",
			Keys:     []runtoken.KeyMaterial{{ID: "k1", Material: "m"}},
		},
		"keys with no active id": {
			Keys: []runtoken.KeyMaterial{{ID: "k1", Material: "m"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			signer, err := session.New(session.Options{Material: material})
			if !errors.Is(err, session.ErrNoKeyring) {
				t.Fatalf("New returned (%v, %v), want %v", signer, err,
					session.ErrNoKeyring)
			}
			if signer != nil {
				t.Error("a signer was returned beside the refusal, so a caller " +
					"that logged the error and carried on would mint cookies " +
					"no peer can read")
			}
		})
	}
}

// THE SIGNING DOMAIN IS WHAT STOPS ONE CREDENTIAL VALIDATING AS ANOTHER.
//
// A per-run token and a session bearer are HMACs over the SAME fleet keyring,
// so without a domain in the derivation a cookie would authenticate at the
// telemetry receiver a sandbox can reach, and a token a sandbox holds would
// authenticate as somebody's browser session.
func TestASessionBearerDoesNotValidateAtTheOtlpReceiver(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)

	// The receiver's own signer, over the same keyring.
	receiver := runtoken.New(runtoken.Options{
		Domain: otlpDomain, Material: keyring(),
	})
	if subject := receiver.Validate(rig.cookie); subject != "" {
		t.Errorf("the session cookie validated at the telemetry receiver as %q",
			subject)
	}

	// And the other direction: the receiver's token in the cookie slot.
	token := receiver.Mint("trace-1", time.Hour)
	if got := rig.signer.Validate(t.Context(), rig.dir, token); got.Row !=
		session.RowMalformed {
		t.Errorf("a per-run token presented as a cookie landed on %q, want %q",
			got.Row, session.RowMalformed)
	}

	// THE TWO CHECKS ABOVE WOULD PASS ON A SHARED DOMAIN, because the two
	// formats differ — so they are not what this case is about. THIS is:
	// a bearer of this package's own shape, signed under the SAME keyring
	// entry but through the receiver's domain, which is exactly what an
	// endpoint that shared the domain would produce.
	forged := resignForDomain(t, rig.cookie, otlpDomain, "k2",
		"the-active-key-material")
	if got := rig.signer.Validate(t.Context(), rig.dir, forged); got.Row !=
		session.RowMalformed {
		t.Errorf("a bearer signed through the telemetry receiver's domain "+
			"landed on %q, want %q — the two endpoints are HMACs over one "+
			"keyring, and without a domain in the derivation each one's "+
			"credential opens the other", got.Row, session.RowMalformed)
	}
}

// otlpDomain is the telemetry receiver's own, as internal/sandbox declares it.
const otlpDomain = "crewlet/otlp/v1"

// resignForDomain rebuilds a bearer's tag and mac under another domain's key,
// leaving every other field alone.
func resignForDomain(t *testing.T, cookie, domain, id, material string) string {
	t.Helper()
	parts := strings.Split(cookie, ".")
	parts[1] = runtoken.KeyTag(domain, id)
	payload := strings.Join(parts[:len(parts)-1], ".")
	mac := hmac.New(sha256.New, runtoken.DeriveKey(domain, id, material))
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// DROPPING A KEY ENDS THE SESSIONS SIGNED UNDER IT, AND NOTHING IS READ BEFORE
// THE MAC VERIFIES.
//
// The first half is the runbook's last step working: an operator who drops a
// key early is told that is what ends live sessions, and it has to actually do
// so. The second is the shape of the whole validate path — the person id and
// the key tag both look like things to look up, and looking either up before
// the mac has verified lets an unauthenticated caller make this node read a
// row of their choosing, for every cookie they care to send.
func TestABearerSignedUnderADroppedKeyIsRefused(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)

	// The same fleet, after the operator dropped k2 and kept k1 active.
	dropped, err := session.New(session.Options{
		Material: runtoken.OneKey("k1", "the-previous-key-material"),
		Now:      rig.clock.now,
	})
	if err != nil {
		t.Fatalf("build the rotated signer: %v", err)
	}
	got := dropped.Validate(t.Context(), rig.dir, rig.cookie)
	if got.Row != session.RowMalformed {
		t.Errorf("a bearer under a dropped key landed on %q, want %q",
			got.Row, session.RowMalformed)
	}
	if rig.dir.calls != 0 {
		t.Errorf("the directory was read %d times for a bearer that never "+
			"verified — an unauthenticated caller can then price a request by "+
			"sending rubbish", rig.dir.calls)
	}
}

// A KEYRING ROTATION LOGS NOBODY OUT, WHICH IS THE WHOLE POINT OF THE TAG.
//
// Add a key, restart, flip active, restart: at every step a node holds both
// keys, so a cookie minted under either verifies — and the re-issue moves the
// session onto the new key the first time it is used.
func TestAddingAKeyLogsNobodyOutAndTheReissueMovesThem(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)

	// The fleet before k2 existed mints the cookie...
	before, err := session.New(session.Options{
		Material: runtoken.OneKey("k1", "the-previous-key-material"),
		Now:      rig.clock.now, RotateAfter: time.Hour,
	})
	if err != nil {
		t.Fatalf("build the old signer: %v", err)
	}
	cookie, err := before.Mint(session.Mint{
		Lineage: rig.lineage, Person: personID, Epoch: 3, Generation: 1,
		StartPosition:     startPos,
		AbsoluteExpiresAt: rig.clock.now().Add(absolute),
	})
	if err != nil {
		t.Fatalf("mint under the old key: %v", err)
	}

	// ...and the fleet that has added k2 and flipped active still serves
	// it, then hands back one under the new key.
	rig.clock.advance(10 * time.Minute)
	got := rig.signer.Validate(t.Context(), rig.dir, cookie)
	if got.Row != session.RowValid {
		t.Fatalf("a cookie from before the rotation landed on %q: %s",
			got.Row, got.Detail)
	}
	if got.Reissue == "" {
		t.Fatal("no re-issue, so the session would stay on the old key until " +
			"the operator dropped it and signed them out")
	}
	next := rig.signer.Validate(t.Context(), rig.dir, got.Reissue)
	if next.Row != session.RowValid {
		t.Errorf("the re-issued cookie landed on %q: %s", next.Row, next.Detail)
	}
	if strings.Split(got.Reissue, ".")[1] == strings.Split(cookie, ".")[1] {
		t.Error("the re-issue was signed under the old key's tag, so a " +
			"rotation would never drain")
	}
}

// --- minting ----------------------------------------------------------------- //

// A LINEAGE THAT IS NOT A UUID7 IS REFUSED AT MINT.
//
// The rotation index is the session's age in windows, and the age is read out
// of the instant inside the lineage. A lineage minted any other way is a
// session whose age nothing can compute — which would silently pin every
// cookie at index 0 for ever.
func TestALineageWithNoInstantInItIsRefused(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	v4, err := uuid.NewRandom()
	if err != nil {
		t.Fatalf("mint a v4: %v", err)
	}
	for name, in := range map[string]session.Mint{
		"a version-4 lineage": {
			Lineage: v4, Person: personID,
			AbsoluteExpiresAt: rig.clock.now().Add(absolute),
		},
		"no lineage at all": {
			Person: personID, AbsoluteExpiresAt: rig.clock.now().Add(absolute),
		},
		"no person": {
			Lineage:           rig.lineage,
			AbsoluteExpiresAt: rig.clock.now().Add(absolute),
		},
		"no absolute deadline": {Lineage: rig.lineage, Person: personID},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if cookie, err := rig.signer.Mint(in); err == nil {
				t.Errorf("minted %q", cookie)
			}
		})
	}
}

// THE COOKIE NAME CARRIES THE `__Host-` PREFIX UNLESS THE DEPLOYMENT IS PLAIN
// HTTP, AND ANYTHING UNREADABLE TAKES THE PREFIX.
//
// The failure directions are not symmetric: a prefixed cookie on a loopback
// http deployment does not stick and somebody notices during setup, while an
// unprefixed one on a public https deployment works perfectly and silently
// accepts a cookie a sibling subdomain wrote.
func TestTheCookieNameDropsThePrefixOnlyForPlainHttp(t *testing.T) {
	t.Parallel()
	for external, want := range map[string]string{
		"https://crewlet.example.com": session.HostCookieName,
		"HTTPS://crewlet.example.com": session.HostCookieName,
		"":                            session.HostCookieName,
		"crewlet.example.com":         session.HostCookieName,
		"://%%bad":                    session.HostCookieName,
		"http://localhost:8000":       session.CookieBaseName,
		"  http://127.0.0.1:8000  ":   session.CookieBaseName,
	} {
		if got := session.CookieName(external); got != want {
			t.Errorf("CookieName(%q) = %q, want %q", external, got, want)
		}
	}
}

// THE COOKIE'S ATTRIBUTES ARE DECIDED IN ONE PLACE, AND EVERY ONE OF THEM IS
// LOAD-BEARING.
//
// They drift silently when they are written at each call site: the sign-in
// still works with `Secure` dropped, with `SameSite=None`, with a `Domain`
// attribute set — and each of those is a different way for somebody else's
// page to act as the person holding the cookie.
func TestTheCookieCarriesEveryAttributeThatMakesItSafe(t *testing.T) {
	t.Parallel()
	expires := time.Date(2026, 3, 2, 17, 0, 0, 0, time.UTC)
	got := session.Cookie("https://crewlet.example.com", "v2.value", expires)
	switch {
	case got.Name != session.HostCookieName:
		t.Errorf("the name is %q", got.Name)
	case !got.HttpOnly:
		t.Error("not HttpOnly, so any script on the page can read the session")
	case !got.Secure:
		t.Error("not Secure, which a browser requires for the __Host- prefix " +
			"and which otherwise sends the cookie over plain http")
	case got.Path != "/":
		t.Errorf("the path is %q, which the __Host- prefix refuses and which "+
			"leaves part of the API without a credential", got.Path)
	case got.Domain != "":
		t.Errorf("a Domain of %q is set, which the __Host- prefix refuses — "+
			"it is what would let a sibling subdomain write this cookie",
			got.Domain)
	case got.SameSite != http.SameSiteLaxMode:
		t.Errorf("SameSite is %v, so a cross-site form can post as the person "+
			"holding it", got.SameSite)
	case !got.Expires.Equal(expires):
		t.Errorf("the expiry is %v, want the absolute deadline %v — the idle "+
			"one signs somebody out at the moment a re-issue would have moved "+
			"it, and none at all leaves a dead cookie re-presented for ever",
			got.Expires, expires)
	}

	// A loopback http deployment drops the prefix AND the Secure flag,
	// because a browser refuses the Set-Cookie with either one alone.
	local := session.Cookie("http://localhost:8000", "v2.value", expires)
	if local.Name != session.CookieBaseName || local.Secure {
		t.Errorf("the loopback cookie is %q (secure %v), want %q and not secure",
			local.Name, local.Secure, session.CookieBaseName)
	}

	// And the clear matches on name, path and domain, or the browser
	// keeps the original and the person stays signed in.
	clear := session.Clear("https://crewlet.example.com")
	if clear.Name != got.Name || clear.Path != got.Path ||
		clear.Domain != got.Domain || clear.MaxAge >= 0 {
		t.Errorf("the clear is %+v, which does not match the cookie it is "+
			"meant to delete", clear)
	}
}
