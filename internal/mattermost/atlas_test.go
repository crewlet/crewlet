package mattermost_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
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
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)

// Values the fake carries in fields the read never decodes.
const (
	atlasCanaryEmail  = "atlas-canary-team@acme.example"
	atlasCanaryInvite = "atlas-canary-invite-id"
)

// The credentials a company holds, and the server's account key.
const (
	atlasTokenAda   = "bot-token-of-ada"
	atlasTokenSRE   = "bot-token-of-sre"
	atlasTokenAdmin = "admin-token-value"
	atlasServerKey  = "mattermost/chat.acme.example"
	atlasTeamRoute  = "GET /api/v4/teams/name/acme"
)

// atlasServer is a Mattermost server as the atlas asks it: the team by its
// slug, answering each token with the row or the status refuse names for it.
type atlasServer struct {
	id, display string
	refuse      map[string]int
	// fault answers a status to every token.
	fault int

	mu    sync.Mutex
	calls []string
}

func atlasTeam() *atlasServer {
	return &atlasServer{id: "7z9tf6yk3pdmbc8qro4hnswe1a", display: "Acme [Engineering]", refuse: map[string]int{}}
}

func (f *atlasServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+token)
	w.Header().Set("Content-Type", "application/json")
	status := f.fault
	if status == 0 {
		status = f.refuse[token]
	}
	if status == 0 && r.Method+" "+r.URL.Path != atlasTeamRoute {
		status = http.StatusNotFound
	}
	if status != 0 {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "refused by the stand-in", "status_code": status})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": f.id, "display_name": f.display, "name": "acme",
		"email": atlasCanaryEmail, "invite_id": atlasCanaryInvite, "description": atlasCanaryInvite,
		"allowed_domains": "acme.example", "type": "I"})
}

// atlasCompany is a company on Mattermost whose two agents each have a bot,
// and which holds the provisioning token.
func atlasCompany() *config.Company {
	on := true
	seat := func(name, handle string) config.Role {
		return config.Role{Name: name, Handle: handle,
			Integrations: config.RoleIntegrations{Mattermost: &config.RoleMattermost{Enabled: &on}}}
	}
	c := &config.Company{Name: "Acme", Roles: []config.Role{seat("SRE", "sre"), seat("Ada", "ada")}}
	c.Integrations.Mattermost = &config.Mattermost{Enabled: true, URL: "https://Chat.acme.example/", Team: "acme",
		Provisioning: &config.MattermostProvisioning{AdminToken: "${MM_ADMIN_TOKEN}"}}
	return c
}

func (f *atlasServer) input(t *testing.T, edit ...func(*config.Company)) collect.Input {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := atlasCompany()
	for _, e := range edit {
		e(c)
	}
	sealed := func(handle string) string {
		return seataccess.SinkName(integration.KindMattermost, mattermost.BotTokenField, handle)
	}
	env := config.NewResolver(config.MapSource{
		sealed("ada"): atlasTokenAda, sealed("sre"): atlasTokenSRE, "MM_ADMIN_TOKEN": atlasTokenAdmin,
	})
	return collect.Input{Config: c, Now: atlasNow, Env: env, HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}}
}

func mattermostSource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindMattermost)
	if !ok {
		t.Fatal("no Mattermost source is registered")
	}
	return src
}

