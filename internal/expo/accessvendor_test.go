package expo_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

func vendor(t *testing.T) seataccess.Vendor {
	t.Helper()
	v, ok := seataccess.Lookup(integration.KindExpo)
	if !ok {
		t.Fatal("the Expo vendor is not registered")
	}
	return v
}

// THE CATALOG IS ONE LANE OF FIXED, UNSCOPED LEVELS with one rotatable token,
// sealed under the engine's name for the seat.
func TestTheExpoCatalog(t *testing.T) {
	t.Parallel()
	cat := vendor(t).Catalog()
	spec, ok := cat.Lane(seataccess.LaneExpo)
	if !ok || !spec.Levels || spec.LiveLevels || spec.Scoped || !spec.Rotatable ||
		spec.CreatedBy != seataccess.ByCrewlet || spec.EnabledBy != seataccess.ByCrewlet {
		t.Fatalf("lane spec = %+v", spec)
	}
	if !slices.Equal(cat.Credentials, []string{expo.TokenField}) {
		t.Fatalf("credentials = %v", cat.Credentials)
	}
	if got := seataccess.SinkName(integration.KindExpo, expo.TokenField, "qa"); got != "EXPO_TOKEN_QA" {
		t.Fatalf("sink = %q", got)
	}
	if !slices.Equal(config.SeatSealedFields("expo"), cat.Credentials) {
		t.Fatalf("config restates %v, the catalog %v", config.SeatSealedFields("expo"), cat.Credentials)
	}
}

// THE ONE TABLE: every level the document offers, least access first, each
// strictly more than the one before, and none ever Admin or Own.
func TestTheExpoLevelTable(t *testing.T) {
	t.Parallel()
	if !slices.Equal(expo.Levels(), config.ExpoLevels()) {
		t.Fatalf("levels = %v, want the document's %v", expo.Levels(), config.ExpoLevels())
	}
	names := []string{"Viewer", "Runner", "Developer", "Release Manager"}
	var prev []expo.Permission
	for i, id := range expo.Levels() {
		perms, ok := expo.Permissions(id)
		if !ok || len(perms) == 0 {
			t.Fatalf("%s grants nothing", id)
		}
		if got := expo.LevelName(id); got != names[i] {
			t.Errorf("%s is named %q, want %q", id, got, names[i])
		}
		if expo.LevelNote(id) == "" {
			t.Errorf("%s has no note", id)
		}
		for _, p := range perms {
			if !p.Valid() || p == expo.PermissionAdmin || p == expo.PermissionOwn {
				t.Errorf("%s grants %s", id, p)
			}
		}
		for _, p := range prev {
			if !slices.Contains(perms, p) {
				t.Errorf("%s drops %s, which the level below grants", id, p)
			}
		}
		if len(perms) <= len(prev) {
			t.Errorf("%s grants no more than the level below", id)
		}
		prev = perms
	}
	if _, ok := expo.Permissions("admin"); ok {
		t.Error("a level the document does not offer resolved")
	}
}

// EVERY OP IS DRAWN IN EXPO'S WORDS, naming the level and the sealed name, in
// the order Apply takes it; a scope change takes no step.
func TestExpoStepsForEveryOp(t *testing.T) {
	t.Parallel()
	v := vendor(t)
	seat := seataccess.SeatRef{Handle: "qa", Name: "QA"}
	want := seataccess.Desired{Enabled: true, Level: config.ExpoLevelDeveloper}
	verbs := func(op seataccess.Op, have seataccess.Observed) ([]seataccess.StepVerb, string) {
		var out []seataccess.StepVerb
		var text []string
		for i, s := range v.Steps(seat, seataccess.LaneExpo, op, want, have) {
			if s.Index != i || s.Op != op || s.By != seataccess.ByCrewlet {
				t.Errorf("%s step %d = %+v", op, i, s)
			}
			out = append(out, s.Verb)
			text = append(text, s.Text)
		}
		return out, strings.Join(text, " | ")
	}
	sealed := &seataccess.Credential{Variable: "EXPO_TOKEN_QA"}
	for _, tc := range []struct {
		op   seataccess.Op
		have seataccess.Observed
		want []seataccess.StepVerb
		says string
	}{
		{seataccess.OpCreate, seataccess.Observed{}, []seataccess.StepVerb{seataccess.VerbCreate, seataccess.VerbMint}, "Developer"},
		{seataccess.OpEnable, seataccess.Observed{Credential: sealed}, []seataccess.StepVerb{seataccess.VerbEnable}, "Developer"},
		{seataccess.OpEnable, seataccess.Observed{}, []seataccess.StepVerb{
			seataccess.VerbEnable, seataccess.VerbMint, seataccess.VerbRevoke}, "EXPO_TOKEN_QA"},
		{seataccess.OpLevel, seataccess.Observed{}, []seataccess.StepVerb{seataccess.VerbRerole}, "Developer"},
		{seataccess.OpMint, seataccess.Observed{}, []seataccess.StepVerb{seataccess.VerbMint, seataccess.VerbRevoke}, "EXPO_TOKEN_QA"},
		{seataccess.OpRotate, seataccess.Observed{}, []seataccess.StepVerb{seataccess.VerbMint, seataccess.VerbRevoke}, "new access token"},
		{seataccess.OpDisable, seataccess.Observed{}, []seataccess.StepVerb{seataccess.VerbDisable}, "delete its robot"},
		{seataccess.OpRevoke, seataccess.Observed{}, []seataccess.StepVerb{seataccess.VerbRevoke}, "EXPO_TOKEN_QA"},
		{seataccess.OpRemove, seataccess.Observed{}, []seataccess.StepVerb{seataccess.VerbRemove}, "stop managing"},
		{seataccess.OpScope, seataccess.Observed{}, nil, ""},
	} {
		got, text := verbs(tc.op, tc.have)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: verbs %v, want %v", tc.op, got, tc.want)
		}
		if !strings.Contains(text, tc.says) {
			t.Errorf("%s: %q does not say %q", tc.op, text, tc.says)
		}
	}
}

