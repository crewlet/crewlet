package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// configSurface builds a config service over a real store with one revision.
func configSurface(t *testing.T, docs ...string) (*configapi.Service, []string) {
	t.Helper()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "cq.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })

	// A KEYRING, because the surface refuses to be built without one and
	// reads only what was sealed under it.
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": key}})
	if err != nil {
		t.Fatal(err)
	}
	// EACH ONE ACTIVATED IN TURN, as the writes that made them were: the
	// company the surface answers is the fleet's pointer.
	plane := coordmemory.NewFleet()
	var ids []string
	for i, doc := range docs {
		cfg, err := config.ParseCompany([]byte(doc))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		document, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := secrets.Seal(cipher, document)
		if err != nil {
			t.Fatal(err)
		}
		revision := store.Revision{
			CreatedByKind: iam.ActorOperator,
			Source:        "test", CreatedBy: "operator", Summary: "revision",
			Payload: payload, CreatedAt: pinned.Add(time.Duration(i) * time.Minute),
		}
		id, err := db.Configs().Insert(t.Context(), revision)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		request := coord.ActivationRequest{
			RevisionID: id, Summary: revision.Summary, Payload: payload, At: revision.CreatedAt,
			Origin: coord.RevisionOrigin{
				Author: revision.CreatedBy, AuthorKind: revision.CreatedByKind,
				Source: revision.Source, CreatedAt: revision.CreatedAt,
			},
			ExpectAbsent: len(ids) == 0,
		}
		if len(ids) > 0 {
			request.Expect = ids[len(ids)-1]
		}
		if _, err := plane.Activate(t.Context(), request); err != nil {
			t.Fatalf("activate: %v", err)
		}
		ids = append(ids, id)
	}
	svc, err := configapi.New(configapi.Options{
		Store: db, Plane: plane, Cipher: cipher, Node: "core-1",
		Holders: func(context.Context, []string, time.Time) (
			map[string]configapi.SeatHolder, error) {

			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("configapi.New: %v", err)
	}
	return svc, ids
}

func TestTheConfigQueryIsAdmins(t *testing.T) {
	t.Parallel()
	// Reading the document exposes the whole company — its org chart,
	// which integrations are wired, and every ${VAR} reference by name.
	// That is what makes it admin's, and the socket must not be the way
	// around it: somebody who uses every app of the company and is not an
	// admin must reach none of these.
	surface, _ := configSurface(t, companyDoc)
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Config: surface})

	narrow := iam.WithPrincipal(t.Context(), iam.Principal{
		ID: uuid.New(), Login: "token:reader", Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: iam.Apps,
	})
	for _, what := range []string{"config", "config_audit", "config_diff", "config_entities"} {
		if _, err := r.Answer(narrow, what, nil); err == nil {
			t.Errorf("%s answered somebody holding every app", what)
		}
		// The control: the permission it DOES need is declared, or the
		// assertion above would pass on a question nothing can reach.
		if got := r.PostureOf(what).Grant(); got != iam.GrantAdmin {
			t.Errorf("%s requires %q, want %q", what, got, iam.GrantAdmin)
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
		map[string]any{"kind": configapi.EntityLLMProviders}))
	ids, _ := list["ids"].([]any)
	if len(ids) == 0 {
		t.Fatalf("the providers collection came back empty: %v", list)
	}

	one := asMap(t, answer(t, sources, "config_entities",
		map[string]any{"kind": configapi.EntityLLMProviders, "id": ids[0]}))
	entity, _ := one["entity"].(map[string]any)
	if entity == nil {
		t.Fatalf("opening %v produced no entity: %v", ids[0], one)
	}
	if entity["model"] == nil {
		t.Errorf("the entity carries no model, so the editor renders nothing: %v", entity)
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
		{"an id nothing carries", map[string]any{"kind": configapi.EntityLLMProviders, "id": "nobody"}},
		{"a seat nothing carries", map[string]any{"kind": configapi.EntityRoles, "id": "nobody"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := r.Answer(everyGrant(t), "config_entities", tc.params)
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
		map[string]any{"kind": configapi.EntityLLMProviders})
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
	data, err := r.Answer(everyGrant(t), "config", nil)
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
	data, err := r.Answer(everyGrant(t), "config_diff",
		map[string]any{"revision_id": "00000000-0000-0000-0000-000000000000"})
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
	if _, err := r.Answer(everyGrant(t), "config_diff", nil); err == nil {
		t.Fatal("a diff query with no revision was answered")
	}
}

// A FLEET REVISION THIS NODE CANNOT OPEN IS ANSWERED IN THE /config ROUTE'S
// CODE AND WORDS on the socket's config questions: it was `query_failed`, a
// failure no screen could say anything about, while REST named the key to add.
func TestAConfigQuestionOnARevisionThisNodeCannotOpenSaysWhatFixesIt(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "cq.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	keyring := func() secrets.Cipher {
		key, err := secrets.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
			Keys: map[string][]byte{"k1": key}})
		if err != nil {
			t.Fatal(err)
		}
		return cipher
	}
	// THE FLEET'S REVISION, sealed under a key this node was never given.
	plane := coordmemory.NewFleet()
	cfg, err := config.ParseCompany([]byte(companyDoc))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Seal(keyring(), stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "the-fleets", Summary: "peer", Payload: payload, At: pinned,
		Origin: coord.RevisionOrigin{Author: "operator", AuthorKind: iam.ActorOperator,
			Source: "peer", CreatedAt: pinned},
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	svc, err := configapi.New(configapi.Options{
		Store: db, Plane: plane, Cipher: keyring(), Node: "core-1",
		Holders: func(context.Context, []string, time.Time) (
			map[string]configapi.SeatHolder, error) {

			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("configapi.New: %v", err)
	}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Config: svc, Now: func() time.Time { return pinned }})
	for what, params := range map[string]map[string]any{
		"config":          nil,
		"config_entities": {"kind": configapi.EntityLLMProviders},
	} {
		_, err := r.Answer(everyGrant(t), what, params)
		var revision *stream.RevisionError
		if !errors.As(err, &revision) || revision.Code != stream.CodeUnreadableFleetRevision {
			t.Errorf("%s = %v, want the /config route's unreadable_fleet_revision", what, err)
			continue
		}
		if !strings.Contains(revision.Detail, "secrets.keys") ||
			strings.Contains(revision.Detail, "secrets:") {
			t.Errorf("%s: the detail does not say which setting holds the key, in "+
				"words: %q", what, revision.Detail)
		}
	}
}
