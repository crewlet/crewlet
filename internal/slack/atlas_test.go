package slack_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/slack"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)

// Values the fake carries in fields the read never decodes.
const (
	atlasCanaryBot  = "atlas-canary-bot-user"
	atlasCanaryUser = "U0ATLASCANARY"
)

// The two agents' bot tokens, by the handle each is sealed under.
const (
	atlasTokenAda = "bot-token-of-ada"
	atlasTokenSRE = "bot-token-of-sre"
)

// atlasSlack is Slack as the atlas asks it: auth.test, answering each live
// token with the workspace and refusing every other.
type atlasSlack struct {
	team, teamID, url string
	live              map[string]bool
	// limited answers 429 this many times first, naming wait seconds.
	limited, wait int
	// fault answers a status in place of an answer.
	fault int

	mu    sync.Mutex
	calls []string
}

func atlasWorkspace() *atlasSlack {
	return &atlasSlack{team: "Acme [Corp]", teamID: "T0ACME123", url: "https://Acme.slack.com/",
		live: map[string]bool{atlasTokenAda: true, atlasTokenSRE: true}}
}

func (f *atlasSlack) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+token)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path != "/api/auth.test":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unknown_method"})
	case f.fault != 0:
		w.WriteHeader(f.fault)
	case f.limited > 0:
		f.limited--
		w.Header().Set("Retry-After", strconv.Itoa(f.wait))
		w.WriteHeader(http.StatusTooManyRequests)
	case !f.live[token]:
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "token_revoked"})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "team": f.team, "team_id": f.teamID,
			"url": f.url, "user": atlasCanaryBot, "user_id": atlasCanaryUser, "bot_id": "B0ATLASCANARY"})
	}
}

// atlasCompany is a company on Slack whose two agents each have an app, and
// whose person has none.
func atlasCompany() *config.Company {
	on := true
	seat := func(name, handle string) config.Role {
		return config.Role{Name: name, Handle: handle,
			Integrations: config.RoleIntegrations{Slack: &config.RoleSlack{Enabled: &on}}}
	}
	c := &config.Company{Name: "Acme", Roles: []config.Role{seat("SRE", "sre"), seat("Ada", "ada")}}
	c.Integrations.Slack = &config.Slack{}
	return c
}

func (f *atlasSlack) input(t *testing.T, edit ...func(*config.Company)) collect.Input {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := atlasCompany()
	for _, e := range edit {
		e(c)
	}
	env := config.NewResolver(config.MapSource{
		slack.SeatVar("ada", slack.FieldBotToken): atlasTokenAda,
		slack.SeatVar("sre", slack.FieldBotToken): atlasTokenSRE,
	})
	return collect.Input{Config: c, Now: atlasNow, Env: env, HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}}
}

func slackSource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindSlack)
	if !ok {
		t.Fatal("no Slack source is registered")
	}
	return src
}

