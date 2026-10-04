package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A PERSON'S SESSION IS THE FLEET'S, NOT THE NODE'S THAT MINTED IT.
//
// # Why this is a fleet case and nothing else can be
//
// A browser behind a load balancer reaches whichever member it is sent to, so
// the cookie a sign-in minted on one node is presented to every other. What
// makes that work is two things no single node exercises: every member signs
// and verifies under ONE keyring, and every member resolves the cookie against
// its OWN copy of the identity estate, which reaches it only through the
// identity log. Each half has unit tests against stubs — the session table's
// behind-node arm, the guard's origin rule, the socket's cookie handshake —
// and none of them could see a fleet where a cookie minted on one node was
// refused on the next, or a sign-out one node never heard: every fixture here
// presented a Tier A bearer to a node serving no sign-in surface at all, so a
// total regression of browser identity across a fleet left this suite green.
//
// # What it asserts, in the order a person meets it
//
//   - A Tier A token invites a person to the company's human seat; they redeem
//     the link and sign in with a password on one member.
//   - THE SAME COOKIE reads on the OTHER member, which learns the session only
//     from the log — waiting or answering 503 while it catches up, and never
//     401, which a browser reads as "signed out" and throws the cookie away.
//   - A cookie-authenticated WRITE on the other member lands AUTHORED BY THE
//     PERSON: as their seat, of the human kind, through their session.
//   - The live socket opens on the other member with the cookie and the
//     deployment's Origin, and delivers its snapshot; the same handshake from
//     a foreign Origin is refused by the cross-site rule.
//   - Signing out everywhere on the other member reaches back: the old cookie
//     is refused 401 on the member that minted it, once that member applies
//     the revocation, inside the bound a record takes to cross the fleet.
//
// # One cluster, three phases
//
// A fleet start is the costly part of every case here, and each phase is the
// state the next one starts from — the session the write is authored through,
// the socket opened on it, the revocation that ends it — so they share one
// start and stop at the first phase that fails, since every later one would
// only report the same failure again.
//
// TWO MEMBERS, which is [fleetSize] and enough: the session crosses from the
// member that minted it to the other, and the revocation crosses back the
// opposite way, so each member learns one of the two facts only from the log.
//
// Mutations, each run: record a bound person's writes as an operator's and the
// write phase names the kind it landed as; drop the handshake's origin check
// and the foreign dial opens; stop a person's revocation epoch ending their
// sessions and the copied cookie is served for ever on the member that minted
// it; stop the engine handing the identity applier's moves to the API and the
// tab left open on that member never closes.
func TestASessionCrossesTheFleet(t *testing.T) {
	noParallel(t)
	c := startCluster(t, fleetSize)
	c.hydrated(t)
	minted, other := c.nodes[0], c.nodes[1]

	jane := newBrowser(t)
	if !t.Run("signs in on one member and is served on the other", func(t *testing.T) {
		signIn(t, minted, jane)
		servedElsewhere(t, other, jane)
		writesAsThePerson(t, other, jane)
	}) {
		return
	}
	if !t.Run("the socket opens on the other member for this deployment only", func(t *testing.T) {
		socketOpens(t, other, jane)
	}) {
		return
	}
	t.Run("signing out everywhere reaches the member that minted the session", func(t *testing.T) {
		revokedEverywhere(t, minted, other, jane)
	})
}

// deploymentURL is where a browser reaches the fleet this suite stands up: one
// address for every member, as behind a load balancer.
//
// HTTPS, because that is every real deployment's shape — the engine behind a
// TLS-terminating proxy — and it is what decides the session cookie's name
// (`__Host-` prefixed) and the Origin every state change must carry. A test on
// plain http would exercise the name no production browser holds.
const deploymentURL = "https://crewlet.example.com"

