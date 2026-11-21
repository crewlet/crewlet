package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
)

// THE FIRST PERSON IS INVITED LIKE EVERYBODY AFTER THEM, END TO END: a real node
// on a fresh estate, and the command an operator types.
//
// There is no founder route: `crewlet iam invite` under the Tier A token issues
// an ordinary invitation onto the first person's seat, conferring only what its
// writer holds, and they redeem it like anybody. Only a real node says nothing
// requires somebody already enrolled, and that they then sign in as the seat.
// `-grants admin` STORES EVERY APP BESIDE IT, so removing admin later leaves them.
//
// Mutation: require an enrolled administrator, not the grant, and the invitation
// is refused; drop a grant from the redemption, and the session lacks it; send
// `admin` alone, and the row omits the apps; drop the seat, and it is refused.
func TestTheFirstPersonIsInvitedUnderATierAToken(t *testing.T) {
	// NOT PARALLEL: the command reads its credential from the environment,
	// and only there, which is the rule it is exercising.
	port := reservePort(t)
	boot := bootstrapFor(t, port)
	e := startFreshNode(t, boot)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := httpxtest.Pool(t)

	if got := getJSON(t, base+"/health")["identity"]; got != api.IdentityUnclaimed {
		t.Fatalf("/health says identity %v on an estate nobody is enrolled "+
			"in, want %q", got, api.IdentityUnclaimed)
	}

	// THE COMMAND, as an operator types it on a fresh install: the
	// deployment's own Tier A token, the seat the company declares for its
	// founder, and admin.
	tierA := bootstrapWithKeyring(t, "k1")
	appendFile(t, tierA, "api:\n  external_url: "+base+"\n")
	var out, errs bytes.Buffer
	if err := runIAM(tokenEnviron(cliFixtureToken), []string{"invite", "jane@example.com", "-seat", "founder",
		"-grants", string(iam.GrantAdmin),
		"-config", tierA, "-api", base}, nil, &out, &errs); err != nil {
		t.Fatalf("iam invite under a Tier A token on an empty estate: %v\n%s",
			err, errs.String())
	}
	id, secret := linkIn(t, out.String(), base, "invite")

	// THE LINK, redeemed exactly as the dashboard's invitation screen
	// redeems it: the id in the path, the secret in the body.
	const password = "a-perfectly-fine-passphrase"
	redemption, _ := json.Marshal(map[string]string{
		"secret": secret, "login": "jane.founder", "name": "Jane Founder",
		"password": password,
	})
	status, body := post(t, client, base+"/auth/invite/"+id, redemption)
	if status != http.StatusOK {
		t.Fatalf("the redemption answered %d: %s", status, body)
	}

	// THE PERSON EXISTS, active, on the seat the invitation named, carrying
	// what it conferred: admin, and every app stored beside it.
	held, err := e.IAM().PersonByLogin(t.Context(), "jane.founder")
	if err != nil {
		t.Fatalf("read the founder back: %v", err)
	}
	if held.ID == "" || held.Kind != iam.KindPerson || held.Stage != iam.StageActive {
		t.Fatalf("the redemption left %+v, want an active person", held)
	}
	if held.Seat != "founder" {
		t.Errorf("the founder holds the seat %q, want founder — the one the "+
			"invitation named", held.Seat)
	}
	if got := slices.Sorted(slices.Values(held.Grants)); !slices.Equal(got,
		slices.Sorted(slices.Values(iam.AllGrants))) {
		t.Errorf("the founder's row lists %v, want admin and every app: %v",
			held.Grants, iam.AllGrants)
	}

	// AND THEY CAN SIGN IN with the password they chose, and the session
	// that opens says they are who they are, on their seat, with what they
	// were given.
	who := signedIn(t, client, base, "jane.founder", password)
	if who.Person != held.ID || who.Login != "jane.founder" || who.Seat != "founder" {
		t.Errorf("the session is %s as %q on %q, want %s as jane.founder on "+
			"founder", who.Person, who.Login, who.Seat, held.ID)
	}
	if !slices.Contains(who.Grants, iam.GrantAdmin) {
		t.Errorf("the founder's session carries %v — without admin they "+
			"cannot invite anybody after them", who.Grants)
	}

	if got := getJSON(t, base+"/health")["identity"]; got != api.IdentityReady {
		t.Errorf("/health says identity %v once the founder is in, want %q",
			got, api.IdentityReady)
	}
}