func (f *atlasSlack) read(t *testing.T, edit ...func(*config.Company)) collect.Read {
	t.Helper()
	read, err := slackSource(t).Collect(t.Context(), f.input(t, edit...))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

// atlasAccount is the Slack account of claims as the atlas folds them.
func atlasAccount(t *testing.T, claims []atlas.Claim) (atlas.View, atlas.Entry) {
	t.Helper()
	v := atlas.Join(atlas.Fold(claims, atlasNow), atlas.Joined{}, atlasNow)
	e, ok := v.Find(atlas.MustRef(atlas.KindAccount, "slack").ID())
	if !ok {
		t.Fatal("the Slack account is not in the read")
	}
	return v, e
}

func atlasFact(e atlas.Entry, f atlas.Fact) string {
	for _, fv := range e.Facts {
		if fv.Fact == f {
			return fv.Value.Text
		}
	}
	return ""
}

// THE READ SAYS THE WORKSPACE'S OWN ID, NAME AND ADDRESS, on the account the
// settings name, with one call under the first agent's bot token. Nothing
// about anybody reaches the read: not the bot user that asked, not its ids.
//
// Mutation: decode auth.test's user or user_id into a claim, and a canary
// reaches the read; ask through every seat, and a read costs a call an agent.
func TestTheAtlasReadsTheSlackWorkspacesIdentity(t *testing.T) {
	t.Parallel()
	f := atlasWorkspace()
	read := f.read(t)
	if !slices.Equal(f.calls, []string{"POST /api/auth.test " + atlasTokenAda}) || read.Calls != 1 ||
		len(read.Withheld) != 0 {
		t.Fatalf("the read made %v, counted %d calls and withheld %+v", f.calls, read.Calls, read.Withheld)
	}
	v, account := atlasAccount(t, read.Claims)
	if len(v.Entries) != 1 || atlasFact(account, atlas.FactAccountRef) != "t0acme123" ||
		atlasFact(account, atlas.FactAccountName) != "Acme (Corp)" ||
		atlasFact(account, atlas.FactSite) != "https://acme.slack.com" || len(account.Facts) != 3 {
		t.Errorf("the workspace: %+v", account.Facts)
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{atlasCanaryBot, atlasCanaryUser, "B0ATLASCANARY", atlasTokenAda, "@"} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q reached the read", canary)
		}
	}
	// With the settings, it is one account, named by them and disputed by
	// nothing.
	settings := derive.FromSettings(atlasCompany(), "r1", derive.Deployment{}).Claims(atlasNow)
	v, account = atlasAccount(t, append(slices.Clone(settings.Claims), read.Claims...))
	if len(v.Entries) != 1 || account.Name != "Slack" || account.Review || len(account.Sources) != 2 {
		t.Errorf("with the settings: %d entries, %q, review %v, %+v", len(v.Entries), account.Name,
			account.Review, account.Sources)
	}
}

// AN APP SOMEBODY UNINSTALLED IS PASSED OVER, AND A WORKSPACE NO APP CAN ASK
// OF IS A FINDING THAT WITHDRAWS NOTHING: the next agent's token is asked,
// and where Slack refuses every one the standing says what to do and what an
// earlier read recorded stands, however long it lasts.
//
// Mutation: read a workspace nobody could ask of as one that says nothing,
// and its name and id are withdrawn the hour an app is uninstalled.
func TestASlackWorkspaceNoAppCanAskOfIsAFinding(t *testing.T) {
	t.Parallel()
	whole := atlasWorkspace().read(t)
	f := atlasWorkspace()
	f.live[atlasTokenAda] = false
	if read := f.read(t); len(read.Unread) != 0 || len(read.Claims) != len(whole.Claims) || read.Calls != 2 {
		t.Fatalf("with one dead token: %d claims in %d calls, %+v", len(read.Claims), read.Calls, read.Unread)
	}
	f = atlasWorkspace()
	f.live = nil
	read := f.read(t)
	if len(read.Unread) != 1 || read.Unread[0].Part != "workspace" || len(read.Claims) != 1 ||
		strings.Contains(read.Unread[0].Detail, "token_revoked") {
		t.Fatalf("with every token dead: %d claims, %+v", len(read.Claims), read.Unread)
	}
	status := collect.Status{Kind: integration.KindSlack}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 || len(plan.Status.Unread) != 1 {
			t.Fatalf("at %s the refusal recorded %+v and missed %+v", at, plan.Records, plan.Status.Missing)
		}
		status = plan.Status
	}
	// The same read, were it whole, withdraws the three facts at once.
	empty := read
	empty.Unread = nil
	plan := collect.Decide(whole.Claims, empty, collect.Status{Kind: integration.KindSlack}, atlasNow.Add(time.Hour))
	if len(plan.Records) != 1 || len(plan.Records[0].Withdrawn) != 3 {
		t.Errorf("the control recorded %+v, want three facts withdrawn", plan.Records)
	}
	// An unchanged read records nothing, and a renamed workspace one record.
	if plan := collect.Decide(whole.Claims, whole, status, atlasNow.Add(time.Hour)); len(plan.Records) != 0 {
		t.Errorf("an unchanged read recorded %+v", plan.Records)
	}
	renamed := atlasWorkspace()
	renamed.team = "Acme Labs"
	if plan := collect.Decide(whole.Claims, renamed.read(t), status, atlasNow.Add(time.Hour)); len(plan.Records) != 1 ||
		len(plan.Records[0].Claims) != 1 {
		t.Errorf("a renamed workspace recorded %+v, want its one changed fact", plan.Records)
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM SLACK: every shape the redaction
// pass knows, planted in the workspace's name, its id, its address and the
// fields the read never decodes, leaves no piece of itself in the read, in
// any case, and what the mapped fields refused is counted.
//
// Mutation: cut a name or lower an address's host before the builder judges
// it, or keep an address's path, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromSlack(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		for i, f := range []*atlasSlack{
			{team: s, teamID: s, url: "https://" + s + ".slack.com/"},
			{team: long, teamID: "T0ACME123", url: "https://ops:" + s + "@acme.slack.com/" + s + "?t=" + s + "#" + s},
		} {
			f.live = map[string]bool{atlasTokenAda: true}
			read := f.read(t)
			out, err := json.Marshal(read)
			if err != nil {
				t.Fatal(err)
			}
			lowered := strings.ToLower(string(out))
			for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
				if at := strings.Index(lowered, strings.ToLower(piece)); at >= 0 {
					t.Errorf("%s: a piece of the shape reached read %d near %q", shape.Name, i,
						string(out)[max(0, at-120):at])
				}
			}
			if len(read.Withheld) < 1 {
				t.Errorf("%s: read %d withheld nothing: %+v", shape.Name, i, read.Withheld)
			}
			// What sat beside a credential is still read. A shape with a
			// space in it makes the address no address at all.
			if _, account := atlasAccount(t, read.Claims); i == 1 && (atlasFact(account, atlas.FactAccountRef) != "t0acme123" ||
				(!strings.ContainsAny(s, " \n") && atlasFact(account, atlas.FactSite) != "https://acme.slack.com")) {
				t.Errorf("%s: the workspace beside the credential: %+v", shape.Name, account.Facts)
			}
		}
	}
}

