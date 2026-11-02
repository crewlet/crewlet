package accessmigrate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessmigrate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/secrets"
)

// gophrase is GoPhrase's document as the engine before per-seat access stored
// it, its shape taken from the live company and every credential a fake: three
// agents with their own GitHub Apps at three tiers, a backend engineer and a
// QA engineer on GitLab (one under a per-seat override), and the SRE whose
// Datadog key was sealed into the company's own variable (the incident).
func gophrase(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/gophrase.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}
	return root
}

// vendors is every vendor a migration reads, as a case scripts it: the
// accounts each holds by the engine's naming, Datadog's roles, and which
// vendors do not answer.
type vendors struct {
	mu       sync.Mutex
	accounts map[integration.Kind]seataccess.Observation
	roles    []seataccess.Level
	down     map[integration.Kind]bool
	asked    []integration.Kind
}

// gophraseVendors is what GoPhrase's vendors show: the SRE's Datadog service
// account at the Admin Role, the two GitLab service accounts, and the roles
// GoPhrase's Datadog organization lists.
func gophraseVendors() *vendors {
	return &vendors{
		accounts: map[integration.Kind]seataccess.Observation{
			integration.KindDatadog: {"sre": {seataccess.LaneDatadog: {Exists: true,
				Enabled: true, AccountID: "dd-user-sre", Account: "crewlet-sre@agents.crewlet.invalid",
				Level: "role-admin"}}},
			integration.KindGitLab: {
				"backend-engineer": {seataccess.LaneGitLab: {Exists: true, Enabled: true,
					AccountID: "41", Account: "crewlet-backend-engineer", Level: "30"}},
				"qa": {seataccess.LaneGitLab: {Exists: true, Enabled: true, AccountID: "42",
					Account: "crewlet-qa", Level: "40"}},
			},
		},
		roles: []seataccess.Level{
			{ID: "role-ro", Name: "Datadog Read Only Role"},
			{ID: "role-std", Name: "Datadog Standard Role"},
			{ID: "role-admin", Name: "Datadog Admin Role", Administers: true},
		},
	}
}

func (v *vendors) open(_ context.Context, kind integration.Kind,
	_ seataccess.OpenInput) (seataccess.Session, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.asked = append(v.asked, kind)
	if v.down[kind] {
		return nil, errors.New("503 Service Unavailable")
	}
	return session{v: v, kind: kind}, nil
}

type session struct {
	v    *vendors
	kind integration.Kind
}

func (s session) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	if lane == seataccess.LaneDatadog {
		return seataccess.Options{Lane: lane, Live: true, Levels: s.v.roles}, nil
	}
	return seataccess.Options{Lane: lane}, nil
}

func (s session) Observe(context.Context, seataccess.ObserveInput) (seataccess.Observation, error) {
	return maps.Clone(s.v.accounts[s.kind]), nil
}

func (session) Apply(context.Context, seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, errors.New("a migration takes no step at a vendor")
}

