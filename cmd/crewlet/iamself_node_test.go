package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
)

// YOUR OWN TOKEN, MINTED AGAINST A REAL NODE — the founder's first run of
// `crewlet iam token -login`, end to end.
//
// [TestYourOwnTokenIsMintedFromYourOwnSession] holds the command's own half
// against a hand-written node that treats the cookie as live the instant it is
// issued, which no node does: a sign-in answers BEFORE the node applies the
// session it opened, so the mint that follows it in the same breath arrives at
// a node a few hundred milliseconds short of the session's start. The guard
// used to refuse that write 503 on every real run, and only a real node — its
// own identity log, its own applier, its own guard in front of its own routes
// — could show it. So this is the whole path: the founder is invited under
// the deployment's Tier A token and redeems the link, the command signs in as
// them with the password on standard input, mints, and signs out; the token it
// printed then works, and the session it opened is over. Mutation: drop the guard's wait for the
// bearer's start and the mint answers 503.
func TestYourOwnTokenMintsOnARealNode(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	boot := bootstrapFor(t, port)
	boot.API.Auth.Backend = config.AuthBackendLocal
	// OPTIONAL on a loopback address, which is the posture a laptop runs and
	// the one `crewlet validate` admits without an acknowledgement; the
	// second factor has cases of its own.
	boot.API.Auth.Local = &config.APILocal{TOTP: iam.SecondFactorOptional}
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
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := httpxtest.Pool(t)

	// THE FOUNDER, invited under the Tier A token and redeeming the link
	// as anybody would.
	const password = "a-perfectly-fine-passphrase"
	id, secret := inviteOverHTTP(t, client, base, "jane@example.com",
		[]iam.Grant{iam.GrantStateRead, iam.GrantPeopleManage})
	founder, _ := json.Marshal(map[string]string{
		"secret": secret, "login": "jane.founder", "name": "Jane Founder",
		"password": password,
	})
	status, body := post(t, client, base+"/auth/invite/"+id, founder)
	if status != http.StatusOK {
		t.Fatalf("the founder's redemption answered %d: %s", status, body)
	}
	var signedIn struct {
		Person string `json:"person"`
	}
	if err := json.Unmarshal([]byte(body), &signedIn); err != nil || signedIn.Person == "" {
		t.Fatalf("the redemption answered no person (%v): %s", err, body)
	}

	// THE COMMAND, as a founder types it, pointed at the deployment by the
	// external URL a cookie write has to state as its origin.
	tierA := bootstrapWithKeyring(t, "k1")
	appendFile(t, tierA, fmt.Sprintf("api:\n  external_url: %s\n", base))
	var out, errs bytes.Buffer
	err = runIAM([]string{"token", "-login", "jane.founder", "-label", "laptop",
		"-grants", string(iam.GrantStateRead), "-config", tierA, "-api", base},
		strings.NewReader(password+"\n"), &out, &errs)
	if err != nil {
		t.Fatalf("iam token -login against a real node: %v\n%s", err, errs.String())
	}
	if strings.Contains(errs.String(), "could not be ended") {
		t.Errorf("the session the command opened was left open:\n%s", errs.String())
	}
	// THE SESSION IT OPENED IS OVER, on the node's own rows: the founder's
	// redemption session is the one left live, and the command's is ended —
	// its sign-out is a write straight after a sign-in too.
	sessions, err := e.IAM().Sessions(t.Context(), signedIn.Person)
	if err != nil {
		t.Fatalf("read the founder's sessions: %v", err)
	}
	var live, ended int
	for _, s := range sessions {
		if s.Live(time.Now()) {
			live++
		} else if !s.EndedAt.IsZero() {
			ended++
		}
	}
	if len(sessions) != 2 || live != 1 || ended != 1 {
		t.Errorf("the founder holds %d sessions, %d live and %d ended — want "+
			"the redemption's live and the command's ended: %+v",
			len(sessions), live, ended, sessions)
	}
	token, _, _ := strings.Cut(out.String(), "\n")
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "cwl_pat_") {
		t.Fatalf("the command printed no token:\n%s", out.String())
	}

	// AND THE TOKEN IT PRINTED IS ONE THE NODE HONOURS, as its owner.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		base+"/agents", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("present the minted token: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("the minted token answered %d on a read it carries the grant "+
			"for, want 200", res.StatusCode)
	}
}

// inviteOverHTTP issues one invitation under the fixture's Tier A token, the
// way `crewlet iam invite` does, and answers the id and secret its link
// carries.
func inviteOverHTTP(t *testing.T, client *http.Client, base, email string,
	grants []iam.Grant) (id, secret string) {

	t.Helper()
	raw, _ := json.Marshal(map[string]any{"email": email, "grants": grants})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/iam/invitations", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cliFixtureToken)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("invite %s: %v", email, err)
	}
	defer res.Body.Close()
	var issued struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&issued); err != nil ||
		res.StatusCode != http.StatusCreated {
		t.Fatalf("the invitation answered %d (%v)", res.StatusCode, err)
	}
	return inviteLink(t, issued.URL, base)
}

// post sends one unauthenticated JSON write and answers its status and body.
func post(t *testing.T, client *http.Client, url string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url,
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer res.Body.Close()
	var answer bytes.Buffer
	_, _ = answer.ReadFrom(res.Body)
	return res.StatusCode, answer.String()
}

// appendFile adds text to the end of a file a case wrote.
func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}