func (f *atlasServer) read(t *testing.T, edit ...func(*config.Company)) collect.Read {
	t.Helper()
	read, err := mattermostSource(t).Collect(t.Context(), f.input(t, edit...))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

// atlasAccount is the server's account of claims as the atlas folds them.
func atlasAccount(t *testing.T, claims []atlas.Claim) (atlas.View, atlas.Entry) {
	t.Helper()
	v := atlas.Join(atlas.Fold(claims, atlasNow), atlas.Joined{}, atlasNow)
	e, ok := v.Find(atlas.MustRef(atlas.KindAccount, atlasServerKey).ID())
	if !ok {
		t.Fatal("the Mattermost account is not in the read")
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

// THE READ SAYS THE TEAM'S OWN ID AND DISPLAY NAME, on the account the
// settings name, with one GET under the first agent's bot token. Nothing
// about anybody reaches the read: a team's contact address and its invite id
// are never decoded.
//
// Mutation: decode a team's email or invite id into a claim, and a canary
// reaches the read; ask with the provisioning token first, and a system
// administrator's credential is on a path a bot's would do.
func TestTheAtlasReadsTheMattermostTeamsIdentity(t *testing.T) {
	t.Parallel()
	f := atlasTeam()
	read := f.read(t)
	if !slices.Equal(f.calls, []string{atlasTeamRoute + " " + atlasTokenAda}) || read.Calls != 1 ||
		len(read.Withheld) != 0 {
		t.Fatalf("the read made %v, counted %d calls and withheld %+v", f.calls, read.Calls, read.Withheld)
	}
	v, account := atlasAccount(t, read.Claims)
	if len(v.Entries) != 1 || atlasFact(account, atlas.FactAccountRef) != f.id ||
		atlasFact(account, atlas.FactAccountName) != "Acme (Engineering)" || len(account.Facts) != 2 {
		t.Errorf("the team: %+v", account.Facts)
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{atlasCanaryEmail, atlasCanaryInvite, atlasTokenAda, "@"} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q reached the read", canary)
		}
	}
	// With the settings, it is one account, named by them and disputed by
	// nothing.
	settings := derive.FromSettings(atlasCompany(), "r1", derive.Deployment{}).Claims(atlasNow)
	v, account = atlasAccount(t, append(slices.Clone(settings.Claims), read.Claims...))
	if len(v.Entries) != 1 || account.Name != "Mattermost" || account.Review || len(account.Sources) != 2 {
		t.Errorf("with the settings: %d entries, %q, review %v, %+v", len(v.Entries), account.Name,
			account.Review, account.Sources)
	}
}

// A CREDENTIAL MATTERMOST REFUSES IS PASSED OVER, THE NARROWEST ASKED FIRST,
// AND A TEAM NO CREDENTIAL IS SHOWN IS A FINDING THAT WITHDRAWS NOTHING: each
// agent's bot is asked by handle, then the provisioning token, and where
// every one is refused the standing says what to check and what an earlier
// read recorded stands, however long it lasts.
//
// Mutation: read a team nobody was shown as one that says nothing, and its
// name and id are withdrawn the hour a bot is deactivated.
func TestAMattermostTeamNoCredentialIsShownIsAFinding(t *testing.T) {
	t.Parallel()
	whole := atlasTeam().read(t)
	f := atlasTeam()
	f.refuse[atlasTokenAda], f.refuse[atlasTokenSRE] = http.StatusUnauthorized, http.StatusForbidden
	read := f.read(t)
	if len(read.Unread) != 0 || len(read.Claims) != len(whole.Claims) || read.Calls != 3 ||
		!strings.HasSuffix(f.calls[2], atlasTokenAdmin) || !strings.HasSuffix(f.calls[1], atlasTokenSRE) {
		t.Fatalf("with both bots refused: %d claims, %+v, calls %v", len(read.Claims), read.Unread, f.calls)
	}
	f = atlasTeam()
	f.refuse[atlasTokenAda], f.refuse[atlasTokenSRE], f.refuse[atlasTokenAdmin] =
		http.StatusNotFound, http.StatusNotFound, http.StatusNotFound
	read = f.read(t)
	if len(read.Unread) != 1 || read.Unread[0].Part != "team" || len(read.Claims) != 1 ||
		!strings.Contains(read.Unread[0].Detail, "integrations.mattermost.team") ||
		strings.Contains(read.Unread[0].Detail, "refused by the stand-in") {
		t.Fatalf("with every credential refused: %d claims, %+v", len(read.Claims), read.Unread)
	}
	status := collect.Status{Kind: integration.KindMattermost}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 || len(plan.Status.Unread) != 1 {
			t.Fatalf("at %s the refusal recorded %+v and missed %+v", at, plan.Records, plan.Status.Missing)
		}
		status = plan.Status
	}
	// The same read, were it whole, withdraws the two facts at once.
	empty := read
	empty.Unread = nil
	plan := collect.Decide(whole.Claims, empty, collect.Status{Kind: integration.KindMattermost}, atlasNow.Add(time.Hour))
	if len(plan.Records) != 1 || len(plan.Records[0].Withdrawn) != 2 {
		t.Errorf("the control recorded %+v, want two facts withdrawn", plan.Records)
	}
	// An unchanged read records nothing, and a renamed team one record.
	if plan := collect.Decide(whole.Claims, whole, status, atlasNow.Add(time.Hour)); len(plan.Records) != 0 {
		t.Errorf("an unchanged read recorded %+v", plan.Records)
	}
	renamed := atlasTeam()
	renamed.display = "Acme Labs"
	if plan := collect.Decide(whole.Claims, renamed.read(t), status, atlasNow.Add(time.Hour)); len(plan.Records) != 1 ||
		len(plan.Records[0].Claims) != 1 {
		t.Errorf("a renamed team recorded %+v, want its one changed fact", plan.Records)
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM MATTERMOST: every shape the
// redaction pass knows, planted in the team's id, its display name and the
// fields the read never decodes, leaves no piece of itself in the read, in
// any case, and what the mapped fields refused is counted.
//
// Mutation: cut a name before the builder judges it, or skip the builder's
// grammar on a value, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromMattermost(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		for i, f := range []*atlasServer{{id: s, display: s}, {id: atlasTeam().id, display: long}} {
			f.refuse = map[string]int{}
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
			if len(read.Withheld) < 2-i {
				t.Errorf("%s: read %d withheld %+v", shape.Name, i, read.Withheld)
			}
			// What sat beside a credential is still read.
			if _, account := atlasAccount(t, read.Claims); i == 1 && atlasFact(account, atlas.FactAccountRef) != f.id {
				t.Errorf("%s: the team beside the credential: %+v", shape.Name, account.Facts)
			}
		}
	}
}