// plan is a migration's plan of root, read from vs.
func plan(t *testing.T, root map[string]any, vs *vendors) accessmigrate.Migration {
	t.Helper()
	facts, err := accessmigrate.Read(t.Context(), root, vs.open, config.NewResolver(), false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := accessmigrate.Plan(root, facts)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// THE GOPHRASE DOCUMENT MIGRATES AS THE DESIGN SAYS (section 8, D12, D14).
//
// Every agent seat gets `enabled` per integration from the old plan; the
// company roles land in the seats in each vendor's terms (the SRE keeps the
// Datadog Admin Role, by the id Datadog lists it under; GitLab's override
// gives QA Maintainer; each GitHub tier becomes its permissions); a credential
// under a name of the seat's own moves to the engine's name; the SRE's key,
// which named the company's own variable, moves nowhere and is recorded for
// the danger notice; and every account the engine made before the ledger is
// seeded, from what its vendor shows (the SRE's Datadog service account, the
// GitLab service accounts) or from the seat's own block (the GitHub Apps).
func TestTheGoPhraseDocumentMigrates(t *testing.T) {
	t.Parallel()
	root := gophrase(t)
	if !accessmigrate.Needed(root) {
		t.Fatal("the premise: the old document needs a migration")
	}
	m := plan(t, root, gophraseVendors())
	if !slices.Equal(m.Migrated, []integration.Kind{integration.KindGitHub,
		integration.KindGitLab, integration.KindDatadog}) || len(m.Pending) != 0 {
		t.Fatalf("migrated %v, pending %v; want every connected integration", m.Migrated, m.Pending)
	}
	want := map[string]map[seataccess.Lane]seataccess.Desired{
		"pm": {
			seataccess.LaneGitHub: {Enabled: true, Permissions: seataccess.Permissions{
				"contents": "read", "issues": "read", "pull_requests": "read",
				"checks": "read", "actions": "read", "deployments": "read"},
				Scope: []string{"gophrase/backend", "gophrase/frontend", "gophrase/mobile-app"}},
			seataccess.LaneGitLab:  {},
			seataccess.LaneDatadog: {},
		},
		"backend-engineer": {
			seataccess.LaneGitHub: {Enabled: true, Permissions: seataccess.Permissions{
				"contents": "write", "issues": "write", "pull_requests": "write",
				"checks": "write", "actions": "read", "deployments": "read", "workflows": "write"},
				Scope: []string{"gophrase/backend"}},
			seataccess.LaneGitLab:  {Enabled: true, Level: "30"},
			seataccess.LaneDatadog: {},
		},
		"qa": {
			seataccess.LaneGitHub: {Enabled: true, Permissions: seataccess.Permissions{
				"contents": "read", "issues": "write", "pull_requests": "write",
				"checks": "read", "actions": "read", "deployments": "read"},
				Scope: []string{"gophrase/backend", "gophrase/frontend"}},
			seataccess.LaneGitLab:  {Enabled: true, Level: "40"},
			seataccess.LaneDatadog: {},
		},
		"sre": {
			seataccess.LaneGitHub:  {},
			seataccess.LaneGitLab:  {},
			seataccess.LaneDatadog: {Enabled: true, Level: "role-admin"},
		},
		"designer": {
			seataccess.LaneGitHub: {}, seataccess.LaneGitLab: {}, seataccess.LaneDatadog: {},
		},
	}
	for handle, lanes := range want {
		for lane, d := range lanes {
			got := m.Seats[handle][lane]
			if got.Enabled != d.Enabled || !got.SameAccess(d.Normalized()) {
				t.Errorf("%s on %s = %+v, want %+v", handle, lane, got, d)
			}
		}
	}
	if _, human := m.Seats["miles"]; human {
		t.Error("a person's seat was given agent access")
	}
	wantMoves := []accessmigrate.Move{
		{Handle: "backend-engineer", Kind: integration.KindGitLab, Field: "TOKEN",
			From: "BACKEND_GITLAB_PAT", To: "GITLAB_TOKEN_BACKEND_ENGINEER"},
	}
	if !slices.Equal(m.Moves, wantMoves) {
		t.Errorf("moves = %+v, want %+v", m.Moves, wantMoves)
	}
	if len(m.Notices) != 1 || m.Notices[0].Variable != "DATADOG_APP_KEY" ||
		m.Notices[0].Kind != integration.KindDatadog ||
		!slices.Equal(m.Notices[0].Seats, []string{"sre"}) {
		t.Errorf("notices = %+v, want one: the SRE shared DATADOG_APP_KEY", m.Notices)
	}
	seeded := map[string]string{}
	for _, s := range m.Seeds {
		seeded[string(s.Kind)+"/"+s.Handle] = s.Account.ID
	}
	if !maps.Equal(seeded, map[string]string{
		"datadog/sre": "dd-user-sre", "gitlab/backend-engineer": "41", "gitlab/qa": "42",
		"github/pm": "1000001", "github/backend-engineer": "1000002", "github/qa": "1000003",
	}) {
		t.Errorf("seeds = %v, want every account the engine made before the ledger", seeded)
	}
}

// THE MIGRATED DOCUMENT CARRIES NOTHING RETIRED AND IS MARKED, AND A SECOND RUN
// ASKS NOBODY ANYTHING.
func TestTheMigratedDocumentCarriesNothingRetired(t *testing.T) {
	t.Parallel()
	root := gophrase(t)
	changed, err := accessmigrate.Apply(root, plan(t, root, gophraseVendors()))
	if err != nil || !changed {
		t.Fatalf("Apply = %v, %v; want a change", changed, err)
	}
	doc := mustJSON(t, root)
	for _, retired := range []string{`"tier"`, `"workflows":true`, `"access_levels"`,
		`"role":"Datadog Admin Role"`, `"BACKEND_GITLAB_PAT"`} {
		if strings.Contains(doc, retired) {
			t.Errorf("the migrated document still carries %s", retired)
		}
	}
	if !strings.Contains(doc, `"seat_access_migrated":["datadog","github","gitlab"]`) {
		t.Errorf("the document does not mark the three integrations it migrated")
	}
	if sre := mustJSON(t, seat(t, root, "sre")); strings.Contains(sre, "${DATADOG_APP_KEY}") {
		t.Errorf("the SRE still names the company's Datadog key: %s", sre)
	}
	// THE ENGINE BINDS EACH SEAT'S OWN CREDENTIAL TO ITS TOOL SERVER (D15),
	// so a seat's own keys for a bound server are gone, and the sandbox's
	// issued GitHub token, which no server binding replaces, stays.
	backend := mustJSON(t, seat(t, root, "backend-engineer"))
	if strings.Contains(backend, "BACKEND_GITLAB_PAT") {
		t.Errorf("the backend engineer still names its own GitLab key: %s", backend)
	}
	if !strings.Contains(backend, `"GITHUB_TOKEN":"${GITHUB_APP_TOKEN_BACKEND_ENGINEER}"`) {
		t.Errorf("the migration touched the backend engineer's sandbox: %s", backend)
	}
	if !strings.Contains(doc, `"DD_API_KEY":"${DATADOG_API_KEY}"`) {
		t.Error("the company's own tool server lost its organization key")
	}
	if accessmigrate.Needed(root) {
		t.Fatal("a migrated document still reports a migration needed")
	}
	vs := gophraseVendors()
	again := plan(t, root, vs)
	if !again.Empty() || len(vs.asked) != 0 {
		t.Errorf("a second plan = %+v asking %v; want nothing to do and nobody asked",
			again, vs.asked)
	}
}

// A DATADOG THAT DOES NOT ANSWER LEAVES DATADOG, AND ONLY DATADOG, GATED.
//
// Its role cannot be looked up or its accounts read, so nothing is guessed:
// the SRE keeps its old seat exactly as it was, the company role is not
// removed, and Datadog is not marked, so its access pass waits. GitHub and
// GitLab move on. A Datadog that answers but lists no such role is the same,
// for an SRE whose account the engine has not made yet (one that exists keeps
// the role it holds, which needs no lookup). Mutation: fall back to a default
// role, or migrate every integration or none, and a case here fails.
func TestAnUnansweredVendorLeavesOnlyItsIntegrationGated(t *testing.T) {
	t.Parallel()
	for name, vs := range map[string]*vendors{
		"unreachable": func() *vendors {
			v := gophraseVendors()
			v.down = map[integration.Kind]bool{integration.KindDatadog: true}
			return v
		}(),
		"no such role": func() *vendors {
			v := gophraseVendors()
			v.roles = v.roles[:2]
			delete(v.accounts, integration.KindDatadog)
			return v
		}(),
	} {
		root := gophrase(t)
		before := mustJSON(t, seat(t, root, "sre")["mcp_env"])
		m := plan(t, root, vs)
		if m.Pending[integration.KindDatadog] == nil ||
			slices.Contains(m.Migrated, integration.KindDatadog) {
			t.Errorf("%s: migrated %v, pending %v; want Datadog pending", name, m.Migrated,
				m.Pending)
			continue
		}
		// THE REASON IS A PERSON'S TO READ: the access pass reports it.
		if why := m.Pending[integration.KindDatadog].Error(); !strings.Contains(why, "Datadog") ||
			strings.Contains(why, "accessmigrate") {
			t.Errorf("%s: the reason reads %q", name, why)
		}
		if _, err := accessmigrate.Apply(root, m); err != nil {
			t.Fatal(err)
		}
		sre := seat(t, root, "sre")
		if after := mustJSON(t, sre["mcp_env"]); after != before {
			t.Errorf("%s: the SRE's Datadog key moved while Datadog is gated:\n%s\n%s", name,
				before, after)
		}
		if _, written := sre["integrations"].(map[string]any)["datadog"]; written {
			t.Errorf("%s: the SRE was given a Datadog block while Datadog is gated", name)
		}
		doc := mustJSON(t, root)
		if !strings.Contains(doc, `"role":"Datadog Admin Role"`) ||
			!strings.Contains(doc, `"seat_access_migrated":["github","gitlab"]`) {
			t.Errorf("%s: the document is %s", name, doc)
		}
		if got := accessmigrate.Unmigrated(root); !slices.Equal(got,
			[]integration.Kind{integration.KindDatadog}) {
			t.Errorf("%s: unmigrated = %v, want Datadog alone", name, got)
		}
	}
}

// A CREDENTIAL HANDED IN FROM OUTSIDE CREWLET STAYS WHERE THE SEAT NAMES IT.
//
// GitLab connected without provisioning is GitLab the engine does not
// provision (D4b): the backend engineer's own token, exclusive to it, is not
// moved, not dropped and not re-minted, and the seat is on, so its tool is
// bound to it. Mutation: migrate an external lane as a provisioned one and
// the token moves.
func TestAnExternalCredentialStaysWhereTheSeatNamesIt(t *testing.T) {
	t.Parallel()
	root := gophrase(t)
	delete(root["integrations"].(map[string]any)["gitlab"].(map[string]any), "provisioning")
	m := plan(t, root, gophraseVendors())
	for _, move := range m.Moves {
		if move.Kind == integration.KindGitLab {
			t.Errorf("an external GitLab token was moved: %+v", move)
		}
	}
	if got := m.Seats["backend-engineer"][seataccess.LaneGitLab]; !got.Enabled {
		t.Errorf("the backend engineer's external GitLab lane = %+v, want on", got)
	}
	if _, err := accessmigrate.Apply(root, m); err != nil {
		t.Fatal(err)
	}
	if backend := mustJSON(t, seat(t, root, "backend-engineer")); !strings.Contains(backend,
		`"GITLAB_TOKEN":"${BACKEND_GITLAB_PAT}"`) {
		t.Errorf("the handed-in token is not where the seat named it: %s", backend)
	}
	for _, s := range m.Seeds {
		if s.Kind == integration.KindGitLab {
			t.Errorf("an account at an external GitLab was seeded: %+v", s)
		}
	}
}

// A MATTERMOST BOT'S CHANNELS ARE WRITTEN OUT, since an empty list now means no
// channels: the company's list and the seat's own channel, as the old plan
// applied them, or, where the plan named none, the channels the bot is in
// today. Mutation: leave the list empty and the bot is taken out of every
// channel on its next sync.
func TestAMattermostBotsChannelsAreWrittenOut(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		company []any
		member  []string
		own     string
		want    []string
	}{
		"the company's list and the seat's own": {company: []any{"town-square", "incidents"},
			own: "releases", want: []string{"incidents", "releases", "town-square"}},
		"the bot's own, where the plan named none": {member: []string{"engineering", "town-square"},
			want: []string{"engineering", "town-square"}},
	} {
		root := gophrase(t)
		provisioning := map[string]any{"team": "gophrase"}
		if tc.company != nil {
			provisioning["channels"] = tc.company
		}
		root["integrations"].(map[string]any)["mattermost"] = map[string]any{
			"enabled": true, "url": "https://chat.example.invalid", "provisioning": provisioning}
		block := map[string]any{"bot_token": "${MATTERMOST_BOT_TOKEN_QA}"}
		if tc.own != "" {
			block["channel"] = tc.own
		}
		seat(t, root, "qa")["integrations"].(map[string]any)["mattermost"] = block
		vs := gophraseVendors()
		vs.accounts[integration.KindMattermost] = seataccess.Observation{"qa": {
			seataccess.LaneMattermost: {Exists: true, Enabled: true, AccountID: "mm-qa",
				Scope: tc.member}}}
		got := plan(t, root, vs).Seats["qa"][seataccess.LaneMattermost]
		if !got.Enabled || !slices.Equal(got.Scope, tc.want) || got.Level != "system_user" {
			t.Errorf("%s: the QA bot = %+v, want on in %v", name, got, tc.want)
		}
	}
}

// RUN SEALS AND SEEDS BEFORE THE REVISION THAT OPENS THE GATE, never moves a
// shared value, and writes one revision however often it runs.
func TestRunSealsAndSeedsBeforeTheGatingRevision(t *testing.T) {
	t.Parallel()
	root := gophrase(t)
	vault := &fakeSecrets{values: map[string]string{
		"BACKEND_GITLAB_PAT": "fake-gitlab-token", "DATADOG_APP_KEY": "fake-company-key",
	}}
	ledger := &fakeLedger{records: map[integration.Kind]map[string]seataccess.Record{}}
	doc := &fakeDocument{root: root, revision: "rev-1", sealed: vault, ledger: ledger}
	vs := gophraseVendors()
	deps := accessmigrate.Deps{
		Document: doc, Secrets: vault, Records: &fakeRecords{ledger: ledger},
		Open: vs.open, Env: config.NewResolver(),
		Now: func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) },
	}
	result, err := accessmigrate.Run(t.Context(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Revision != "rev-2" || result.Moved != 1 || result.Notices != 1 || result.Seeded != 6 {
		t.Errorf("result = %+v", result)
	}
	if !doc.readyFirst {
		t.Error("the gating revision was written before the credential was sealed and the " +
			"accounts recorded")
	}
	if vault.sealed["GITLAB_TOKEN_BACKEND_ENGINEER"] != "fake-gitlab-token" {
		t.Error("the backend engineer's token was not re-sealed under the engine's name")
	}
	if _, moved := vault.sealed["DATADOG_APP_KEY_SRE"]; moved {
		t.Error("the company's Datadog key was copied into the SRE's own variable")
	}
	sre := ledger.records[integration.KindDatadog]["sre"]
	if sre.Account == nil || sre.Account.ID != "dd-user-sre" || sre.SharedCredential == nil ||
		sre.SharedCredential.Variable != "DATADOG_APP_KEY" {
		t.Errorf("the SRE's ledger record = %+v, want its account and the shared variable", sre)
	}
	if cred := ledger.records[integration.KindGitLab]["backend-engineer"].Credential; cred == nil ||
		cred.Variable != "GITLAB_TOKEN_BACKEND_ENGINEER" || cred.Source != seataccess.CredentialMinted {
		t.Errorf("the backend engineer's credential is recorded as %+v", cred)
	}
	// THE FINDING THE PASS RAISES AND THE TAB DRAWS names the variable and the
	// seat, never the value, and clears once the variable is rewritten.
	names := map[string]string{"sre": "SRE"}
	unchanged := func(context.Context, string) (time.Time, bool, error) {
		return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), true, nil
	}
	findings, err := accessmigrate.SharedFindings(t.Context(), integration.KindDatadog,
		ledger.records[integration.KindDatadog], names, unchanged)
	if err != nil || len(findings) != 1 ||
		findings[0].Kind != integration.FindingCredentialShared ||
		findings[0].Subject != "DATADOG_APP_KEY" ||
		strings.Contains(findings[0].Detail+findings[0].Remedy, "fake-company-key") {
		t.Errorf("findings = %+v, %v; want one naming DATADOG_APP_KEY, never its value",
			findings, err)
	}
	notice := accessmigrate.NoticeOf(integration.KindDatadog, findings[0])
	if notice.Tone != "danger" || !notice.ReplaceKey || !strings.Contains(notice.Body, "SRE") {
		t.Errorf("notice = %+v", notice)
	}
	replaced := func(context.Context, string) (time.Time, bool, error) {
		return time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), true, nil
	}
	if after, err := accessmigrate.SharedFindings(t.Context(), integration.KindDatadog,
		ledger.records[integration.KindDatadog], names, replaced); err != nil || len(after) != 0 {
		t.Errorf("after the variable was rewritten: %+v, %v; want none", after, err)
	}
	again, err := accessmigrate.Run(t.Context(), deps)
	if err != nil || again.Revision != "" || doc.writes != 1 {
		t.Errorf("a second run = %+v, %v after %d writes; want one revision in all", again,
			err, doc.writes)
	}
}

