package github_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// THE CATALOGUE THE VALIDATOR RESTATES IS THE ONE THIS PACKAGE OWNS: config
// is a leaf and cannot import it, so the two are held to one set here.
func TestTheConfigPermissionKeysAreGitHubsCatalogue(t *testing.T) {
	t.Parallel()
	var keys []string
	for _, perm := range github.SeatPermissions {
		keys = append(keys, perm.Key)
	}
	if !slices.Equal(keys, config.GitHubPermissionKeys()) {
		t.Errorf("github offers %v, config validates %v", keys, config.GitHubPermissionKeys())
	}
	vendor, ok := seataccess.Lookup(integration.KindGitHub)
	if !ok {
		t.Fatal("the GitHub vendor is not registered")
	}
	cat := vendor.Catalog()
	spec, _ := cat.Lane(seataccess.LaneGitHub)
	var admin, writeOnly []string
	for _, perm := range spec.Permissions {
		if perm.AdministersOnWrite {
			admin = append(admin, perm.Key)
		}
		if perm.WriteOnly {
			writeOnly = append(writeOnly, perm.Key)
		}
	}
	if !slices.Equal(admin, []string{"contents", "workflows"}) || !slices.Equal(writeOnly, []string{"workflows"}) {
		t.Errorf("administration %v, write only %v", admin, writeOnly)
	}
	// THE TOKEN THE SEAT ACTS WITH is the one the issuer answers.
	tool := seataccess.ToolFor(seataccess.LaneGitHub)
	if tool == nil || tool.Server != "github" || tool.Keys[0].Field != github.FieldAppToken ||
		tool.Keys[0].Header != "Authorization" || tool.Keys[0].Prefix != "Bearer " {
		t.Errorf("tool binding = %+v", tool)
	}
	if spec.Rotatable || spec.CreatedBy != seataccess.ByYou || spec.EnabledBy != seataccess.ByCrewlet {
		t.Errorf("lane = %+v", spec)
	}
}

// EVERY OP DRAWS GITHUB'S OWN STEPS, in order and with who takes each.
func TestStepsPerOp(t *testing.T) {
	t.Parallel()
	vendor, _ := seataccess.Lookup(integration.KindGitHub)
	seat := seataccess.SeatRef{Handle: "sre", Name: "SRE"}
	perms := func(kv ...string) seataccess.Permissions {
		out := seataccess.Permissions{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = config.GitHubAccess(kv[i+1])
		}
		return out
	}
	type drawn struct {
		verb seataccess.StepVerb
		by   seataccess.StepActor
	}
	crewlet, you := seataccess.ByCrewlet, seataccess.ByYou
	for name, tc := range map[string]struct {
		op   seataccess.Op
		want seataccess.Desired
		have seataccess.Observed
		out  []drawn
	}{
		"create is two clicks": {op: seataccess.OpCreate, out: []drawn{
			{seataccess.VerbCreate, you}, {seataccess.VerbApprove, you}}},
		"enable unsuspends": {op: seataccess.OpEnable, out: []drawn{{seataccess.VerbEnable, crewlet}}},
		"a person's suspension is lifted at GitHub": {op: seataccess.OpEnable,
			have: seataccess.Observed{Exists: true, DisabledBy: seataccess.DisabledByHand},
			out:  []drawn{{seataccess.VerbEnable, you}}},
		"a narrowing needs nothing at GitHub": {op: seataccess.OpLevel,
			want: seataccess.Desired{Permissions: perms("contents", "read")},
			have: seataccess.Observed{Permissions: perms("contents", "write"), Ceiling: perms("contents", "write")},
			out:  []drawn{{seataccess.VerbRerole, crewlet}}},
		"a raise within the registration waits on the organization": {op: seataccess.OpLevel,
			want: seataccess.Desired{Permissions: perms("contents", "write")},
			have: seataccess.Observed{Permissions: perms("contents", "read"), Ceiling: perms("contents", "write")},
			out:  []drawn{{seataccess.VerbRerole, crewlet}, {seataccess.VerbApprove, you}}},
		"a raise past the registration waits on the owner first": {op: seataccess.OpLevel,
			want: seataccess.Desired{Permissions: perms("contents", "write")},
			have: seataccess.Observed{Permissions: perms("contents", "read"), Ceiling: perms("contents", "read")},
			out: []drawn{{seataccess.VerbRerole, crewlet}, {seataccess.VerbGrant, you},
				{seataccess.VerbApprove, you}}},
		"a covered scope needs nothing at GitHub": {op: seataccess.OpScope,
			want: seataccess.Desired{Scope: []string{"acme/backend"}},
			have: seataccess.Observed{Scope: []string{"acme/backend"}},
			out:  []drawn{{seataccess.VerbScope, crewlet}}},
		"an uncovered repository waits on the organization": {op: seataccess.OpScope,
			want: seataccess.Desired{Scope: []string{"acme/backend", "acme/infra"}},
			have: seataccess.Observed{Scope: []string{"acme/backend"}},
			out:  []drawn{{seataccess.VerbScope, crewlet}, {seataccess.VerbScope, you}}},
		"back on at the document's setting": {op: seataccess.OpEnable,
			want: seataccess.Desired{Permissions: perms("contents", "write"), Scope: []string{"acme/infra"}},
			have: seataccess.Observed{Permissions: perms("contents", "read"), Ceiling: perms("contents", "read")},
			out: []drawn{{seataccess.VerbEnable, crewlet}, {seataccess.VerbGrant, you},
				{seataccess.VerbApprove, you}, {seataccess.VerbScope, you}}},
		"a lost key means a new app": {op: seataccess.OpMint, out: []drawn{{seataccess.VerbCreate, you}}},
		"a sealed key mints nothing": {op: seataccess.OpMint,
			have: seataccess.Observed{Credential: &seataccess.Credential{Variable: "GITHUB_APP_KEY_SRE"}}},
		"off keeps the key": {op: seataccess.OpRevoke},
		"off suspends":      {op: seataccess.OpDisable, out: []drawn{{seataccess.VerbDisable, crewlet}}},
		"remove ends with a person": {op: seataccess.OpRemove, out: []drawn{
			{seataccess.VerbRemove, crewlet}, {seataccess.VerbRemove, you}}},
		"nothing to rotate": {op: seataccess.OpRotate},
	} {
		steps := vendor.Steps(seat, seataccess.LaneGitHub, tc.op, tc.want, tc.have)
		var got []drawn
		for i, step := range steps {
			got = append(got, drawn{step.Verb, step.By})
			if step.Op != tc.op || step.Index != i || step.Text == "" {
				t.Errorf("%s: step %d = %+v", name, i, step)
			}
		}
		if !slices.Equal(got, tc.out) {
			t.Errorf("%s: drew %v, want %v", name, got, tc.out)
		}
	}
}