// A CALL THAT DID NOT FINISH IS NO READ AT ALL: a server fault fails the
// read, and no other credential is tried, since a fault says nothing of the
// one that met it.
//
// Mutation: read a fault as a refusal, and a server restarting is recorded
// as a team the bots cannot see.
func TestAMattermostCallThatFailsFailsTheRead(t *testing.T) {
	t.Parallel()
	f := atlasTeam()
	f.fault = http.StatusInternalServerError
	read, err := mattermostSource(t).Collect(t.Context(), f.input(t))
	if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 || len(f.calls) != 1 {
		t.Errorf("the read answered %d claims and %v after %v", len(read.Claims), err, f.calls)
	}
}

// A COMPANY THAT DOES NOT CONNECT MATTERMOST, NAMES NO TEAM OR HOLDS NO
// CREDENTIAL TO ASK WITH IS NOT CONFIGURED, which the duty reads as nothing
// to read. A seat switched off is not asked through.
func TestAnUnconnectedMattermostIsNotConfigured(t *testing.T) {
	t.Parallel()
	off := false
	for name, edit := range map[string]func(*config.Company){
		"no block":     func(c *config.Company) { c.Integrations.Mattermost = nil },
		"switched off": func(c *config.Company) { c.Integrations.Mattermost.Enabled = false },
		"no team":      func(c *config.Company) { c.Integrations.Mattermost.Team = " " },
		"no address":   func(c *config.Company) { c.Integrations.Mattermost.URL = "${MM_UNSET}" },
		"no credential": func(c *config.Company) {
			c.Integrations.Mattermost.Provisioning = nil
			for i := range c.Roles {
				c.Roles[i].Integrations.Mattermost.Enabled = &off
			}
		},
	} {
		f := atlasTeam()
		if _, err := mattermostSource(t).Collect(t.Context(), f.input(t, edit)); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s: the read called Mattermost: %v", name, f.calls)
		}
	}
	if _, err := mattermostSource(t).Collect(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}