// --- fakes ---

type fakeSecrets struct {
	values map[string]string
	sealed map[string]string
}

func (f *fakeSecrets) Value(name string) (string, bool) {
	if v, ok := f.sealed[name]; ok {
		return v, true
	}
	v, ok := f.values[name]
	return v, ok
}

func (f *fakeSecrets) Seal(_ context.Context, name, value string, _ secrets.Author,
	_ time.Time) error {
	if f.sealed == nil {
		f.sealed = map[string]string{}
	}
	f.sealed[name] = value
	return nil
}

type fakeDocument struct {
	root       map[string]any
	revision   string
	sealed     *fakeSecrets
	ledger     *fakeLedger
	readyFirst bool
	writes     int
}

func (f *fakeDocument) Stored(context.Context) (map[string]any, string, error) {
	return f.root, f.revision, nil
}

func (f *fakeDocument) Rewrite(_ context.Context, expect, _ string, _ iam.Actor,
	transform func(map[string]any) (bool, error)) (string, bool, error) {
	if expect != f.revision {
		return "", false, errors.New("raced")
	}
	f.readyFirst = len(f.sealed.sealed) > 0 && len(f.ledger.records) > 0
	changed, err := transform(f.root)
	if err != nil || !changed {
		return "", false, err
	}
	f.writes++
	f.revision = "rev-" + string(rune('1'+f.writes))
	return f.revision, true, nil
}

