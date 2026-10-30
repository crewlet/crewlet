package configapi_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// byEngine is the author an in-process write records.
var byEngine = iam.Actor{Name: "seat access migration", Kind: iam.ActorSystem}

// goalEdit sets a seat's goal, the smallest change a seat write can make.
func goalEdit(handle, goal string) configapi.SeatEdit {
	return configapi.SeatEdit{Handle: handle, Edit: func(r *config.Role) error {
		r.Goal = goal
		return nil
	}}
}

// SEVERAL SEATS ARE ONE REVISION.
//
// A save of an integration's Agents tab changes several seats at once, and it
// is one decision: one revision, one activation, one history line. The seats
// sit at the root and two units deep, and every one of them is changed.
func TestSeveralSeatsAreWrittenAsOneRevision(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	base := s.seed(t, orgDoc)

	applied, err := s.service().ApplySeats(t.Context(), configapi.ApplySeatsRequest{
		Seats:   []configapi.SeatEdit{goalEdit("ceo", "set direction"), goalEdit("staff-eng", "own the build")},
		Summary: "two goals", By: byEngine, Expect: base,
	})
	if err != nil {
		t.Fatalf("ApplySeats: %v", err)
	}
	if applied.Parent != base {
		t.Errorf("the revision was built on %q, want the one the edit named (%q)",
			applied.Parent, base)
	}
	for handle, want := range map[string]string{"ceo": "set direction", "staff-eng": "own the build"} {
		if got := entityOf(t, s, configapi.EntityRoles, handle)["goal"]; got != want {
			t.Errorf("%s's goal = %v, want %q", handle, got, want)
		}
	}
	if got := entityOf(t, s, configapi.EntityRoles, "cto")["goal"]; got != nil {
		t.Errorf("a seat the write did not name changed: goal = %v", got)
	}
	revisions, err := s.service().Revisions(t.Context(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 {
		t.Errorf("the write stored %d revisions after the seed, want one", len(revisions)-1)
	}
}

// A SEAT WRITE IS REFUSED WHOLE, AND STALE IS REFUSED BEFORE ANYTHING.
//
// A seat the document does not carry, a seat named twice, an edit that fails
// and a revision the caller did not read each refuse the write, and the fleet
// stays on the seed.
func TestASeatWriteIsRefusedWhole(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	base := s.seed(t, orgDoc)
	refused := errors.New("the edit refuses")
	for name, tc := range map[string]struct {
		seats  []configapi.SeatEdit
		expect string
		want   func(error) bool
	}{
		"no such seat": {
			seats: []configapi.SeatEdit{goalEdit("ceo", "x"), goalEdit("nobody", "y")},
			want:  func(err error) bool { return errors.Is(err, configapi.ErrNoSuchEntity) },
		},
		"named twice": {
			seats: []configapi.SeatEdit{goalEdit("ceo", "x"), goalEdit("ceo", "y")},
			want:  func(err error) bool { return errors.Is(err, configapi.ErrSeatNamedTwice) },
		},
		"an edit refuses": {
			seats: []configapi.SeatEdit{goalEdit("ceo", "x"), {Handle: "cto",
				Edit: func(*config.Role) error { return refused }}},
			want: func(err error) bool { return errors.Is(err, refused) },
		},
		"stale": {
			seats: []configapi.SeatEdit{goalEdit("ceo", "x")}, expect: "not-the-base",
			want: func(err error) bool {
				var raced *configapi.RacedError
				return errors.As(err, &raced)
			},
		},
	} {
		_, err := s.service().ApplySeats(t.Context(), configapi.ApplySeatsRequest{
			Seats: tc.seats, Summary: name, By: byEngine, Expect: tc.expect,
		})
		if err == nil || !tc.want(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if now, _ := s.service().ActiveRevision(t.Context()); now != base {
		t.Errorf("a refused write moved the fleet to %s", now)
	}
}

// A CHECK OF SEVERAL SEATS STORES NOTHING and answers the company it would.
func TestCheckSeatsStoresNothing(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	base := s.seed(t, orgDoc)
	checked, err := s.service().CheckSeats(t.Context(), configapi.ApplySeatsRequest{
		Seats: []configapi.SeatEdit{goalEdit("cto", "ship")}, By: byEngine, Expect: base,
	})
	if err != nil {
		t.Fatalf("CheckSeats: %v", err)
	}
	if checked.Base != base {
		t.Errorf("the check was built on %q, want %q", checked.Base, base)
	}
	found := false
	for r := range checked.Company.EachRole() {
		if r.Seat().Handle() == "cto" {
			found = r.Goal == "ship"
		}
	}
	if !found {
		t.Error("the checked company does not carry the edit")
	}
	if now, _ := s.service().ActiveRevision(t.Context()); now != base {
		t.Errorf("a check moved the fleet to %s", now)
	}
}

// A MIGRATION REWRITES THE STORED DOCUMENT ONCE.
//
// It reads a key this build no longer has from the stored tree, removes it,
// and writes one revision; run again it finds nothing to do and writes none.
// The retired key is gone for good: the next write does not carry it back.
func TestAMigrationRewritesTheStoredDocumentOnce(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	var tree map[string]any
	if err := json.Unmarshal(companyJSON(t, orgDoc), &tree); err != nil {
		t.Fatal(err)
	}
	tree["retired_setting"] = "old"
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	base := s.seedJSON(t, raw)

	root, revision, err := s.service().StoredDocument(t.Context())
	if err != nil || revision != base || root["retired_setting"] != "old" {
		t.Fatalf("StoredDocument = %v, %q, %v; want the seed with its retired key",
			root["retired_setting"], revision, err)
	}
	read := ""
	migrate := configapi.MigrateRequest{Summary: "retire a setting", By: byEngine,
		Expect: base, Transform: func(root map[string]any) (bool, error) {
			read, _ = root["retired_setting"].(string)
			if _, ok := root["retired_setting"]; !ok {
				return false, nil
			}
			delete(root, "retired_setting")
			return true, nil
		}}
	applied, err := s.service().Migrate(t.Context(), migrate)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if read != "old" {
		t.Errorf("the transform read %q, want the stored value", read)
	}
	if strings.Contains(s.activeDocument(t), "retired_setting") {
		t.Error("the migrated revision still carries the retired key")
	}
	migrate.Expect = applied.RevisionID
	if _, err := s.service().Migrate(t.Context(), migrate); !errors.Is(err,
		configapi.ErrNothingToMigrate) {
		t.Errorf("a second run = %v, want ErrNothingToMigrate", err)
	}
	if _, err := s.service().ApplySeats(t.Context(), configapi.ApplySeatsRequest{
		Seats: []configapi.SeatEdit{goalEdit("ceo", "x")}, Summary: "later", By: byEngine,
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.activeDocument(t), "retired_setting") {
		t.Error("a later write carried the retired key back")
	}
}

// A MIGRATION PLANNED ON ONE REVISION IS NOT WRITTEN OVER ANOTHER.
func TestAMigrationIsHeldToTheRevisionItPlannedOn(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	base := s.seed(t, orgDoc)
	if _, err := s.service().ApplySeats(t.Context(), configapi.ApplySeatsRequest{
		Seats: []configapi.SeatEdit{goalEdit("ceo", "moved")}, Summary: "meanwhile",
		By: byEngine,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.service().Migrate(t.Context(), configapi.MigrateRequest{
		Summary: "late", By: byEngine, Expect: base,
		Transform: func(map[string]any) (bool, error) { return true, nil },
	})
	var raced *configapi.RacedError
	if !errors.As(err, &raced) {
		t.Errorf("a migration planned on a revision the fleet left = %v, want RacedError", err)
	}
}

// seedJSON stores a document as the stored JSON it is, keys this build does not
// represent included, sealed under the surface's keyring.
func (s *surface) seedJSON(t *testing.T, raw []byte) string {
	t.Helper()
	payload, err := secrets.Seal(s.cipher, raw)
	if err != nil {
		t.Fatal(err)
	}
	return s.seedPayload(t, payload)
}

// githubSeatDoc is a company whose CTO holds a GitHub App the engine recorded.
const githubSeatDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  github:
    enabled: true
    webhook_secret: ${GITHUB_WEBHOOK_SECRET}
    provisioning: {org: acme}
roles:
  - name: CTO
    handle: cto
    llm: zulu
    integrations:
      github:
        enabled: true
        permissions: {contents: read}
        app_id: 101
        app_slug: acme-cto
        installation_id: 202
        private_key: ${GITHUB_APP_KEY_CTO}
`

// A FIELD ONLY THE ENGINE WRITES IS REFUSED FROM ANY OTHER WRITER.
//
// A GitHub App's id names which app an agent acts as, and only the engine
// writes it, once GitHub has answered: a person or a model changing it would
// point the agent at an app that is not its own. The engine's own write of it
// lands, and an edit that leaves it as it was is not refused. Mutation: drop
// the check, or refuse the engine too, and a case here fails.
func TestAFieldOnlyTheEngineWritesIsRefusedFromAnyoneElse(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, githubSeatDoc)
	repoint := func(r *config.Role) error {
		r.Integrations.GitHub.AppID = 999
		return nil
	}
	person := iam.Actor{Name: "jane", Kind: iam.ActorHuman}
	_, err := s.service().ApplySeats(t.Context(), configapi.ApplySeatsRequest{
		Seats: []configapi.SeatEdit{{Handle: "cto", Edit: repoint}}, Summary: "repoint", By: person,
	})
	var invalid *configapi.ValidationError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "app_id") {
		t.Errorf("a person changing the CTO's app id = %v, want refused naming app_id", err)
	}
	if _, err := s.service().ApplySeats(t.Context(), configapi.ApplySeatsRequest{
		Seats: []configapi.SeatEdit{goalEdit("cto", "ship")}, Summary: "a goal", By: person,
	}); err != nil {
		t.Errorf("an edit that leaves the app id alone = %v, want it admitted", err)
	}
	body, err := json.Marshal(entityOf(t, s, configapi.EntityRoles, "cto"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := strings.Replace(string(body), `"app_id":101`, `"app_id":303`, 1)
	if _, err := s.service().ApplyEntity(t.Context(), configapi.ApplyEntityRequest{
		Kind: configapi.EntityRoles, ID: "cto", Body: []byte(recorded), Summary: "record the app",
		By: byEngine, Engine: true,
	}); err != nil {
		t.Errorf("the engine recording an app id = %v, want it admitted", err)
	}
}