// withSignIn gives a fleet member the deployment's address and a password
// sign-in, which is what a person needs to become a principal on it.
//
// A SECOND FACTOR IS OPTIONAL HERE, and said out loud. What this suite proves
// is that a session crosses the fleet; a required factor would put an
// enrolment in front of every case and prove nothing more about the crossing,
// and the factor's own flow is internal/api/authapi's subject. A deployment a
// browser reaches at a routable address has to acknowledge that posture to
// serve it, and so does this one, which is what keeps the file a
// configuration `crewlet validate` would accept with a port set.
func withSignIn(boot *config.Bootstrap) {
	boot.API.ExternalURL = deploymentURL
	boot.API.Auth.Backend = config.AuthBackendLocal
	boot.API.Auth.Local = &config.APILocal{
		TOTP: iam.SecondFactorOptional, AcceptInsecure: true,
	}
}

// Who signs in, and what they are given.
const (
	janeAddress  = "jane@example.com"
	janeLogin    = "jane.doe"
	janePassword = "a-perfectly-fine-passphrase"

	// janeSeat is the company document's human seat, which the invitation
	// binds her to: a bound person writes AS their seat, which is the
	// attribution the write phase reads back.
	janeSeat = "founder"
)

// janeGrants are what the invitation confers: enough to read the company's
// state — the socket needs it — and to file work, and nothing else, so no
// phase passes on a grant it did not ask for.
var janeGrants = []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite}

// signIn invites Jane under the deployment's Tier A token, redeems the link
// and signs her in with her password, all on one member — which leaves her
// browser holding the session cookie that member minted.
func signIn(t *testing.T, n *node, b *browser) {
	t.Helper()

	// THE INVITATION, as `crewlet iam invite` issues it: the deployment's
	// own token, since the company has nobody in it who could.
	status, issued := tierA(t, n, http.MethodPost, "/iam/invitations", map[string]any{
		"email": janeAddress, "grants": janeGrants,
		"seat": janeSeat, "reason": "the fleet session case",
	})
	if status != http.StatusCreated && status != http.StatusAccepted {
		t.Fatalf("POST /iam/invitations answered %d: %v", status, issued)
	}
	link, _ := issued["url"].(string)
	rest, found := strings.CutPrefix(link, deploymentURL+"/dashboard#/invite/")
	id, secret, split := strings.Cut(rest, ".")
	if !found || !split || id == "" || secret == "" {
		t.Fatalf("the invitation's link %q is not the dashboard's invitation "+
			"screen on %s", link, deploymentURL)
	}
	// A 202 IS DURABLE AND NOT YET HERE: the link opens on this member only
	// once it has applied the invitation, and a redemption before that is
	// refused as a link nobody issued.
	settle(t, "member "+n.id+" to apply the invitation", func() (bool, string) {
		row, err := n.engine.IAM().InvitationByID(t.Context(), id)
		return err == nil && row.ID == id, ""
	})

	// THE REDEMPTION, as the dashboard's invitation screen posts it: the id
	// in the path, the secret in the body, from the deployment's origin.
	status, redeemed := b.send(n, http.MethodPost, auth.AuthInvitePrefix+id, map[string]any{
		"secret": secret, "login": janeLogin, "name": "Jane Doe", "password": janePassword,
	})
	if status != http.StatusOK {
		t.Fatalf("redeeming the invitation answered %d: %v", status, redeemed)
	}

	// AND A SIGN-IN, with the password she chose, which replaces the
	// redemption's cookie with one opened by the password route.
	status, opened := b.send(n, http.MethodPost, auth.PathAuthLogin, map[string]any{
		"login": janeLogin, "password": janePassword,
	})
	if status != http.StatusOK || opened["status"] != "signed_in" {
		t.Fatalf("signing in answered %d: %v", status, opened)
	}
	if opened["login"] != janeLogin || opened["seat"] != janeSeat {
		t.Fatalf("the sign-in opened a session for %v at seat %v, want %s at %s",
			opened["login"], opened["seat"], janeLogin, janeSeat)
	}
	if b.session() == "" {
		t.Fatalf("the sign-in set no %s cookie on a deployment at %s: %v",
			session.CookieName(deploymentURL), deploymentURL, b.cookies)
	}
}