type fakeLedger struct {
	records map[integration.Kind]map[string]seataccess.Record
}

func (f *fakeLedger) Records(_ context.Context, kind integration.Kind) (map[string]seataccess.Record, error) {
	return maps.Clone(f.records[kind]), nil
}

func (f *fakeLedger) Put(_ context.Context, rec seataccess.Record) error {
	if f.records[rec.Kind] == nil {
		f.records[rec.Kind] = map[string]seataccess.Record{}
	}
	f.records[rec.Kind][rec.Handle] = rec
	return nil
}

func (f *fakeLedger) Drop(_ context.Context, kind integration.Kind, handle string) error {
	delete(f.records[kind], handle)
	return nil
}

type fakeRecords struct{ ledger *fakeLedger }

func (f *fakeRecords) Ledger() seataccess.Ledger { return f.ledger }

// seat is one stored seat by handle.
func seat(t *testing.T, root map[string]any, handle string) map[string]any {
	t.Helper()
	for _, unit := range root["units"].([]any) {
		for _, s := range unit.(map[string]any)["roles"].([]any) {
			if m := s.(map[string]any); m["handle"] == handle {
				return m
			}
		}
	}
	t.Fatalf("no seat %s", handle)
	return nil
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A JIRA AND CONFLUENCE SEAT THAT WAS ON COMES OUT WITH PRODUCT ACCESS ONLY.
//
// The engine before per-seat access granted an Atlassian agent the products
// and no project or space role, so the migration writes exactly that: each
// lane on, with the empty level, which is the "Product access only" option the
// Agents tab offers. It invents no role, and the agent's token and address move
// to the engine's own names. Mutation: copy a role into the lanes, or leave
// them unwritten, and this fails.
func TestJiraAndConfluenceSeatsComeOutWithProductAccessOnly(t *testing.T) {
	t.Parallel()
	root := gophrase(t)
	integrations := root["integrations"].(map[string]any)
	integrations["atlassian"] = map[string]any{"deployment": "cloud", "org_id": "${ATLASSIAN_ORG_ID}",
		"api_key": "${ATLASSIAN_ORG_KEY}"}
	integrations["jira"] = map[string]any{"site_url": "https://gophrase.atlassian.net"}
	integrations["confluence"] = map[string]any{"site_url": "https://gophrase.atlassian.net/wiki"}
	pm := seat(t, root, "pm")
	pm["mcp_env"].(map[string]any)["atlassian"] = map[string]any{
		"JIRA_API_TOKEN": "${PM_ATLASSIAN_TOKEN}", "JIRA_USERNAME": "${PM_ATLASSIAN_EMAIL}"}
	m := plan(t, root, gophraseVendors())
	if !slices.Contains(m.Migrated, integration.KindAtlassian) {
		t.Fatalf("migrated %v, pending %v; want Atlassian migrated", m.Migrated, m.Pending)
	}
	for _, lane := range []seataccess.Lane{seataccess.LaneJira, seataccess.LaneConfluence} {
		if got := m.Seats["pm"][lane]; !got.Enabled || got.Level != "" || len(got.Scope) != 0 {
			t.Errorf("the PM on %s = %+v, want on with product access only", lane, got)
		}
		if got := m.Seats["qa"][lane]; got.Enabled {
			t.Errorf("the QA engineer on %s = %+v, want off: it held no Atlassian account", lane, got)
		}
	}
	moved := map[string]string{}
	for _, move := range m.Moves {
		if move.Kind == integration.KindAtlassian {
			moved[move.From] = move.To
		}
	}
	if !maps.Equal(moved, map[string]string{"PM_ATLASSIAN_TOKEN": "ATLASSIAN_TOKEN_PM",
		"PM_ATLASSIAN_EMAIL": "ATLASSIAN_EMAIL_PM"}) {
		t.Errorf("Atlassian moves = %v, want the token and address under the engine's names", moved)
	}
	if _, err := accessmigrate.Apply(root, m); err != nil {
		t.Fatal(err)
	}
	blocks := pm["integrations"].(map[string]any)
	for _, lane := range []string{"jira", "confluence"} {
		block, _ := blocks[lane].(map[string]any)
		if block["enabled"] != true || block["project_role"] != nil || block["space_role"] != nil ||
			block["operations"] != nil {
			t.Errorf("the PM's %s block = %v, want it written on, with no role", lane, block)
		}
	}
}
