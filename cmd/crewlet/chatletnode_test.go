package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY `chatlet` VERB, AGAINST A REAL NODE, AS A PERSON — and refused to a
// credential acting as itself.
//
// The cases beside this one hold each verb against a hand-written node, which
// agrees with the CLI about the routes, the bodies and the answers because one
// author wrote both. Only a real node can say they agree with the SERVER: its
// guard, its authority table's seated rows, its act transport, the chat
// questions and the human write surface's destructions. So the founder is
// invited and redeems the link, mints their own token with `crewlet iam token
// -login`, and runs every verb with it — say something, list, read, search,
// erase, set a horizon, prune — while the deployment's own Tier A token, which
// holds admin and acts as nobody's seat, is refused a read and told how to
// get a seat.
func TestEveryChatletVerbWorksAgainstARealNodeAsAPerson(t *testing.T) {
	t.Parallel()
	port := reservePort(t)
	boot := bootstrapFor(t, port)
	boot.API.Auth.TOTP = iam.SecondFactorOptional
	company, err := config.ParseCompany([]byte(peopleCompanyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if _, onChatlet := engine.OperatorChat(e); !onChatlet {
		t.Fatal("the test company runs no Chatlet, so this asserts nothing — " +
			"check peopleCompanyYAML still leaves chat.backend to derive")
	}
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := httpxtest.Pool(t)

	// THE DEPLOYMENT'S OWN TOKEN reads nothing: it holds admin, and acts as
	// itself.
	_, _, err = cli(t, "chatlet", "channels", "-api", base)
	if err == nil || !strings.Contains(err.Error(), "crewlet iam bind") {
		t.Fatalf("the Tier A token read the rooms or was refused without the "+
			"binding named: %v", err)
	}

	token := founderToken(t, client, base)
	as := func(args ...string) string {
		t.Helper()
		out, errs, err := cliIn(t, tokenEnviron(token),
			append(append([]string{"chatlet"}, args...), "-api", base)...)
		if err != nil {
			t.Fatalf("crewlet chatlet %s: %v\n%s%s", strings.Join(args, " "), err, out, errs)
		}
		return out
	}
	room := openRoom(t, client, base, token, "launch")

	posted := as("post", "launch", "-body", "the launch moves to friday")
	if !strings.Contains(posted, "in launch: applied") {
		t.Fatalf("the post printed:\n%s", posted)
	}
	sent := strings.Fields(strings.TrimPrefix(posted, "post "))[0]

	if row := lineWith(as("channels"), room); !strings.Contains(row, "launch") {
		t.Errorf("the room the founder made is not on their rail: %q", row)
	}
	if read := lineWith(as("read", room), "the launch moves to friday"); !strings.Contains(
		read, "founder") || !strings.Contains(read, sent) {
		t.Errorf("the founder's room reads %q, want their own message, under their "+
			"seat and its id", read)
	}
	// THE INDEX FOLLOWS THE TRANSCRIPT, so a search is asked until it has
	// caught up — and is then held to the hit. THE MESSAGE IS ERASED BY THE ID
	// THE SEARCH PRINTED, as an administrator finds the one a credential was
	// pasted into: nobody erasing it is the one who posted it.
	var message string
	deadline := time.Now().Add(30 * time.Second)
	for {
		found := as("search", "friday")
		if hit := lineWith(found, room); !strings.Contains(found, "still building") &&
			hit != "" {
			if fields := strings.Fields(hit); len(fields) > 2 {
				message = fields[2]
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the search never found the message:\n%s", found)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if message != sent {
		t.Fatalf("the search's hit names the message %q, and the post said %q", message,
			sent)
	}

	if erased := as("erase", room, "-message", message, "-reason", "said in error",
		"-confirm", "1"); !strings.Contains(erased, "erase 1 message(s) in "+room+": applied") {
		t.Errorf("the erase printed:\n%s", erased)
	}
	if read := as("read", room); strings.Contains(read, "friday") ||
		!strings.Contains(read, "No messages") {
		t.Errorf("the erased message still reads:\n%s", read)
	}
	if kept := as("retention", room, "-days", "30", "-confirm", "30"); !strings.Contains(kept,
		"retention of "+room+": 30 day(s), applied") {
		t.Errorf("the horizon printed:\n%s", kept)
	}
	// THE COMPANY'S HORIZON IS CONFIRMED BY A WORD, which the command and the
	// route each spell: against the real route, so the two cannot drift.
	if kept := as("retention", room, "-company", "-confirm", "company"); !strings.Contains(kept,
		"retention of "+room+": the company's horizon, applied") {
		t.Errorf("the company's horizon printed:\n%s", kept)
	}
	cutoff := time.Now().UTC().Format(time.RFC3339Nano)
	if pruned := as("prune", room, "-cutoff", cutoff, "-confirm", cutoff); !strings.Contains(
		pruned, ": applied") {
		t.Errorf("the prune printed:\n%s", pruned)
	}

	// A REFUSAL NOBODY WORDED FOR A PERSON reads its code's own sentence, which
	// is all the node answers at ADR-0060's floor: a cutoff in the future, which
	// would empty the room, is refused `422 invalid`, and printed as the bare
	// code it said nothing about what to change.
	future := time.Now().UTC().Add(240 * time.Hour).Format(time.RFC3339Nano)
	_, _, err = cliIn(t, tokenEnviron(token), "chatlet", "prune", room, "-cutoff", future,
		"-confirm", future, "-api", base)
	if err == nil || !strings.Contains(err.Error(), httpjson.CodeInvalid.Message()) {
		t.Errorf("a prune of the future reads %v, want the invalid code's own sentence",
			err)
	}
}

// founderToken is the founder's own machine token on the node at base: invited
// under the deployment's Tier A token onto the founder's seat, the link
// redeemed, and the token minted by `crewlet iam token -login` as a founder
// runs it ([TestYourOwnTokenMintsOnARealNode]'s path).
func founderToken(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	const password = "a-perfectly-fine-passphrase"
	id, secret := inviteOverHTTP(t, client, base, "jane@example.com", "founder",
		[]iam.Grant{iam.GrantAdmin})
	founder, _ := json.Marshal(map[string]string{
		"secret": secret, "login": "jane.founder", "name": "Jane Founder",
		"password": password,
	})
	if status, body := post(t, client, base+"/auth/invite/"+id, founder); status != http.StatusOK {
		t.Fatalf("the founder's redemption answered %d: %s", status, body)
	}
	tierA := bootstrapWithKeyring(t, "k1")
	appendFile(t, tierA, fmt.Sprintf("api:\n  external_url: %s\n", base))
	var out, errs bytes.Buffer
	if err := runIAM(noEnviron, []string{"token", "-login", "jane.founder",
		"-label", "chat", "-grants", string(iam.GrantAdmin), "-config", tierA,
		"-api", base}, strings.NewReader(password+"\n"), &out, &errs); err != nil {
		t.Fatalf("iam token -login: %v\n%s", err, errs.String())
	}
	token, _, _ := strings.Cut(out.String(), "\n")
	if token = strings.TrimSpace(token); !strings.HasPrefix(token, "cwl_pat_") {
		t.Fatalf("the command printed no token:\n%s", out.String())
	}
	return token
}

// openRoom makes a public room through the act transport, as the dashboard's
// create form does — `crewlet chatlet` has no verb for it — and answers its id.
func openRoom(t *testing.T, client *http.Client, base, token, name string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"args": map[string]any{
		"channel": name, "kind": "public"}})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/operator/act/create_channel", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(opkey.Header, statelog.NewOpID(time.Now(), "create"))
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("create #%s: %v", name, err)
	}
	defer res.Body.Close()
	var answer struct {
		Receipt struct {
			Channel string `json:"channel"`
		} `json:"receipt"`
	}
	if err := json.NewDecoder(res.Body).Decode(&answer); err != nil ||
		res.StatusCode != http.StatusOK || answer.Receipt.Channel == "" {
		t.Fatalf("create #%s answered %d (%v) with no room", name, res.StatusCode, err)
	}
	return answer.Receipt.Channel
}