// servedElsewhere presents the cookie to a member that learns the session only
// from the log, and waits for it to answer as Jane.
//
// NEVER 401 ON THE WAY, which is the whole of the behind-node rule: until this
// member applies the session's start record it serves the read on the bearer
// alone or answers 503, and a 401 would have a browser discard a cookie that
// is perfectly good one apply later.
func servedElsewhere(t *testing.T, n *node, b *browser) {
	t.Helper()
	// WHAT THE MEMBER SAID ON THE WAY, logged: whether it was still behind
	// when the cookie first arrived is the network's to decide, and a run
	// that never saw it behind exercised only the caught-up arm.
	var seen []string
	defer func() { t.Logf("member %s answered the cookie: %s", n.id, strings.Join(seen, ", ")) }()
	settle(t, "member "+n.id+" to serve the session as "+janeLogin, func() (bool, string) {
		status, who := b.send(n, http.MethodGet, auth.PathAuthSession, nil)
		if login, _ := who["login"].(string); login != "" {
			seen = append(seen, fmt.Sprintf("%d as %s", status, login))
		} else {
			seen = append(seen, fmt.Sprintf("%d naming nobody", status))
		}
		switch status {
		case http.StatusUnauthorized:
			return false, fmt.Sprintf("GET %s answered 401 on a member that had "+
				"not caught up with the session (%v): a browser reads that as "+
				"signed out and throws a good cookie away", auth.PathAuthSession, who)
		case http.StatusServiceUnavailable:
			return false, ""
		case http.StatusOK:
			// A BEHIND MEMBER SERVES THE BEARER ALONE, with no row to
			// name anybody: that is a read it may serve, and not yet
			// the answer this waits for.
			if who["login"] != janeLogin {
				return false, ""
			}
			if who["seat"] != janeSeat || !grantsAre(who["grants"], janeGrants) {
				return false, fmt.Sprintf("the session reads as %v at seat %v "+
					"holding %v, want %s at %s holding %v", who["login"],
					who["seat"], who["grants"], janeLogin, janeSeat, janeGrants)
			}
			return true, ""
		}
		return false, fmt.Sprintf("GET %s answered %d: %v", auth.PathAuthSession,
			status, who)
	})
}

// writesAsThePerson files a work item through the cookie on the member that did
// not mint it, and reads back who the record says wrote it.
//
// AS THEIR SEAT, OF THE HUMAN KIND, THROUGH THEIR SESSION: a person bound to a
// seat acts as it, the kind is what tells a colleague's edit from an agent's
// and an operator's, and the credential column is what says which sign-in it
// came through. Read back from this member's own rows, because the write's
// answer is a receipt and the record is what every board renders.
func writesAsThePerson(t *testing.T, n *node, b *browser) {
	t.Helper()
	// ONE KEY FOR EVERY ATTEMPT, because a 503 from a member still catching
	// up with the session may be retried and a retry must be the same
	// operation rather than a second item — minted in the engine's grammar,
	// as every client that keys a write mints one, since the surface refuses
	// a key that carries no instant.
	key := statelog.NewOpID(time.Now(), "")
	var filed map[string]any
	settle(t, "member "+n.id+" to take a write through the cookie", func() (bool, string) {
		status, body := b.sendKeyed(n, http.MethodPost, "/work/items", key, map[string]any{
			"title": "file the fleet session case", "project": "ENG",
		})
		switch status {
		case http.StatusOK, http.StatusAccepted:
			filed = body
			return true, ""
		case http.StatusServiceUnavailable:
			return false, ""
		}
		return false, fmt.Sprintf("POST /work/items through the cookie answered "+
			"%d: %v", status, body)
	})
	item, _ := filed["key"].(string)
	if item == "" {
		t.Fatalf("the write answered no work item key: %v", filed)
	}

	var change tracker.HistoryEntry
	settle(t, "member "+n.id+" to apply "+item, func() (bool, string) {
		detail, err := n.engine.Tracker().Task(t.Context(), item,
			tracker.DetailWants{History: true},
			statelog.Freshness{Level: statelog.ReadSession})
		if err != nil || len(detail.History) == 0 {
			return false, ""
		}
		change = detail.History[len(detail.History)-1]
		return true, ""
	})
	if change.Actor != janeSeat || change.ActorKind != tracker.AuthorHuman ||
		!strings.HasPrefix(change.OperatorID, iam.SessionPrefix) {
		t.Errorf("%s is written as %q (%s) through %q, want Jane's seat %q as a "+
			"human, through her session", item, change.Actor, change.ActorKind,
			change.OperatorID, janeSeat)
	}
}