// A CALL THAT DID NOT FINISH IS NO READ AT ALL: a server fault and a rate
// limit past what a call waits fail the read, and a short limit is waited
// out as Slack asks.
//
// Mutation: read a rate limit as a dead token, and a busy minute is recorded
// as an app somebody uninstalled.
func TestASlackCallThatFailsOrIsRateLimitedFailsTheRead(t *testing.T) {
	t.Parallel()
	for name, fault := range map[string]func(*atlasSlack){
		"a server fault": func(f *atlasSlack) { f.fault = http.StatusBadGateway },
		// A wait longer than one call takes, so the limit is handed back.
		"the rate limit": func(f *atlasSlack) { f.limited, f.wait = 1, 60 },
	} {
		f := atlasWorkspace()
		fault(f)
		read, err := slackSource(t).Collect(t.Context(), f.input(t))
		if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 {
			t.Errorf("%s: the read answered %d claims and %v", name, len(read.Claims), err)
		}
		if len(f.calls) != 1 {
			t.Errorf("%s: the read went on to another agent's token: %v", name, f.calls)
		}
	}
	f := atlasWorkspace()
	f.limited, f.wait = 1, 1
	if read := f.read(t); len(read.Claims) != 4 || read.Calls != 2 {
		t.Errorf("a short limit: %d claims in %d calls, want 4 in 2", len(read.Claims), read.Calls)
	}
}

// A COMPANY THAT DOES NOT CONNECT SLACK, OR NONE OF WHOSE AGENTS HAS AN APP
// TO ASK THROUGH, IS NOT CONFIGURED, which the duty reads as nothing to read.
// A seat switched off is not asked through.
func TestAnUnconnectedSlackIsNotConfigured(t *testing.T) {
	t.Parallel()
	off := false
	for name, edit := range map[string]func(*config.Company){
		"no block": func(c *config.Company) { c.Integrations.Slack = nil },
		"no seats": func(c *config.Company) { c.Roles = nil },
		"switched off": func(c *config.Company) {
			for i := range c.Roles {
				c.Roles[i].Integrations.Slack.Enabled = &off
			}
		},
		"no token": func(c *config.Company) {
			for i := range c.Roles {
				c.Roles[i].Integrations.Slack.BotToken = "${SLACK_UNSET}"
			}
		},
		"people only": func(c *config.Company) {
			for i := range c.Roles {
				c.Roles[i].Kind = "human"
			}
		},
	} {
		f := atlasWorkspace()
		if _, err := slackSource(t).Collect(t.Context(), f.input(t, edit)); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s: the read called Slack: %v", name, f.calls)
		}
	}
	if _, err := slackSource(t).Collect(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}