// ghSeat is a GitHub world for one seat: a fake GitHub, the company block
// pointing at it, and the seat's sealed key.
type ghSeat struct {
	gh     *fakeApp
	cfg    *config.Company
	env    *config.Resolver
	vendor seataccess.Vendor
}

func newGHSeat(t *testing.T, block config.RoleGitHub) *ghSeat {
	t.Helper()
	gh := newFakeApp(t)
	_, pem := testKey(t)
	on := true
	if block.Enabled == nil {
		block.Enabled = &on
	}
	cfg := &config.Company{
		Name: "acme",
		Integrations: config.Integrations{GitHub: &config.GitHub{
			Enabled: true, URL: gh.URL,
			Provisioning: &config.GitHubProvisioning{Org: "acme", Repos: []string{"acme/backend", "acme/infra"}},
		}},
		Roles: []config.Role{{
			Name: "SRE", Handle: "sre",
			Integrations: config.RoleIntegrations{GitHub: &block},
		}},
	}
	vendor, _ := seataccess.Lookup(integration.KindGitHub)
	return &ghSeat{
		gh: gh, cfg: cfg, vendor: vendor,
		env: config.NewResolver(config.MapSource{"GITHUB_APP_KEY_SRE": pem}),
	}
}

func (w *ghSeat) open(t *testing.T) seataccess.Session {
	t.Helper()
	session, err := w.vendor.Open(t.Context(), seataccess.OpenInput{Config: w.cfg, Env: w.env})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

// installedSRE is a seat whose app is registered for Contents read and
// Issues write, installed on selected repositories, holding Issues read.
func installedSRE(t *testing.T) *ghSeat {
	t.Helper()
	w := newGHSeat(t, config.RoleGitHub{
		Permissions: map[string]config.GitHubAccess{"contents": "read", "issues": "write"},
		Repos:       []string{"acme/backend"},
		AppID:       41, AppSlug: "acme-sre", InstallationID: 7,
		PrivateKey: "${GITHUB_APP_KEY_SRE}",
	})
	w.gh.on("GET /app", 200, `{"id":41,"slug":"acme-sre","permissions":{"contents":"read","issues":"write","metadata":"read"}}`)
	w.gh.on("GET /app/installations/7", 200, `{"id":7,"account":{"login":"acme"},
		"repository_selection":"selected","html_url":"https://github.com/organizations/acme/settings/installations/7",
		"permissions":{"contents":"read","issues":"read","metadata":"read"}}`)
	w.gh.on("POST /app/installations/7/access_tokens", 201, `{"token":"ghs_listing"}`)
	w.gh.on("GET /installation/repositories?per_page=100&page=1", 200,
		`{"total_count":1,"repositories":[{"full_name":"acme/backend"}]}`)
	w.gh.on("DELETE /installation/token", 204, ``)
	return w
}

var sre = seataccess.SeatRef{Handle: "sre", Name: "SRE"}

// OBSERVED PERMISSIONS ARE WHAT THE SEAT'S TOKENS CARRY: the lower of the
// seat's setting and the installation's grant, with the registration as the
// ceiling a raise is measured against.
func TestObserveReportsWhatTheTokensCarry(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	obs, err := w.open(t).Observe(t.Context(), seataccess.ObserveInput{Seats: []seataccess.SeatRef{sre}})
	if err != nil {
		t.Fatal(err)
	}
	got := obs["sre"][seataccess.LaneGitHub]
	if !got.Exists || !got.Enabled || got.AccountID != "41" || got.Account != "acme-sre" {
		t.Fatalf("observed %+v", got)
	}
	if !got.Permissions.Equal(seataccess.Permissions{"contents": "read", "issues": "read"}) {
		t.Errorf("permissions = %v, want the installation's lower Issues", got.Permissions)
	}
	if !got.Ceiling.Equal(seataccess.Permissions{"contents": "read", "issues": "write"}) {
		t.Errorf("ceiling = %v", got.Ceiling)
	}
	if !slices.Equal(got.Scope, []string{"acme/backend"}) {
		t.Errorf("scope = %v", got.Scope)
	}
	if got.Credential == nil || got.Credential.Variable != "GITHUB_APP_KEY_SRE" {
		t.Errorf("credential = %+v", got.Credential)
	}
}

// A SUSPENSION IS TOLD APART BY THE LEDGER (D3): this engine's own is
// crewlet's, any other is somebody's at GitHub.
func TestASuspensionIsAttributedByTheLedger(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		state seataccess.AccountState
		want  seataccess.DisabledBy
	}{
		"suspended by this engine": {seataccess.AccountDisabled, seataccess.DisabledByCrewlet},
		"suspended at GitHub":      {seataccess.AccountActive, seataccess.DisabledByHand},
	} {
		w := installedSRE(t)
		w.gh.on("GET /app/installations/7", 200, `{"id":7,"account":{"login":"acme"},
			"repository_selection":"all","suspended_at":"2026-10-29T10:00:00Z",
			"permissions":{"contents":"read","issues":"write"}}`)
		obs, err := w.open(t).Observe(t.Context(), seataccess.ObserveInput{
			Seats: []seataccess.SeatRef{sre},
			Records: map[string]seataccess.Record{"sre": {
				Kind: integration.KindGitHub, Handle: "sre",
				Account: &seataccess.Account{ID: "41", State: tc.state},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		got := obs["sre"][seataccess.LaneGitHub]
		if got.Enabled || got.DisabledBy != tc.want {
			t.Errorf("%s: enabled %v, disabled by %q", name, got.Enabled, got.DisabledBy)
		}
	}
}

// A SEAT THAT LEFT THE DOCUMENT IS STILL REACHED, by the ledger's app id and
// the engine-named key, so it can be switched off.
func TestADepartedSeatIsObservedThroughTheLedger(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	w.cfg.Roles = nil
	w.gh.on("GET /app/installations?per_page=100&page=1", 200, `[{"id":7,"account":{"login":"acme"},
		"repository_selection":"all","permissions":{"contents":"read"}}]`)
	obs, err := w.open(t).Observe(t.Context(), seataccess.ObserveInput{
		Records: map[string]seataccess.Record{"sre": {
			Kind: integration.KindGitHub, Handle: "sre",
			Account: &seataccess.Account{ID: "41", Name: "acme-sre", State: seataccess.AccountActive},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := obs["sre"][seataccess.LaneGitHub]; !got.Exists || got.AccountID != "41" {
		t.Errorf("a departed seat observed %+v", got)
	}
}

func apply(t *testing.T, s seataccess.Session, op seataccess.Op, index int, want seataccess.Desired,
	record *seataccess.Record,
) (seataccess.StepResult, error) {
	t.Helper()
	vendor, _ := seataccess.Lookup(integration.KindGitHub)
	steps := vendor.Steps(sre, seataccess.LaneGitHub, op, want, seataccess.Observed{})
	step := seataccess.Step{Op: op, Index: index}
	if index < len(steps) {
		step = steps[index]
	}
	return s.Apply(t.Context(), seataccess.ApplyInput{
		Seat: sre, Lane: seataccess.LaneGitHub, Step: step, Want: want, Record: record,
	})
}

// OFF AND BACK ON ARE THE INSTALLATION'S SUSPENSION, and off is recorded as
// this engine's so back on may lift it.
func TestOffSuspendsAndBackOnUnsuspends(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	w.gh.on("PUT /app/installations/7/suspended", 204, ``)
	w.gh.on("DELETE /app/installations/7/suspended", 204, ``)
	s := w.open(t)
	res, err := apply(t, s, seataccess.OpDisable, 0, seataccess.Desired{}, nil)
	if err != nil || !res.Disabled {
		t.Fatalf("off: %+v, %v", res, err)
	}
	// GitHub now reports the installation suspended, which back on reads.
	w.gh.on("GET /app/installations/7", 200, `{"id":7,"account":{"login":"acme"},"repository_selection":"all",
		"suspended_at":"2026-10-29T10:00:00Z","permissions":{"contents":"read","issues":"write"}}`)
	if _, err := apply(t, s, seataccess.OpEnable, 0, seataccess.Desired{Enabled: true}, nil); err != nil {
		t.Fatalf("back on: %v", err)
	}
	calls := w.gh.called()
	if !slices.Contains(calls, "PUT /app/installations/7/suspended") ||
		!slices.Contains(calls, "DELETE /app/installations/7/suspended") {
		t.Errorf("calls = %v", calls)
	}
}

// A REFUSAL NAMES GITHUB'S REASON, THE REMEDY AND WHO ACTS; a fault GitHub
// did not answer is not dressed up as one.
func TestARefusedStepSaysWhyAndWhoActs(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	w.gh.on("PUT /app/installations/7/suspended", 403, `{"message":"Resource not accessible by integration"}`)
	_, err := apply(t, w.open(t), seataccess.OpDisable, 0, seataccess.Desired{}, nil)
	var refusal *seataccess.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a refusal answered %v", err)
	}
	if !strings.Contains(refusal.Said, "Resource not accessible by integration") ||
		refusal.Remedy == "" || refusal.Actor != seataccess.ByYou || refusal.ActionURL == "" {
		t.Errorf("refusal = %+v", refusal)
	}

	faulty := installedSRE(t)
	faulty.gh.on("PUT /app/installations/7/suspended", 502, `{"message":"bad gateway"}`)
	_, err = apply(t, faulty.open(t), seataccess.OpDisable, 0, seataccess.Desired{}, nil)
	if err == nil || errors.As(err, &refusal) {
		t.Errorf("a 502 answered %v, want a plain fault to retry", err)
	}
}

// A RAISE WAITS ON THE RIGHT PERSON, and "Done, check again" re-reads GitHub.
func TestARaiseWaitsOnTheOwnerThenTheOrganization(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	s := w.open(t)
	want := seataccess.Desired{Enabled: true, Permissions: seataccess.Permissions{"contents": "write", "issues": "write"}}

	// Contents write is past the registration: the owner's step waits on
	// the app's permissions page.
	res, err := s.Apply(t.Context(), seataccess.ApplyInput{
		Seat: sre, Lane: seataccess.LaneGitHub, Want: want,
		Step: seataccess.Step{Op: seataccess.OpLevel, Verb: seataccess.VerbGrant, By: seataccess.ByYou, Index: 1},
	})
	if err != nil || res.Waiting == nil || !strings.HasSuffix(res.Waiting.ActionURL, "/settings/apps/acme-sre/permissions") {
		t.Fatalf("the owner's step: %+v, %v", res.Waiting, err)
	}

	// The owner adds it: the owner's step is done, and the organization's
	// acceptance waits on the installation's own page.
	w.gh.on("GET /app", 200, `{"id":41,"permissions":{"contents":"write","issues":"write","metadata":"read"}}`)
	res, err = s.Apply(t.Context(), seataccess.ApplyInput{
		Seat: sre, Lane: seataccess.LaneGitHub, Want: want,
		Step: seataccess.Step{Op: seataccess.OpLevel, Verb: seataccess.VerbGrant, By: seataccess.ByYou, Index: 1},
	})
	if err != nil || res.Waiting != nil {
		t.Fatalf("the owner's step after the owner acted: %+v, %v", res.Waiting, err)
	}
	accept := seataccess.Step{Op: seataccess.OpLevel, Verb: seataccess.VerbApprove, By: seataccess.ByYou, Index: 2}
	res, err = s.Apply(t.Context(), seataccess.ApplyInput{Seat: sre, Lane: seataccess.LaneGitHub, Want: want, Step: accept})
	if err != nil || res.Waiting == nil || res.Waiting.ActionURL != "https://github.com/organizations/acme/settings/installations/7" {
		t.Fatalf("the organization's step: %+v, %v", res.Waiting, err)
	}

	// The organization accepts: done.
	w.gh.on("GET /app/installations/7", 200, `{"id":7,"account":{"login":"acme"},"repository_selection":"all",
		"permissions":{"contents":"write","issues":"write","metadata":"read"}}`)
	res, err = s.Apply(t.Context(), seataccess.ApplyInput{Seat: sre, Lane: seataccess.LaneGitHub, Want: want, Step: accept})
	if err != nil || res.Waiting != nil {
		t.Errorf("the organization's step after it accepted: %+v, %v", res.Waiting, err)
	}

	// A narrowing takes no call at all.
	before := len(w.gh.called())
	res, err = apply(t, s, seataccess.OpLevel, 0, seataccess.Desired{Permissions: seataccess.Permissions{"issues": "read"}}, nil)
	if err != nil || res.Waiting != nil || len(w.gh.called()) != before {
		t.Errorf("a narrowing called GitHub or waited: %+v, %v", res, err)
	}
}

// A REPOSITORY THE INSTALLATION DOES NOT COVER waits on the organization.
func TestAnUncoveredRepositoryWaitsOnTheInstallation(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	want := seataccess.Desired{Enabled: true, Scope: []string{"acme/backend", "acme/infra"}}
	res, err := w.open(t).Apply(t.Context(), seataccess.ApplyInput{
		Seat: sre, Lane: seataccess.LaneGitHub, Want: want,
		Step: seataccess.Step{Op: seataccess.OpScope, Verb: seataccess.VerbScope, By: seataccess.ByYou, Index: 1},
	})
	if err != nil || res.Waiting == nil || !strings.Contains(res.Waiting.Detail, "acme/infra") {
		t.Fatalf("waiting = %+v, %v", res.Waiting, err)
	}
}

// CREATE IS TWO PEOPLE'S CLICKS, each read back from GitHub.
func TestCreateWaitsForTheAppAndThenItsInstall(t *testing.T) {
	t.Parallel()
	none := newGHSeat(t, config.RoleGitHub{})
	res, err := apply(t, none.open(t), seataccess.OpCreate, 0, seataccess.Desired{Enabled: true}, nil)
	if err != nil || res.Waiting == nil || res.Waiting.ActionURL != "" {
		t.Fatalf("no app yet: %+v, %v", res, err)
	}

	created := newGHSeat(t, config.RoleGitHub{AppID: 41, AppSlug: "acme-sre", PrivateKey: "${GITHUB_APP_KEY_SRE}"})
	created.gh.on("GET /app", 200, `{"id":41,"slug":"acme-sre","permissions":{}}`)
	created.gh.on("GET /app/installations?per_page=100&page=1", 200, `[]`)
	s := created.open(t)
	res, err = apply(t, s, seataccess.OpCreate, 0, seataccess.Desired{Enabled: true}, nil)
	if err != nil || res.Waiting != nil || res.AccountID != "41" || res.Account != "acme-sre" {
		t.Fatalf("the app exists: %+v, %v", res, err)
	}
	res, err = apply(t, s, seataccess.OpCreate, 1, seataccess.Desired{Enabled: true}, nil)
	if err != nil || res.Waiting == nil || !strings.HasSuffix(res.Waiting.ActionURL, "/settings/apps/acme-sre/installations") {
		t.Fatalf("not installed: %+v, %v", res.Waiting, err)
	}
}

// REMOVE UNINSTALLS, waits on the registration's deletion, which only a
// person can make, and hands the sealed key back for clearing once GitHub
// reports the registration gone.
func TestRemoveUninstallsAndWaitsOnTheRegistration(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	w.gh.on("DELETE /app/installations/7", 204, ``)
	s := w.open(t)
	res, err := apply(t, s, seataccess.OpRemove, 0, seataccess.Desired{}, nil)
	if err != nil || len(res.Revoked) != 0 || res.Gone {
		t.Fatalf("uninstall: %+v, %v", res, err)
	}
	if !slices.Contains(w.gh.called(), "DELETE /app/installations/7") {
		t.Errorf("nothing was uninstalled: %v", w.gh.called())
	}
	res, err = apply(t, s, seataccess.OpRemove, 1, seataccess.Desired{}, nil)
	if err != nil || res.Waiting == nil || !strings.HasSuffix(res.Waiting.ActionURL, "/settings/apps/acme-sre/advanced") {
		t.Fatalf("the registration: %+v, %v", res.Waiting, err)
	}
	w.gh.on("GET /app", 404, `{"message":"Integration not found"}`)
	res, err = apply(t, s, seataccess.OpRemove, 1, seataccess.Desired{}, nil)
	if err != nil || !res.Gone || !slices.Equal(res.Revoked, []string{github.FieldAppKey, github.FieldAppWebhookSecret}) {
		t.Errorf("a deleted registration: %+v, %v", res, err)
	}
}

// AN UNINSTALLED APP IS AN ACCOUNT THAT IS OFF, never one that is gone: gone
// clears the sealed key, and GitHub issues it once.
func TestAnUninstalledAppIsOffNotGone(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	w.gh.on("GET /app/installations/7", 404, `{"message":"Not Found"}`)
	w.gh.on("GET /app/installations?per_page=100&page=1", 200, `[]`)
	obs, err := w.open(t).Observe(t.Context(), seataccess.ObserveInput{Seats: []seataccess.SeatRef{sre}})
	if err != nil {
		t.Fatal(err)
	}
	got := obs["sre"][seataccess.LaneGitHub]
	if !got.Exists || got.Enabled || got.Credential == nil {
		t.Errorf("an uninstalled app observed %+v", got)
	}
	res, err := apply(t, w.open(t), seataccess.OpEnable, 0, seataccess.Desired{Enabled: true}, nil)
	if err != nil || res.Waiting == nil || !strings.HasSuffix(res.Waiting.ActionURL, "/installations") || res.AccountID != "41" {
		t.Errorf("back on: %+v, %v", res, err)
	}
}

// A LOST KEY CANNOT BE MINTED AGAIN, so the step waits on a new app; a key
// that resolves is adopted into the ledger.
func TestMintAdoptsASealedKeyAndWaitsOnALostOne(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	res, err := apply(t, w.open(t), seataccess.OpMint, 0, seataccess.Desired{Enabled: true}, nil)
	if err != nil || res.Waiting != nil || res.AccountID != "41" {
		t.Errorf("a sealed key: %+v, %v", res, err)
	}
	lost := installedSRE(t)
	lost.env = config.NewResolver(config.MapSource{})
	res, err = apply(t, lost.open(t), seataccess.OpMint, 0, seataccess.Desired{Enabled: true}, nil)
	if err != nil || res.Waiting == nil || !strings.HasSuffix(res.Waiting.ActionURL, "/advanced") {
		t.Errorf("a lost key: %+v, %v", res.Waiting, err)
	}
}

// A COMPANY THAT DOES NOT CONNECT GITHUB IS NOT A FAULT, and neither is one
// whose agents' apps are handed in from outside Crewlet (D4b): the access sync
// opens no session for either.
func TestOpenWithoutGitHubIsNotConfigured(t *testing.T) {
	t.Parallel()
	vendor, _ := seataccess.Lookup(integration.KindGitHub)
	_, err := vendor.Open(t.Context(), seataccess.OpenInput{Config: &config.Company{}})
	if !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("open answered %v", err)
	}
	external := installedSRE(t)
	external.cfg.Integrations.GitHub.Provisioning = nil
	if _, err := vendor.Open(t.Context(), seataccess.OpenInput{Config: external.cfg, Env: external.env}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("a company whose apps are external opened a session: %v", err)
	}
}

// suspendedSRE is installedSRE with its installation suspended, by who.
func suspendedSRE(t *testing.T, by string) *ghSeat {
	t.Helper()
	w := installedSRE(t)
	w.gh.on("GET /app/installations/7", 200, `{"id":7,"account":{"login":"acme"},
		"repository_selection":"selected","suspended_at":"2026-10-29T10:00:00Z",`+by+`
		"html_url":"https://github.com/organizations/acme/settings/installations/7",
		"permissions":{"contents":"read","issues":"read","metadata":"read"}}`)
	return w
}

// A SUSPENDED INSTALLATION IS READ WITHOUT A TOKEN. Listing its repositories
// mints one, which GitHub refuses while it is suspended, and that refusal
// failed the whole kind, so the seat could never be turned back on. What it
// covers is unknown, never missing; back on unsuspends and reads again.
func TestASuspendedInstallationIsReadWithoutAToken(t *testing.T) {
	t.Parallel()
	w := suspendedSRE(t, `"suspended_by":{"login":"acme-sre[bot]","type":"Bot"},`)
	w.gh.on("POST /app/installations/7/access_tokens", 403, `{"message":"This installation has been suspended"}`)
	w.gh.on("DELETE /app/installations/7/suspended", 204, ``)
	s := w.open(t)
	disabled := map[string]seataccess.Record{"sre": {Kind: integration.KindGitHub, Handle: "sre",
		Account: &seataccess.Account{ID: "41", State: seataccess.AccountDisabled}}}
	obs, err := s.Observe(t.Context(), seataccess.ObserveInput{Seats: []seataccess.SeatRef{sre}, Records: disabled})
	if err != nil {
		t.Fatalf("a suspended installation failed the read: %v", err)
	}
	got := obs["sre"][seataccess.LaneGitHub]
	if got.Enabled || got.DisabledBy != seataccess.DisabledByCrewlet || !slices.Equal(got.Scope, []string{"acme/backend"}) {
		t.Errorf("observed %+v", got)
	}
	if slices.Contains(w.gh.called(), "POST /app/installations/7/access_tokens") {
		t.Error("a suspended installation was asked for a token")
	}

	rec := disabled["sre"]
	if _, err := apply(t, s, seataccess.OpEnable, 0, seataccess.Desired{Enabled: true}, &rec); err != nil {
		t.Fatalf("back on: %v", err)
	}
	calls := w.gh.called()
	lifted := slices.Index(calls, "DELETE /app/installations/7/suspended")
	if lifted < 0 || !slices.Contains(calls[lifted:], "GET /app/installations/7") {
		t.Errorf("back on did not read the installation again after lifting it: %v", calls)
	}
}

// A PERSON'S SUSPENSION IS THEIRS TO LIFT: an owner suspended the installation
// at GitHub, so back on waits for a person at the installation's page rather
// than lifting a decision made there.
func TestAnOwnersSuspensionWaitsOnAPerson(t *testing.T) {
	t.Parallel()
	w := suspendedSRE(t, `"suspended_by":{"login":"octo-owner","type":"User"},`)
	s := w.open(t)
	disabled := map[string]seataccess.Record{"sre": {Kind: integration.KindGitHub, Handle: "sre",
		Account: &seataccess.Account{ID: "41", State: seataccess.AccountDisabled}}}
	obs, err := s.Observe(t.Context(), seataccess.ObserveInput{Seats: []seataccess.SeatRef{sre}, Records: disabled})
	if err != nil {
		t.Fatal(err)
	}
	if got := obs["sre"][seataccess.LaneGitHub]; got.DisabledBy != seataccess.DisabledByHand {
		t.Errorf("an owner's suspension is attributed to %q", got.DisabledBy)
	}
	rec := disabled["sre"]
	for _, by := range []seataccess.StepActor{seataccess.ByCrewlet, seataccess.ByYou} {
		res, err := s.Apply(t.Context(), seataccess.ApplyInput{
			Seat: sre, Lane: seataccess.LaneGitHub, Record: &rec, Want: seataccess.Desired{Enabled: true},
			Step: seataccess.Step{Op: seataccess.OpEnable, Verb: seataccess.VerbEnable, By: by},
		})
		if err != nil || res.Waiting == nil || !strings.Contains(res.Waiting.Detail, "octo-owner") ||
			res.Waiting.ActionURL != "https://github.com/organizations/acme/settings/installations/7" {
			t.Errorf("%s: %+v, %v", by, res.Waiting, err)
		}
	}
	if slices.Contains(w.gh.called(), "DELETE /app/installations/7/suspended") {
		t.Error("this engine lifted a suspension a person placed")
	}
}

// ONE AGENT'S APP, ONE AGENT'S FAULT: a read GitHub refused for one seat is
// that seat's, and the others are observed.
func TestOneUnreadableAppIsThatSeatsFault(t *testing.T) {
	t.Parallel()
	w := installedSRE(t)
	pem, _ := w.env.LookupOK("GITHUB_APP_KEY_SRE")
	on := true
	w.cfg.Roles = append(w.cfg.Roles, config.Role{Name: "Ops", Handle: "ops",
		Integrations: config.RoleIntegrations{GitHub: &config.RoleGitHub{
			Enabled: &on, AppID: 42, AppSlug: "acme-ops", InstallationID: 8,
			PrivateKey: "${GITHUB_APP_KEY_OPS}",
		}}})
	w.env = config.NewResolver(config.MapSource{"GITHUB_APP_KEY_SRE": pem, "GITHUB_APP_KEY_OPS": pem})
	w.gh.on("GET /app/installations/8", 502, `{"message":"Bad Gateway"}`)
	obs, err := w.open(t).Observe(t.Context(), seataccess.ObserveInput{
		Seats: []seataccess.SeatRef{sre, {Handle: "ops", Name: "Ops"}},
	})
	faults, ok := errors.AsType[seataccess.SeatFaults](err)
	if !ok || len(faults) != 1 || faults["ops"] == nil {
		t.Fatalf("observe answered %v", err)
	}
	if got := obs["sre"][seataccess.LaneGitHub]; !got.Exists || !got.Enabled {
		t.Errorf("the readable seat observed %+v", got)
	}
}

// AN OFF SEAT IS ISSUED NO TOKEN: the suspension is the access sync's, and the
// issuer refuses before asking GitHub.
func TestAnOffSeatIsIssuedNoToken(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	seat := github.SeatApp{
		Handle: "sre", AppID: 41, Slug: "acme-sre", InstallationID: 7, Key: pem, Off: true,
		Permissions: tierPerms("read_only", false),
	}
	if _, err := (&github.Issuer{}).Token(t.Context(), github.SeatAppOptions{}, seat, github.ToolTokenLife); !errors.Is(err, github.ErrSeatOff) {
		t.Errorf("an off seat was issued %v", err)
	}
}

// ADOPTION KEEPS THE DOCUMENT IN STEP AND REPORTS NOTHING: an installation a
// person made is recorded, an app GitHub no longer has is forgotten, and a
// dry run writes nothing.
func TestAdoptSeatAppsKeepsTheRecordInStep(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	gh := newFakeApp(t)
	gh.on("GET /app/installations?per_page=100&page=1", 200, `[{"id":7,"account":{"login":"acme"}}]`)
	recorded := map[string]int64{}
	var forgotten []string
	opts := github.SeatAppOptions{
		APIBase: gh.URL, Org: "acme",
		Seats: []github.SeatApp{
			{Handle: "sre", AppID: 41, Key: pem},
			{Handle: "off", AppID: 41, Key: pem, Off: true},
		},
		Record: func(_ context.Context, handle string, id int64) error { recorded[handle] = id; return nil },
		Forget: func(_ context.Context, handle string) error { forgotten = append(forgotten, handle); return nil },
	}
	if err := github.AdoptSeatApps(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	if recorded["sre"] != 7 || recorded["off"] != 7 || len(forgotten) != 0 {
		t.Errorf("recorded %v, forgot %v", recorded, forgotten)
	}

	gone := newFakeApp(t)
	gone.on("GET /app/installations?per_page=100&page=1", 200, `[]`)
	opts.APIBase, opts.Seats = gone.URL, []github.SeatApp{{Handle: "sre", AppID: 41, Key: pem}}
	if err := github.AdoptSeatApps(t.Context(), opts); err != nil || !slices.Equal(forgotten, []string{"sre"}) {
		t.Errorf("a deleted app: forgot %v, %v", forgotten, err)
	}

	dry := opts
	dry.Record, dry.Forget = nil, nil
	before := len(gone.called())
	if err := github.AdoptSeatApps(t.Context(), dry); err != nil || len(gone.called()) != before {
		t.Errorf("a dry run called GitHub or failed: %v", err)
	}
}