// A PERSON CREATED ON A SEAT SETS THEIR FIRST PASSWORD THROUGH THE LINK, END TO
// END: the administrator's command, a real node's create, and the link's screen.
//
// A create leaves the person active on the named seat with no password; the link
// the command prints once is for that. Only a real node says its reset path spends
// it: opens as a FIRST password link, sets the password, signs nobody in, is
// spent; then they sign in as the seat. -grants is every app, not admin.
//
// Mutation: drop the link block from what `iam create` prints, and no link is
// found; drop the seat from what it sends, and the create is refused naming
// it; send no grants when -grants is left out, and the node refuses the
// create; let the link be spent twice, and the second spend answers 200.
func TestAPersonCreatedOnASeatSetsTheirFirstPasswordThroughTheLink(t *testing.T) {
	// NOT PARALLEL, for the first person's case's reason.
	port := reservePort(t)
	boot := bootstrapFor(t, port)
	e := startFreshNode(t, boot)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := httpxtest.Pool(t)

	tierA := bootstrapWithKeyring(t, "k1")
	var out, errs bytes.Buffer
	if err := runIAM(tokenEnviron(cliFixtureToken), []string{"create", "-email", "jane.founder@example.com",
		"-seat", "founder", "-name", "Jane Founder",
		"-config", tierA, "-api", base}, nil, &out, &errs); err != nil {
		t.Fatalf("iam create on a seat: %v\n%s", err, errs.String())
	}
	for _, want := range []string{"login jane.founder, on seat founder",
		"password link for", "-op-id"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the create printed no %q:\n%s", want, out.String())
		}
	}
	link, secret := linkIn(t, out.String(), base, "reset")

	// THE PERSON EXISTS FROM THE CREATE, active, on the seat it named,
	// under the login the node proposed from the address.
	held, err := e.IAM().PersonByLogin(t.Context(), "jane.founder")
	if err != nil {
		t.Fatalf("read the created person back: %v", err)
	}
	if held.Kind != iam.KindPerson || held.Stage != iam.StageActive ||
		held.Seat != "founder" {
		t.Fatalf("the create left %+v, want an active person on founder", held)
	}
	if got := slices.Sorted(slices.Values(held.Grants)); !slices.Equal(got,
		slices.Sorted(slices.Values(iam.DefaultApps))) {
		t.Errorf("a create naming no -grants left %v, want the default apps and "+
			"neither atlas nor admin: %v", held.Grants, iam.DefaultApps)
	}

	// THE SCREEN THE LINK OPENS says it sets their FIRST password.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		base+"/auth/reset/"+link, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(authapi.ResetSecretHeader, secret)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("open the link: %v", err)
	}
	var view struct {
		Login string `json:"login"`
		First bool   `json:"first"`
	}
	err = json.NewDecoder(res.Body).Decode(&view)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("the link's screen answered %d (%v)", res.StatusCode, err)
	}
	if view.Login != "jane.founder" || !view.First {
		t.Errorf("the link opens as %+v, want jane.founder's first password", view)
	}

	// SPENT, exactly as the screen spends it: the secret and the password
	// they chose in the body.
	const password = "a-perfectly-fine-passphrase"
	spend, _ := json.Marshal(map[string]string{"secret": secret,
		"password": password})
	if status, body := post(t, client, base+"/auth/reset/"+link, spend); status != http.StatusOK {
		t.Fatalf("spending the first password link answered %d: %s", status, body)
	}

	// AND THEY SIGN IN with it, as their seat.
	who := signedIn(t, client, base, "jane.founder", password)
	if who.Person != held.ID || who.Seat != "founder" {
		t.Errorf("the session is %s on %q, want %s on founder", who.Person,
			who.Seat, held.ID)
	}

	// THE LINK IS SPENT: a second spend is the 410 every dead link answers —
	// once this node has applied the sign-in's session, which a sign-in
	// publishes without waiting for: until then a link that does not open
	// is one these rows cannot vouch is dead, and the answer is the 503 a
	// client retries.
	again, _ := json.Marshal(map[string]string{"secret": secret,
		"password": "another-perfectly-fine-one"})
	for deadline := time.Now().Add(10 * time.Second); ; {
		status, body := post(t, client, base+"/auth/reset/"+link, again)
		if status == http.StatusGone {
			break
		}
		if status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("a second spend of the first password link answered %d: %s",
				status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// signedInAs is what `/auth/session` says about a signed-in person.
type signedInAs struct {
	Person string      `json:"person"`
	Login  string      `json:"login"`
	Seat   string      `json:"seat"`
	Grants []iam.Grant `json:"grants"`
}

// signedIn signs a person in with their password, exactly as the sign-in
// screen does, and answers what the session that opens says about them.
func signedIn(t *testing.T, client *http.Client, base, login, password string) signedInAs {
	t.Helper()
	signIn, _ := json.Marshal(map[string]string{"login": login, "password": password})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/auth/login", bytes.NewReader(signIn))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || len(res.Cookies()) == 0 {
		t.Fatalf("%s's sign-in answered %d with %d cookies", login,
			res.StatusCode, len(res.Cookies()))
	}
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet,
		base+"/auth/session", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Cookies() {
		req.AddCookie(c)
	}
	res, err = client.Do(req)
	if err != nil {
		t.Fatalf("read the session: %v", err)
	}
	defer res.Body.Close()
	var who signedInAs
	if err := json.NewDecoder(res.Body).Decode(&who); err != nil ||
		res.StatusCode != http.StatusOK {
		t.Fatalf("the session read answered %d (%v)", res.StatusCode, err)
	}
	return who
}

// startFreshNode boots one node serving its API on an estate nobody is
// enrolled in — on a company declaring the human seat `founder`, the seat its
// first person is invited or created onto, signing in with a password alone —
// and stops it with the case.
func startFreshNode(t *testing.T, boot *config.Bootstrap) *engine.Engine {
	t.Helper()
	company, err := config.ParseCompany([]byte(peopleCompanyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	passwordOnly(boot, company)
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	return e
}

// linkIn reads a link `crewlet iam` printed back into its id and the secret
// beside it — the dashboard's screen named by route (`invite` for an
// invitation, `reset` for a reset or first password link), with the two
// halves in its fragment.
func linkIn(t *testing.T, printed, base, route string) (id, secret string) {
	t.Helper()
	prefix := base + "/dashboard#/" + route + "/"
	for _, line := range strings.Split(printed, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			if id, secret, ok = strings.Cut(rest, "."); ok && id != "" && secret != "" {
				return id, secret
			}
		}
	}
	t.Fatalf("the command printed no link under %s:\n%s", prefix, printed)
	return "", ""
}
