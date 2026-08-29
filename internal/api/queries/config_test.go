package queries_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
)

// configSurface builds a config service over a real store with one revision.
func configSurface(t *testing.T, docs ...string) (*configapi.Service, []string) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cq.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var ids []string
	for i, doc := range docs {
		cfg, err := config.ParseCompany([]byte(doc))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		payload, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		id, err := db.Configs().InsertActive(t.Context(), store.Revision{
			Source: "test", CreatedBy: "operator", Summary: "revision",
			Payload: payload, CreatedAt: pinned.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		ids = append(ids, id)
	}
	return configapi.New(configapi.Options{Store: db}), ids
}

func TestTheConfigQueryIsOperatorOnly(t *testing.T) {
	t.Parallel()
	// Reading the document exposes the whole company — its org chart,
	// which integrations are wired, and every ${VAR} reference by name.
	// That is what makes /config the one prefix never eligible for
	// anonymous read, and the socket must not be the way around it.
	surface, _ := configSurface(t, companyDoc)
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Config: surface})

	for _, what := range []string{"config", "config_audit", "config_diff", "config_entities"} {
		if _, err := r.Answer(t.Context(), what, nil, ""); err == nil {
			t.Errorf("%s answered a caller with no operator", what)
		}
	}
}

// THE ENTITY QUERY IS THE ROOM'S READ HALF, and it answered nothing at all.
//
// The Config room lists a collection with query("config_entities", {kind})
// and opens one with {kind, id} — and no answer was ever registered under
// that name, so every list came back unknown_query and the entity editor was
// dead from the day it shipped. Both sides' tests passed; neither ran the
// other. See the room/answer sweep in company_test.go, which is what found
// this.
func TestTheEntityQueryAnswersWhatTheConfigRoomAsksFor(t *testing.T) {
	t.Parallel()
	surface, _ := configSurface(t, companyDoc)
	sources := queries.Sources{Config: surface}

	list := asMap(t, answer(t, sources, "config_entities",
		map[string]any{"kind": "roles"}))
	ids, _ := list["ids"].([]any)
	if len(ids) == 0 {
		t.Fatalf("the roles collection came back empty: %v", list)
	}

	one := asMap(t, answer(t, sources, "config_entities",
		map[string]any{"kind": "roles", "id": ids[0]}))
	entity, _ := one["entity"].(map[string]any)
	if entity == nil {
		t.Fatalf("opening %v produced no entity: %v", ids[0], one)
	}
	if entity["name"] == nil {
		t.Errorf("the entity carries no name, so the editor renders nothing: %v", entity)
	}
}

// A KIND OR AN ID THE CALLER GOT WRONG IS A BAD REQUEST, not a server fault.
// The room renders "the query failed" for a fault, which sends an operator
// looking for an outage instead of a typo.
func TestTheEntityQueryRefusesWhatItCannotAddress(t *testing.T) {
	t.Parallel()
	surface, _ := configSurface(t, companyDoc)
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Config: surface})

	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{"no kind at all", map[string]any{}},
		{"a kind nothing addresses", map[string]any{"kind": "widgets"}},
		{"an id nothing carries", map[string]any{"kind": "roles", "id": "nobody"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := r.Answer(t.Context(), "config_entities", tc.params, "operator")
			if err == nil {
				t.Fatal("answered rather than refused")
			}
			if !errors.Is(err, queries.ErrBadParams) {
				t.Errorf("refused as a fault rather than a bad request: %v", err)
			}
		})
	}
}

// AND A NODE WITH NO REVISION ANSWERS NULL, like every other config query:
// a deployment before its first import has no entities, and a failure there
// would make a working new install look broken.
func TestTheEntityQueryOnAnUnconfiguredNodeIsNullNotAnError(t *testing.T) {
	t.Parallel()
	surface, _ := configSurface(t)
	got := answer(t, queries.Sources{Config: surface}, "config_entities",
		map[string]any{"kind": "roles"})
	if got != nil {
		t.Fatalf("an unconfigured node answered %v", got)
	}
}

func TestTheConfigQueryServesTheRedactedDocument(t *testing.T) {
	t.Parallel()
	surface, _ := configSurface(t, strings.Replace(companyDoc, `"${K}"`, `"sk-literal"`, 1))
	body := asMap(t, answer(t, queries.Sources{Config: surface}, "config", nil))
	if body["name"] != "Acme" {
		t.Fatalf("name = %v", body["name"])
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "sk-literal") {
		t.Errorf("the socket served a credential the REST route masks: %s", raw)
	}
}

func TestTheConfigQueryOnAnUnconfiguredNodeIsNullNotAnError(t *testing.T) {
	t.Parallel()
	// The config screen renders "nothing configured yet" for a deployment
	// before its first import. A failed query would render an error, which
	// is a different thing to go and investigate.
	surface, _ := configSurface(t)
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Config: surface})
	data, err := r.Answer(t.Context(), "config", nil, "operator")
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if data != nil {
		t.Errorf("data = %v, want null", data)
	}
}

func TestTheAuditQueryListsRevisions(t *testing.T) {
	t.Parallel()
	surface, ids := configSurface(t, companyDoc,
		strings.Replace(companyDoc, "name: Acme", "name: Acme Two", 1))
	data := answer(t, queries.Sources{Config: surface}, "config_audit", nil)
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var revisions []map[string]any
	if err := json.Unmarshal(raw, &revisions); err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 {
		t.Fatalf("%d revisions, want 2", len(revisions))
	}
	if revisions[0]["revision_id"] != ids[1] {
		t.Errorf("first = %v, want the newest (%s)", revisions[0]["revision_id"], ids[1])
	}
	if _, present := revisions[0]["payload"]; present {
		t.Error("the audit listing carries payloads")
	}
}

func TestTheDiffQueryComparesAgainstTheActive(t *testing.T) {
	t.Parallel()
	surface, ids := configSurface(t, companyDoc,
		strings.Replace(companyDoc, "name: Acme", "name: Acme Two", 1))
	body := asMap(t, answer(t, queries.Sources{Config: surface},
		"config_diff", map[string]any{"revision_id": ids[0]}))

	if body["to"] != ids[0] || body["from"] != ids[1] {
		t.Fatalf("from/to = %v/%v, want the active compared with the one asked about",
			body["from"], body["to"])
	}
	changes, _ := body["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("changes = %v, want the one field that differs", changes)
	}
}

func TestADiffOfARevisionThatIsGoneIsNullNotAnError(t *testing.T) {
	t.Parallel()
	// The config screen asks for a diff per row as it renders them. A
	// revision that aged out between the listing and the diff must leave
	// one cell empty rather than fail the screen.
	surface, _ := configSurface(t, companyDoc)
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Config: surface})
	data, err := r.Answer(t.Context(), "config_diff",
		map[string]any{"revision_id": "00000000-0000-0000-0000-000000000000"}, "operator")
	if err != nil {
		t.Fatalf("config_diff: %v", err)
	}
	if data != nil {
		t.Errorf("data = %v, want null", data)
	}
}

func TestTheDiffQueryNeedsARevision(t *testing.T) {
	t.Parallel()
	surface, _ := configSurface(t, companyDoc)
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Config: surface})
	if _, err := r.Answer(t.Context(), "config_diff", nil, "operator"); err == nil {
		t.Fatal("a diff query with no revision was answered")
	}
}