// socketOpens dials the live socket with the cookie on a member that did not
// mint it, and then the same handshake from another site.
//
// THE HANDSHAKE IS A GET, so the cross-site rule every write meets would pass
// it as a read — and a browser attaches the session cookie to a hostile page's
// handshake exactly as it does to the dashboard's. So the socket judges the
// Origin itself, by the writes' rule, and the second dial is what says it did.
func socketOpens(t *testing.T, n *node, b *browser) {
	t.Helper()
	conn, res, err := b.dial(t, n, deploymentURL)
	if err != nil {
		t.Fatalf("the socket refused the session cookie from %s: %v (%s)",
			deploymentURL, err, answered(res))
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	// The snapshot carries the whole roster and more than the default cap.
	conn.SetReadLimit(8 << 20)
	read, cancel := context.WithTimeout(t.Context(), waitBudget)
	defer cancel()
	for {
		_, raw, err := conn.Read(read)
		if err != nil {
			t.Fatalf("the socket opened and never delivered its snapshot: %v", err)
		}
		var frame struct {
			Kind stream.Kind `json:"kind"`
		}
		if json.Unmarshal(raw, &frame) == nil && frame.Kind == stream.KindSnapshot {
			break
		}
	}

	const elsewhere = "https://elsewhere.example.net"
	foreign, res, err := b.dial(t, n, elsewhere)
	if err == nil {
		_ = foreign.Close(websocket.StatusNormalClosure, "")
		t.Fatalf("a handshake from %s carrying the session cookie opened the "+
			"socket: any page Jane visits could read the company's state as her",
			elsewhere)
	}
	if res == nil || res.StatusCode != http.StatusForbidden ||
		refusalCode(res) != string(httpjson.CodeCSRFOrigin) {
		t.Errorf("a handshake from %s was refused with %s, want 403 %s",
			elsewhere, answered(res), httpjson.CodeCSRFOrigin)
	}
}

// revokedEverywhere signs out everywhere on one member and waits for the cookie
// it ended to be refused on the member that minted it.
//
// THE COOKIE IS A COPY taken before the sign-out, as a browser left open on
// another machine holds it: the sign-out clears the cookie in the browser that
// asked, and what a revocation is FOR is every other copy. It is refused
// through the person's revocation epoch, which the minting member learns only
// from the log — so until it applies the record it goes on serving the copy,
// which is the horizon, and the bound is the one every record in this suite is
// given to cross the fleet.
//
// AND THE TAB LEFT OPEN THERE CLOSES. A socket is authenticated at its
// handshake and then held, so what ends it is the record that ended its
// session, heard from the minting member's own identity applier — the engine
// hands that applier's word to the API, which decides the socket again and
// closes it `4401`. Left to its handshake, the tab would go on receiving the
// company's state for as long as it stayed open.
func revokedEverywhere(t *testing.T, minted, other *node, b *browser) {
	t.Helper()
	elsewhere := b.copy()
	// THE CONTROL: the copy is live on the minting member before the
	// sign-out, so a 401 below is the revocation's and not a cookie that
	// never worked there.
	if status, who := elsewhere.send(minted, http.MethodGet, auth.PathAuthSession, nil); status != http.StatusOK || who["login"] != janeLogin {
		t.Fatalf("before signing out, member %s answered the session %d: %v",
			minted.id, status, who)
	}
	tab := openTab(t, minted, elsewhere)

	status, body := b.send(other, http.MethodPost, "/auth/logout/all", nil)
	if status != http.StatusOK {
		t.Fatalf("POST /auth/logout/all on member %s answered %d: %v",
			other.id, status, body)
	}
	if b.session() != "" {
		t.Errorf("signing out everywhere left the asking browser holding its " +
			"cookie")
	}

	for _, n := range []*node{minted, other} {
		settle(t, "member "+n.id+" to refuse the signed-out cookie", func() (bool, string) {
			status, who := elsewhere.send(n, http.MethodGet, auth.PathAuthSession, nil)
			switch status {
			case http.StatusUnauthorized:
				return true, ""
			case http.StatusOK, http.StatusServiceUnavailable:
				return false, ""
			}
			return false, fmt.Sprintf("the signed-out cookie answered %d: %v",
				status, who)
		})
	}
	if got := <-tab; got != stream.CloseUnauthenticated {
		t.Errorf("the tab left open on member %s closed %d after signing out "+
			"everywhere, want %d", minted.id, got, stream.CloseUnauthenticated)
	}
}

// openTab opens the live socket on one member with this browser's cookie, reads
// its snapshot, and reports how it closes — reading every frame until then,
// as a tab does, for at most the suite's wait budget.
func openTab(t *testing.T, n *node, b *browser) <-chan websocket.StatusCode {
	t.Helper()
	conn, res, err := b.dial(t, n, deploymentURL)
	if err != nil {
		t.Fatalf("the socket refused the session cookie on member %s: %v (%s)",
			n.id, err, answered(res))
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	// The snapshot carries the whole roster and more than the default cap.
	conn.SetReadLimit(8 << 20)
	closed := make(chan websocket.StatusCode, 1)
	go func() {
		read, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), waitBudget)
		defer cancel()
		for {
			if _, _, err := conn.Read(read); err != nil {
				closed <- websocket.CloseStatus(err)
				return
			}
		}
	}()
	return closed
}