// OPTIONS ARE THE FOUR LEVELS UNDER EXPO'S NAMES, not read from Expo.
func TestExpoOptionsAreTheFixedLevels(t *testing.T) {
	t.Parallel()
	f := newFakeExpo()
	w := newSyncWorld(t, "")
	sess, err := expo.OpenAccess(context.Background(), vendor(t), seataccess.OpenInput{
		Config: w.company,
		Env: config.NewResolver(config.MapSource{
			"EXPO_ACCOUNT": testAccountName, "EXPO_ACCESS_TOKEN": provisioner,
		}),
	}, f.server(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := sess.Options(context.Background(), seataccess.LaneExpo)
	if err != nil || opts.Live || !opts.Rotatable || len(opts.Levels) != 4 {
		t.Fatalf("options = %+v, %v", opts, err)
	}
	if opts.Levels[3].ID != config.ExpoLevelReleaseManager || opts.Levels[3].Name != "Release Manager" ||
		opts.Levels[3].Administers {
		t.Errorf("release manager = %+v", opts.Levels[3])
	}
	if len(f.operations()) != 0 {
		t.Errorf("options called Expo: %v", f.operations())
	}
}

// A COMPANY THAT DOES NOT PROVISION EXPO OPENS NO SESSION, and says so as
// not configured rather than as a fault.
func TestExpoOpenNeedsAnEnabledProvisionedBlock(t *testing.T) {
	t.Parallel()
	env := config.NewResolver(config.MapSource{"EXPO_ACCOUNT": testAccountName})
	for name, c := range map[string]*config.Company{
		"no document": nil,
		"absent":      {},
		"off":         {Integrations: config.Integrations{Expo: &config.Expo{Provisioning: &config.ExpoProvisioning{}}}},
		"unresolved token": {Integrations: config.Integrations{Expo: &config.Expo{Enabled: true,
			Provisioning: &config.ExpoProvisioning{AccountName: "${EXPO_ACCOUNT}", AccessToken: "${EXPO_ACCESS_TOKEN}"}}}},
	} {
		_, err := vendor(t).Open(context.Background(), seataccess.OpenInput{Config: c, Env: env})
		if !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
	}
}

// A ROBOT'S NAME AND ITS TOKENS' NOTE SAY WHICH AGENT OF WHICH COMPANY, and
// label rather than identify.
func TestExpoRobotNamesAndTokenNotes(t *testing.T) {
	t.Parallel()
	if got := expo.RobotName("Acme Corp", "qa"); got != "crewlet-acme-corp-qa" {
		t.Errorf("robot name = %q", got)
	}
	if got := expo.RobotName("", "qa"); got != "crewlet-qa" {
		t.Errorf("robot name with no company = %q", got)
	}
	if got := expo.TokenNote(" qa "); got != "crewlet seat qa" {
		t.Errorf("token note = %q", got)
	}
	if got := expo.AccessTokensURL("example-org"); got != "https://expo.dev/accounts/example-org/settings/access-tokens" {
		t.Errorf("access tokens URL = %q", got)
	}
}
