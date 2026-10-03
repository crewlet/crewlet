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

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
)

// THE FIRST PERSON IS INVITED LIKE EVERYBODY AFTER THEM, END TO END: a real
// node on a fresh estate, its own identity log, its own guard in front of its
// own routes, and the command an operator types.
//
// There is no founder route. Every node that serves the API already requires a
// Tier A token, and that token is the credential a company has before it has
// anybody: `crewlet iam invite` under it issues an ordinary invitation, which
// confers only what its writer holds — the token's grants cut to this node's
// ceiling — and the invited person redeems the link like anybody else. What
// only a real node can say is that the pieces compose on an estate with
// NOBODY in it: that nothing on the way requires somebody to be enrolled
// already, that the invitation carries the grants the founder needs, and that
// the person it creates can sign in afterwards with the password they chose.
//
// Mutation: make the directory write require an enrolled administrator rather
// than the grant, and the invitation is refused; drop a grant from what the
// redemption confers, and the person's own session lacks it.
func TestTheFirstPersonIsInvitedUnderATierAToken(t *testing.T) {
	// NOT PARALLEL: the command reads its credential from the environment,
	// and only there, which is the rule it is exercising.
	port := freePort(t)
	boot := bootstrapFor(t, port)
	boot.API.Auth.Backend = config.AuthBackendLocal
	boot.API.Auth.Local = &config.APILocal{TOTP: iam.SecondFactorOptional}
	e := startFreshNode(t, boot)
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := httpxtest.Pool(t)

	if got := getJSON(t, base+"/health")["identity"]; got != api.IdentityUnclaimed {
		t.Fatalf("/health says identity %v on an estate nobody is enrolled "+
			"in, want %q", got, api.IdentityUnclaimed)
	}

	// THE COMMAND, as an operator types it on a fresh install: the
	// deployment's own Tier A token, and every grant the founder will
	// carry.
	t.Setenv(apiTokenEnv, cliFixtureToken)
	tierA := bootstrapWithKeyring(t, "k1")
	appendFile(t, tierA, "api:\n  external_url: "+base+"\n")
	var grants []string
	for _, g := range iam.AllGrants {
		grants = append(grants, string(g))
	}
	var out, errs bytes.Buffer
	if err := runIAM([]string{"invite", "jane@example.com",
		"-grants", strings.Join(grants, ","),
		"-config", tierA, "-api", base}, nil, &out, &errs); err != nil {
		t.Fatalf("iam invite under a Tier A token on an empty estate: %v\n%s",
			err, errs.String())
	}
	id, secret := inviteLink(t, out.String(), base)

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

	// THE PERSON EXISTS, active, carrying what the invitation conferred.
	held, err := e.IAM().PersonByLogin(t.Context(), "jane.founder")
	if err != nil {
		t.Fatalf("read the founder back: %v", err)
	}
	if held.ID == "" || held.Kind != iam.KindPerson || held.Stage != iam.StageActive {
		t.Fatalf("the redemption left %+v, want an active person", held)
	}
	for _, g := range iam.AllGrants {
		if !slices.Contains(held.Grants, g) {
			t.Errorf("the founder does not carry %s: %v", g, held.Grants)
		}
	}

	// AND THEY CAN SIGN IN with the password they chose, and the session
	// that opens says they are who they are with what they were given.
	signIn, _ := json.Marshal(map[string]string{
		"login": "jane.founder", "password": password})
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
		t.Fatalf("the founder's sign-in answered %d with %d cookies",
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
	var who struct {
		Person string      `json:"person"`
		Login  string      `json:"login"`
		Grants []iam.Grant `json:"grants"`
	}
	if err := json.NewDecoder(res.Body).Decode(&who); err != nil ||
		res.StatusCode != http.StatusOK {
		t.Fatalf("the session read answered %d (%v)", res.StatusCode, err)
	}
	if who.Person != held.ID || who.Login != "jane.founder" {
		t.Errorf("the session is %s as %q, want %s as jane.founder",
			who.Person, who.Login, held.ID)
	}
	if !slices.Contains(who.Grants, iam.GrantPeopleManage) {
		t.Errorf("the founder's session carries %v — without people:manage "+
			"they cannot invite anybody after them", who.Grants)
	}

	if got := getJSON(t, base+"/health")["identity"]; got != api.IdentityReady {
		t.Errorf("/health says identity %v once the founder is in, want %q",
			got, api.IdentityReady)
	}
}

// startFreshNode boots one node serving its API on an estate nobody is
// enrolled in, and stops it with the case.
func startFreshNode(t *testing.T, boot *config.Bootstrap) *engine.Engine {
	t.Helper()
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
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

// inviteLink reads the link `crewlet iam invite` printed back into the
// invitation's id and the secret beside it — the dashboard's screen, with the
// two halves in its fragment.
func inviteLink(t *testing.T, printed, base string) (id, secret string) {
	t.Helper()
	prefix := base + "/dashboard#/invite/"
	for _, line := range strings.Split(printed, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			if id, secret, ok = strings.Cut(rest, "."); ok && id != "" && secret != "" {
				return id, secret
			}
		}
	}
	t.Fatalf("the command printed no invitation link under %s:\n%s", prefix, printed)
	return "", ""
}