// browser is one person's browser, pointed at whichever member of a fleet each
// request is sent to — as a load balancer sends it.
//
// IT ATTACHES WHAT A BROWSER ATTACHES WITHOUT BEING ASKED, and those are the
// two things the engine's cookie rules read: every cookie the deployment set,
// on every request, and the deployment's own Origin on every request that is
// not a read. A cookie jar from net/http would not do: the deployment is
// https, its cookie is Secure, and a jar sends a Secure cookie to no member of
// a fleet served over plain loopback http — which is exactly where the
// TLS-terminating proxy it stands in for would have sent it.
type browser struct {
	t       *testing.T
	cookies map[string]string
}

func newBrowser(t *testing.T) *browser {
	return &browser{t: t, cookies: map[string]string{}}
}

// copy is another browser holding what this one holds now — the same cookie,
// left open somewhere else.
func (b *browser) copy() *browser {
	out := newBrowser(b.t)
	for name, value := range b.cookies {
		out.cookies[name] = value
	}
	return out
}

// session is the session cookie this browser holds, or "".
func (b *browser) session() string {
	return b.cookies[session.CookieName(deploymentURL)]
}

// send makes one request to one member and answers its status and decoded
// body, keeping every cookie the answer set and dropping every one it cleared.
func (b *browser) send(n *node, method, path string, body any) (int, map[string]any) {
	return b.sendKeyed(n, method, path, "", body)
}

// sendKeyed is [browser.send] under an operation key, which is what makes a
// retried write the same write.
func (b *browser) sendKeyed(n *node, method, path, key string, body any) (int, map[string]any) {
	b.t.Helper()
	status, raw := b.sendRaw(n, method, path, key, body)
	return status, decoded(b.t, bytes.NewReader(raw))
}

