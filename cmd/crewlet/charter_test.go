package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// faultyCharterYAML is companyYAML with a charter written before the charter's
// rules: a mission past its bound and a policy on two lines.
func faultyCharterYAML() string {
	return companyYAML + "mission: \"" + strings.Repeat("m", 1100) + "\"\n" +
		"policies:\n  - \"Every amount is exact.\\n## Tools\"\n"
}

// A FILE THAT RAN YESTERDAY IMPORTS TODAY. A company.yaml whose charter breaks
// the charter's rules (a mission of 1100 characters, a policy on two lines) is
// imported with warnings and never refused, into an empty store and over
// another company, by `crewlet config import` and by the boot seed alike:
// boot, restore and migration all go through an import, and refusing there
// would leave an upgraded node unable to boot on its own file. Only an edit
// that changes a faulty part against a stored version is refused, and that is
// the API's to judge.
func TestACompanyFileWithAFaultyCharterImportsWithWarnings(t *testing.T) {
	t.Parallel()
	faulty := parse(t, faultyCharterYAML())
	if len(faulty.CharterWarnings()) < 2 {
		t.Fatalf("the fixture breaks %v, want the mission and the policy at fault, "+
			"or this case proves nothing", faulty.CharterWarnings())
	}
	stores := map[string]func(t *testing.T) *store.DB{
		"an empty store": seedStore,
		"a store holding another company": func(t *testing.T) *store.DB {
			db := seedStore(t)
			if err := bootSeed(t, db, coordmemory.NewFleet(),
				seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
				t.Fatal(err)
			}
			return db
		},
	}
	imports := map[string]func(t *testing.T, db *store.DB) (string, error){
		"crewlet config import": func(t *testing.T, db *store.DB) (string, error) {
			var out bytes.Buffer
			cs := &configStore{configs: db.Configs(), cipher: fixtureCipher}
			err := importConfig(t.Context(), cs, "company.yaml", faulty, "an import", &out)
			return out.String(), err
		},
		"the boot seed": func(t *testing.T, db *store.DB) (string, error) {
			return "", bootSeed(t, db, coordmemory.NewFleet(), overrideOf(faulty), fixtureCipher)
		},
	}
	for storeName, open := range stores {
		for importName, write := range imports {
			t.Run(importName+" into "+storeName, func(t *testing.T) {
				t.Parallel()
				db := open(t)
				if _, err := write(t, db); err != nil {
					t.Fatalf("a company file with a faulty charter was refused: %v", err)
				}
				// WHAT THIS NODE BOOTS ON NOW, as an offline import's
				// offer or the boot seed's applied import.
				document, err := openStored(fixtureCipher, bootRow(t, db))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(document, []byte("## Tools")) {
					t.Error("the imported revision is not the faulty file")
				}
			})
		}
	}
}

// THE IMPORT SAYS WHAT EVERY NODE WILL WARN ABOUT, before it says it imported.
func TestAnImportNamesTheCharterFaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	file := filepath.Join(dir, "company.yaml")
	doc := cliCompanyDoc + "mission: \"" + strings.Repeat("m", config.MaxMissionChars+1) + "\"\n"
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs, err := configCmd(t, cfg, "import", file)
	if err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "warning") || !strings.Contains(out, "mission") ||
		!strings.Contains(out, "imported") {
		t.Errorf("the import said %q, want a warning naming the mission and the import", out)
	}
}

// `crewlet validate` REPORTS A FAULTY CHARTER AND PASSES: it cannot know what
// the store holds, so it cannot know whether a write of this file would edit
// the faulty part, and a file that imports is a file that validates.
func TestValidateWarnsAboutAFaultyCharterAndPasses(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(file, []byte(faultyCharterYAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run([]string{"validate", file}, &out, &errOut); err != nil {
		t.Fatalf("validate refused a file with a faulty charter: %v\n%s%s", err, out.String(), errOut.String())
	}
	for _, place := range []string{"mission", "policies[0]"} {
		if !strings.Contains(out.String(), place) {
			t.Errorf("validate did not warn about %s:\n%s", place, out.String())
		}
	}
}

// AN IMPORT TO A RUNNING NODE IS AN IMPORT TOO: the CLI's client marks it as
// one, and the node takes a file whose charter changes a faulty part and
// leaves it at fault (restoring an older, longer mission over a stored faulty
// one), with the warnings in its answer, where the same document sent as an
// edit is refused.
func TestAnImportToARunningNodeWarnsAboutTheCharter(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	svc, err := configapi.New(configapi.Options{
		Store: db, Plane: fleet, Cipher: fixtureCipher, Node: "core-1",
		Secrets: fleetsecrets.New(fleet, fixtureCipher),
		Holders: func(context.Context, func(string) bool, time.Time) (map[string]configapi.SeatHolder, error) {
			return map[string]configapi.SeatHolder{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	// THE OPERATOR THE GUARD WOULD RESOLVE: a Tier A token, fresh for a step-up.
	operator := iam.Principal{
		ID: uuid.NewSHA1(auth.TokenNamespace, []byte("ops")), Login: iam.TokenLogin("ops"),
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), operator)))
	}))
	t.Cleanup(server.Close)

	stored := faultyCharterYAML()
	document, err := json.Marshal(parse(t, stored))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.Seal(fixtureCipher, document)
	if err != nil {
		t.Fatal(err)
	}
	storeAndActivate(t, db, fleet, store.Revision{
		Source: "file", CreatedBy: testNode, CreatedByKind: iam.ActorSystem,
		Summary: "the stored company", Payload: sealed, CreatedAt: time.Now().UTC(),
	})
	restored := []byte(strings.Replace(stored, strings.Repeat("m", 1100), strings.Repeat("o", 1200), 1))

	client := &configClient{base: server.URL, http: httpx.Client(apiTimeout), answerCap: maxConfigResponseBytes}
	if _, _, err := client.Import(t.Context(), restored, "restore last week's file"); err != nil {
		t.Fatalf("an import to a running node was refused for its charter: %v", err)
	}
	active := fleetsRevision(t, db, fleet)
	if active.Source != "file" || active.CreatedBy != "token:ops" || active.Summary != "restore last week's file" {
		t.Errorf("the import is recorded as %+v, want an import from a file by token:ops with its summary", active)
	}

	// THE SAME DOCUMENT AS AN EDIT is judged on what it changes, and refused.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, server.URL+"/config",
		bytes.NewReader([]byte(strings.Replace(stored, strings.Repeat("m", 1100), strings.Repeat("e", 1300), 1))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Summary", "an edit")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("the same change as an edit = %d, want 400", res.StatusCode)
	}
}