// sendRaw is [browser.sendKeyed] answering the body byte for byte, for a case
// that has to hand the engine's own bytes to the dashboard's client.
func (b *browser) sendRaw(n *node, method, path, key string, body any) (int, []byte) {
	b.t.Helper()
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			b.t.Fatalf("encode the body for %s %s: %v", method, path, err)
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(b.t.Context(), method, n.server.URL+path, payload)
	if err != nil {
		b.t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if !auth.IsRead(method) {
		req.Header.Set("Origin", deploymentURL)
	}
	if key != "" {
		req.Header.Set(opkey.Header, key)
	}
	for _, c := range b.jar() {
		req.AddCookie(c)
	}
	res, err := n.server.Client().Do(req)
	if err != nil {
		b.t.Fatalf("%s %s on member %s: %v", method, path, n.id, err)
	}
	defer res.Body.Close()
	b.keep(res.Cookies())
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		b.t.Fatalf("%s %s on member %s: read the answer: %v", method, path, n.id, err)
	}
	return res.StatusCode, raw
}

// dial opens the live socket on one member, presenting this browser's cookies
// and the given Origin — the deployment's own, or another site's.
func (b *browser) dial(t *testing.T, n *node, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	header := http.Header{"Origin": {origin}}
	for _, c := range b.jar() {
		header.Add("Cookie", c.String())
	}
	target := "ws" + strings.TrimPrefix(n.server.URL, "http") + auth.SocketPath
	return websocket.Dial(t.Context(), target, &websocket.DialOptions{
		HTTPClient: n.server.Client(), HTTPHeader: header,
	})
}

// jar is what this browser attaches, in a stable order.
func (b *browser) jar() []*http.Cookie {
	names := make([]string, 0, len(b.cookies))
	for name := range b.cookies {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]*http.Cookie, 0, len(names))
	for _, name := range names {
		out = append(out, &http.Cookie{Name: name, Value: b.cookies[name]})
	}
	return out
}

// keep applies what an answer said about cookies: a value is set, a deletion
// — an expiry in the past or a negative max-age — removes it.
func (b *browser) keep(set []*http.Cookie) {
	now := time.Now()
	for _, c := range set {
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(now)) {
			delete(b.cookies, c.Name)
			continue
		}
		b.cookies[c.Name] = c.Value
	}
}

// tierA makes one request to one member as the deployment's own Tier A token —
// a script, not a browser: no cookie and no Origin.
func tierA(t *testing.T, n *node, method, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the body for %s %s: %v", method, path, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, n.server.URL+path,
		bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := n.server.Client().Do(present(req))
	if err != nil {
		t.Fatalf("%s %s on member %s: %v", method, path, n.id, err)
	}
	defer res.Body.Close()
	return res.StatusCode, decoded(t, res.Body)
}

// decoded is a JSON answer's body, or an empty one for an answer with none.
func decoded(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"raw": string(raw)}
	}
	return out
}

// refusalCode is the envelope's code on a refused handshake, whose body the
// socket library keeps the first kilobyte of.
func refusalCode(res *http.Response) string {
	if res == nil || res.Body == nil {
		return ""
	}
	var envelope struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &envelope)
	return envelope.Error
}

// answered names a handshake's answer for a failure message.
func answered(res *http.Response) string {
	if res == nil {
		return "no answer"
	}
	return res.Status
}

// grantsAre reports whether a decoded grant list holds exactly want.
func grantsAre(decodedGrants any, want []iam.Grant) bool {
	held, _ := decodedGrants.([]any)
	if len(held) != len(want) {
		return false
	}
	for _, g := range held {
		name, _ := g.(string)
		if !slices.Contains(want, iam.Grant(name)) {
			return false
		}
	}
	return true
}

// settle polls until probe says it is done, within the bound a record takes to
// cross this suite's fleet, and fails at once on an answer probe says must
// never be given.
//
// [clusterSettle], because every wait here is a record one member published
// reaching another's rows — a sign-in's session, a write, a revocation — which
// is the path that constant is measured on.
func settle(t *testing.T, what string, probe func() (done bool, never string)) {
	t.Helper()
	deadline := time.Now().Add(clusterSettle)
	for {
		done, never := probe()
		switch {
		case never != "":
			t.Fatal(never)
		case done:
			return
		case time.Now().After(deadline):
			t.Fatalf("timed out after %s waiting for %s", clusterSettle, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
