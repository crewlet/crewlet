package configapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

var pinned = time.Date(2026, 8, 23, 15, 0, 0, 0, time.UTC)

// The webhook signing tokens the fixture company carries. Both are REAL
// values in GitLab's own shape — whsec_ over standard base64 of a 32-byte
// key — because config validation refuses anything else, so a fixture that
// merely looked the part would never parse.
const (
	signingSecret        = "whsec_YS1maXh0dXJlLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM="
	rotatedSigningSecret = "whsec_YS1yb3RhdGVkLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM="
)

// companyDoc is a company carrying one credential of each kind this surface
// has to keep out of a response: a literal provider key, a ${VAR} reference
// that must survive an edit, and an integration secret. The integration is
// GitLab because that is the code host this build serves — the unserved
// ones are refused by validation and would never reach these routes.
const companyDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-literal", "${ROTATED}"]
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: ` + signingSecret + `
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: CTO
    handle: cto
    llm: zulu
`

// surface is the service plus everything a test needs to look at.
type surface struct {
	mux     *http.ServeMux
	configs *store.Configs
	db      *store.DB
	plane   coord.Plane
	svc     *configapi.Service
	cipher  secrets.Cipher
}

func newSurface(t *testing.T, cipher secrets.Cipher) *surface {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "c.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := &surface{
		mux: http.NewServeMux(), configs: db.Configs(), db: db,
		plane: coordmemory.NewFleet(),
	}
	s.svc = configapi.New(configapi.Options{
		Store: db, Plane: s.plane, Cipher: cipher,
		Now: func() time.Time { return pinned },
	})
	s.svc.Routes(s.mux)
	s.cipher = cipher
	return s
}

// service is the same object the routes are mounted on, for the reads the
// query layer makes directly rather than over HTTP.
func (s *surface) service() *configapi.Service { return s.svc }

// activeDocument is the stored revision as a node applying it would see it:
// unsealed and UNREDACTED, which is the only view that can prove a mask was
// resolved rather than stored.
func (s *surface) activeDocument(t *testing.T) string {
	t.Helper()
	revision, found, err := s.configs.Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active revision: %v (found=%v)", err, found)
	}
	document, err := secrets.Open(s.cipher, revision.Payload)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	return string(document)
}

func (s *surface) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res := httptest.NewRecorder()
	s.mux.ServeHTTP(res, req)
	return res
}

// seed stores a revision the way the node's own seeding does.
func (s *surface) seed(t *testing.T, doc string, cipher secrets.Cipher) string {
	t.Helper()
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
	id, err := s.configs.InsertActive(t.Context(), store.Revision{
		Source: "test", CreatedBy: "operator", Summary: "seed",
		Payload: payload, CreatedAt: pinned,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}

func decode(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", res.Body, err)
	}
	return body
}

// --- reads -----------------------------------------------------------------

func TestAnUnconfiguredNodeSaysSoRatherThanFailing(t *testing.T) {
	t.Parallel()
	// A deployment before its first import has no configuration. Reporting
	// that as an error would make a working new install look broken on the
	// operator's first look at it.
	s := newSurface(t, nil)
	res := s.do(t, http.MethodGet, "/config", "", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", res.Code)
	}
	if decode(t, res)["error"] != "no_active_revision" {
		t.Errorf("body = %s", res.Body)
	}
	// And the history is an empty list, not a 404: "nothing has happened"
	// is an answer, where "this endpoint does not exist" is not.
	list := s.do(t, http.MethodGet, "/config/revisions", "", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("revisions got %d, want 200", list.Code)
	}
	if strings.TrimSpace(list.Body.String()) != "[]" {
		t.Errorf("revisions = %s, want an empty list", list.Body)
	}
}

func TestTheActiveConfigComesBackRedacted(t *testing.T) {
	t.Parallel()
	// This surface is guarded, and it still does not serve credentials:
	// a bearer token authorises reading the COMPANY, not extracting every
	// secret it holds into a browser's network log.
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	res := s.do(t, http.MethodGet, "/config", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	body := res.Body.String()
	if strings.Contains(body, "sk-literal") || strings.Contains(body, signingSecret) {
		t.Errorf("the config surface served a credential: %s", body)
	}
	if !strings.Contains(body, "${ROTATED}") {
		t.Error("the reference was masked, so the document cannot be edited")
	}
	if !strings.Contains(body, "Acme") {
		t.Error("the company itself did not come back")
	}
}

func TestTheConfigCanBeReadAsYAML(t *testing.T) {
	t.Parallel()
	// YAML is the form an operator edits and the form every example is
	// written in. A surface that only spoke JSON would make "read it,
	// change a line, send it back" a format conversion.
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	res := s.do(t, http.MethodGet, "/config?format=yaml", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != "application/yaml" {
		t.Errorf("content type = %q", got)
	}
	var document map[string]any
	if err := yaml.Unmarshal(res.Body.Bytes(), &document); err != nil {
		t.Fatalf("the body is not yaml: %v", err)
	}
	if document["name"] != "Acme" {
		t.Errorf("name = %v", document["name"])
	}
}

func TestTheHistoryIsMetadataOnly(t *testing.T) {
	t.Parallel()
	// A listing carrying every payload would move the whole history
	// through the process to render a table of summaries, and the
	// documents are the largest rows in the database.
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)
	s.seed(t, strings.Replace(companyDoc, "name: Acme", "name: Acme Two", 1), nil)

	res := s.do(t, http.MethodGet, "/config/revisions", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d", res.Code)
	}
	var revisions []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &revisions); err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 {
		t.Fatalf("%d revisions, want 2", len(revisions))
	}
	if _, present := revisions[0]["payload"]; present {
		t.Error("the listing carries payloads")
	}
	// Newest first, and only one is active.
	if revisions[0]["is_active"] != true || revisions[1]["is_active"] != false {
		t.Errorf("active flags = %v, %v", revisions[0]["is_active"], revisions[1]["is_active"])
	}
	for _, field := range []string{"revision_id", "created_at", "created_by", "summary", "source"} {
		if _, present := revisions[0][field]; !present {
			t.Errorf("the listing omits %q, which is what an operator reads at 3am", field)
		}
	}
}

func TestPaginationIsClampedNotRefused(t *testing.T) {
	t.Parallel()
	// A caller asking for more than the ceiling has made no mistake worth a
	// 400 — they want everything. A page size nobody bounds is one tab
	// pulling the whole history through a process every other tab shares.
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)
	if got := s.do(t, http.MethodGet, "/config/revisions?limit=100000", "", nil).Code; got != http.StatusOK {
		t.Errorf("an oversized limit got %d, want 200", got)
	}
	// A limit that is not a number IS a mistake: silently substituting a
	// default would hide a broken client.
	res := s.do(t, http.MethodGet, "/config/revisions?limit=lots", "", nil)
	if res.Code != http.StatusBadRequest {
		t.Errorf("a non-numeric limit got %d, want 400", res.Code)
	}
}

func TestOneRevisionComesBackWithItsPayload(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	id := s.seed(t, companyDoc, nil)

	res := s.do(t, http.MethodGet, "/config/revisions/"+id, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["revision_id"] != id {
		t.Errorf("revision_id = %v", body["revision_id"])
	}
	payload, ok := body["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %v", body["payload"])
	}
	if payload["name"] != "Acme" {
		t.Errorf("payload name = %v", payload["name"])
	}
	// Redacted here too: a historical revision's credentials are as much a
	// credential as the active one's.
	if strings.Contains(res.Body.String(), "sk-literal") {
		t.Error("a historical revision served a credential")
	}
}

func TestAnUnknownRevisionIsNotFound(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	for _, path := range []string{
		"/config/revisions/00000000-0000-0000-0000-000000000000",
		"/config/revisions/00000000-0000-0000-0000-000000000000/diff",
	} {
		if got := s.do(t, http.MethodGet, path, "", nil).Code; got != http.StatusNotFound {
			t.Errorf("%s got %d, want 404", path, got)
		}
	}
}

// --- diff ------------------------------------------------------------------

func TestADiffNamesWhatChanged(t *testing.T) {
	t.Parallel()
	// A LINE diff was the alternative and it is the wrong tool: the stored
	// form is JSON produced by marshalling a struct, so a re-ordered map
	// rewrites lines that mean nothing. What an operator asks is "what
	// changed about the company".
	s := newSurface(t, nil)
	first := s.seed(t, companyDoc, nil)
	grown := strings.Replace(companyDoc,
		"  - name: CTO\n    handle: cto\n    llm: zulu\n",
		"  - name: CTO\n    handle: cto\n    llm: zulu\n  - name: Designer\n    handle: designer\n    llm: zulu\n", 1)
	s.seed(t, grown, nil)

	res := s.do(t, http.MethodGet, "/config/revisions/"+first+"/diff?against=active", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	var body struct {
		From    string             `json:"from"`
		To      string             `json:"to"`
		Changes []configapi.Change `json:"changes"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.To != first {
		t.Errorf("to = %q, want the revision that was asked about", body.To)
	}
	var paths []string
	for _, change := range body.Changes {
		paths = append(paths, change.Path+":"+change.Kind)
	}
	// The active side has the extra seat, and the target does not — so
	// from the target's point of view it is removed.
	if !strings.Contains(strings.Join(paths, " "), "roles[2]") {
		t.Errorf("the diff does not mention the seat that differs: %v", paths)
	}
}

func TestADiffNeverCarriesEitherSecret(t *testing.T) {
	t.Parallel()
	// Comparing raw documents would put the OLD and the NEW value of a
	// rotated credential in one response — strictly worse than the read
	// this surface already refuses to serve.
	s := newSurface(t, nil)
	first := s.seed(t, companyDoc, nil)
	s.seed(t, strings.Replace(companyDoc, signingSecret, rotatedSigningSecret, 1), nil)

	res := s.do(t, http.MethodGet, "/config/revisions/"+first+"/diff", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d", res.Code)
	}
	body := res.Body.String()
	for _, secret := range []string{signingSecret, rotatedSigningSecret} {
		if strings.Contains(body, secret) {
			t.Errorf("the diff carries %q: %s", secret, body)
		}
	}
}

// --- writes ----------------------------------------------------------------

func TestAWriteNeedsASummary(t *testing.T) {
	t.Parallel()
	// The history is what an operator reads at 3am to find the change that
	// broke something. A list of revisions with no summaries is a list of
	// uuids.
	s := newSurface(t, nil)
	res := s.do(t, http.MethodPut, "/config", companyDoc, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", res.Code)
	}
	if decode(t, res)["error"] != "summary_required" {
		t.Errorf("body = %s", res.Body)
	}
}

func TestAWriteActivatesANewRevision(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	res := s.do(t, http.MethodPut, "/config", companyDoc,
		map[string]string{"X-Summary": "first import"})
	if res.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["revision_id"] == "" || body["epoch"] == nil {
		t.Fatalf("body = %s", res.Body)
	}
	// THE POINTER MOVED, which is what makes the write reach every node
	// rather than only the process that handled the request.
	target, found, err := s.plane.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("target: found=%v err=%v", found, err)
	}
	if target.RevisionID != body["revision_id"] {
		t.Errorf("the pointer names %q, want the new revision", target.RevisionID)
	}
}

func TestAWriteIsAFullReplacement(t *testing.T) {
	t.Parallel()
	// Not a merge: a merge would make deleting a role impossible through
	// this surface, which is the one operation an operator most needs to
	// be sure of.
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)
	shrunk := strings.Replace(companyDoc, "  - name: CTO\n    handle: cto\n    llm: zulu\n", "", 1)

	if got := s.do(t, http.MethodPut, "/config", shrunk,
		map[string]string{"X-Summary": "remove the CTO seat"}).Code; got != http.StatusCreated {
		t.Fatalf("got %d", got)
	}
	res := s.do(t, http.MethodGet, "/config", "", nil)
	if strings.Contains(res.Body.String(), "cto") {
		t.Errorf("the deleted seat survived a full replacement: %s", res.Body)
	}
}

func TestAMaskedDocumentCanBeSentBack(t *testing.T) {
	t.Parallel()
	// THE round trip, end to end over HTTP. Without it a reader who
	// fetched the config, changed one line and sent it back would replace
	// every credential in the company with the mask — silently, and only
	// discovered when each integration started failing to authenticate.
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	fetched := s.do(t, http.MethodGet, "/config?format=yaml", "", nil)
	if fetched.Code != http.StatusOK {
		t.Fatalf("get: %d", fetched.Code)
	}
	edited := strings.Replace(fetched.Body.String(), "name: Acme", "name: Acme Renamed", 1)
	if !strings.Contains(edited, config.Redacted) {
		t.Fatal("the fetched document carried no mask, so this proves nothing")
	}

	written := s.do(t, http.MethodPut, "/config", edited,
		map[string]string{"X-Summary": "rename"})
	if written.Code != http.StatusCreated {
		t.Fatalf("put: %d — %s", written.Code, written.Body)
	}

	// The credentials came back, and the edit landed.
	active, found, err := s.configs.Active(t.Context())
	if err != nil || !found {
		t.Fatal(err)
	}
	stored, err := config.DecodeCompany(active.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "Acme Renamed" {
		t.Errorf("name = %q", stored.Name)
	}
	if got := stored.Providers.LLM["zulu"].APIKeys[0]; got != "sk-literal" {
		t.Errorf("api key = %q, want the one the previous revision held", got)
	}
	if got := stored.Integrations.GitLab.SigningSecret; got != signingSecret {
		t.Errorf("signing secret = %q", got)
	}
}

func TestAnInvalidDocumentIsRefusedWithItsReason(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	broken := strings.Replace(companyDoc, "llm: zulu", "llm: nonexistent", 1)
	res := s.do(t, http.MethodPut, "/config", broken,
		map[string]string{"X-Summary": "break it"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", res.Code)
	}
	body := decode(t, res)
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v", body["error"])
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "nonexistent") {
		t.Errorf("the reason does not name the fault: %v", body["detail"])
	}
	// And nothing was stored, so a refused write leaves no trace.
	if _, found, _ := s.configs.Active(t.Context()); found {
		t.Error("a refused document was activated")
	}
}

func TestAnUnknownFieldIsRefusedAtTheDoor(t *testing.T) {
	t.Parallel()
	// The AUTHORED reader, which fails closed on a typo. That strictness
	// belongs here and not on the stored form: this is where a person's
	// document arrives, and a typo is a mistake to catch rather than a peer
	// running a newer build.
	s := newSurface(t, nil)
	res := s.do(t, http.MethodPut, "/config", companyDoc+"\nnaem: typo\n",
		map[string]string{"X-Summary": "typo"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", res.Code)
	}
	if decode(t, res)["error"] != "invalid_body" {
		t.Errorf("body = %s", res.Body)
	}
}

func TestConcurrentEditorsAreToldRatherThanOverwritten(t *testing.T) {
	t.Parallel()
	// Two operators editing one company through a full-document PUT is a
	// last-writer-wins race that silently discards the other's change.
	s := newSurface(t, nil)
	base := s.seed(t, companyDoc, nil)

	// Somebody else writes first.
	if got := s.do(t, http.MethodPut, "/config", strings.Replace(companyDoc, "Acme", "Theirs", 1),
		map[string]string{"X-Summary": "theirs"}).Code; got != http.StatusCreated {
		t.Fatalf("the first write got %d", got)
	}
	// Ours was based on the revision they replaced.
	res := s.do(t, http.MethodPut, "/config", strings.Replace(companyDoc, "Acme", "Ours", 1),
		map[string]string{"X-Summary": "ours", "If-Match": base})
	if res.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", res.Code)
	}
	body := decode(t, res)
	if body["error"] != "revision_advanced" {
		t.Errorf("error = %v", body["error"])
	}
	if body["current_revision_id"] == base || body["current_revision_id"] == nil {
		t.Errorf("the answer does not name the revision to re-read: %v", body)
	}
}

func TestAMatchingPreconditionWrites(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	base := s.seed(t, companyDoc, nil)
	res := s.do(t, http.MethodPut, "/config", strings.Replace(companyDoc, "Acme", "Ours", 1),
		map[string]string{"X-Summary": "ours", "If-Match": base})
	if res.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
}

func TestAPreconditionAgainstNothingIsRefused(t *testing.T) {
	t.Parallel()
	// If-Match names a revision to match, and there is none. Accepting it
	// would let a client believe it had won a race that was never run.
	s := newSurface(t, nil)
	res := s.do(t, http.MethodPut, "/config", companyDoc,
		map[string]string{"X-Summary": "first", "If-Match": "some-revision"})
	if res.Code != http.StatusPreconditionFailed {
		t.Fatalf("got %d, want 412", res.Code)
	}
}

// --- revert ----------------------------------------------------------------

func TestARevertIsANewRevision(t *testing.T) {
	t.Parallel()
	// Never a pointer moved backwards. The history stays append-only, so
	// "we reverted at 04:12" is a fact somebody can find later — and the
	// epoch keeps advancing, which is what makes every node reconcile.
	s := newSurface(t, nil)
	first := s.seed(t, companyDoc, nil)
	s.seed(t, strings.Replace(companyDoc, "name: Acme", "name: Acme Broken", 1), nil)

	res := s.do(t, http.MethodPost, "/config/revisions/"+first+"/revert", "", nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	reverted := decode(t, res)["revision_id"]
	if reverted == first {
		t.Fatal("the revert reused the old revision instead of writing a new one")
	}
	active := s.do(t, http.MethodGet, "/config", "", nil)
	if !strings.Contains(active.Body.String(), `"Acme"`) {
		t.Errorf("the reverted company is not active: %s", active.Body)
	}
	revisions := s.do(t, http.MethodGet, "/config/revisions", "", nil)
	var all []map[string]any
	if err := json.Unmarshal(revisions.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("%d revisions, want the two originals plus the revert", len(all))
	}
}

func TestRevertingToAnUnreadableRevisionIsRefused(t *testing.T) {
	t.Parallel()
	// Activating a document every node will fail to read is worse than
	// refusing, and finding out now is the point.
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": key(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newSurface(t, nil)
	// Sealed under a keyring this surface does not have.
	sealedID := s.seed(t, companyDoc, cipher)

	res := s.do(t, http.MethodPost, "/config/revisions/"+sealedID+"/revert", "", nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409", res.Code)
	}
	if decode(t, res)["error"] != "unreadable_revision" {
		t.Errorf("body = %s", res.Body)
	}
}

func TestASealedStoreRoundTripsThroughTheSurface(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": key(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newSurface(t, cipher)
	if got := s.do(t, http.MethodPut, "/config", companyDoc,
		map[string]string{"X-Summary": "sealed import"}).Code; got != http.StatusCreated {
		t.Fatalf("put: %d", got)
	}
	// Stored sealed...
	active, found, err := s.configs.Active(t.Context())
	if err != nil || !found {
		t.Fatal(err)
	}
	if !secrets.Sealed(active.Payload) {
		t.Fatal("a keyring was configured and the revision was stored in plaintext")
	}
	// ...and read back through the surface.
	res := s.do(t, http.MethodGet, "/config", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("get: %d", res.Code)
	}
	if !strings.Contains(res.Body.String(), "Acme") {
		t.Errorf("the sealed config did not come back: %s", res.Body)
	}
}

func TestABodyOverTheCapIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	huge := companyDoc + "\n# " + strings.Repeat("x", configapi.MaxBodyBytes)
	res := s.do(t, http.MethodPut, "/config", huge, map[string]string{"X-Summary": "big"})
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", res.Code)
	}
}

func TestANodeWithNoStoreServesNoConfigSurface(t *testing.T) {
	t.Parallel()
	// A standalone API with no store genuinely has no config surface, so
	// the routes 404 rather than 500 — the honest answer for a process that
	// does not implement them.
	mux := http.NewServeMux()
	configapi.New(configapi.Options{}).Routes(mux)
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", res.Code)
	}
}

func key(t *testing.T) []byte {
	t.Helper()
	material, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return material
}

// --- the paths that only open when something else is broken ----------------

func TestAStoreThatCannotBeReadIsNotBlamedOnTheCaller(t *testing.T) {
	t.Parallel()
	// 500, not 404: "there is no revision" and "I could not look" are
	// opposite facts, and answering the second as the first would make a
	// database outage read as a company nobody had configured.
	//
	// The REASON stays in the log. A store error can carry a database path
	// or a driver's own message, and this surface is one an operator
	// reaches from a browser.
	s := newSurface(t, nil)
	id := s.seed(t, companyDoc, nil)
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/config", "/config/revisions", "/config/revisions/" + id,
		"/config/revisions/" + id + "/diff",
	} {
		res := s.do(t, http.MethodGet, path, "", nil)
		if res.Code != http.StatusInternalServerError {
			t.Errorf("%s got %d, want 500", path, res.Code)
		}
		// The reason stays in the log: a store error can carry a
		// database path or a driver's own message, and this surface is
		// one an operator reaches from a browser.
		if got := decode(t, res)["error"]; got != "internal_error" {
			t.Errorf("%s answered %v, want an opaque internal_error", path, got)
		}
	}
	write := s.do(t, http.MethodPut, "/config", companyDoc,
		map[string]string{"X-Summary": "while the store is down"})
	if write.Code != http.StatusInternalServerError {
		t.Errorf("a write against a closed store got %d, want 500", write.Code)
	}
}

func TestADiffAgainstANamedRevision(t *testing.T) {
	t.Parallel()
	// The default is the active revision, which answers "what would change
	// if I reverted to this". Naming both sides answers "what happened
	// between these two", which is the question after an incident.
	s := newSurface(t, nil)
	first := s.seed(t, companyDoc, nil)
	second := s.seed(t, strings.Replace(companyDoc, "name: Acme", "name: Acme Two", 1), nil)

	res := s.do(t, http.MethodGet, "/config/revisions/"+second+"/diff?against="+first, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	var body struct {
		From, To string
		Changes  []configapi.Change
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.From != first || body.To != second {
		t.Errorf("from/to = %s/%s, want %s/%s", body.From, body.To, first, second)
	}
	if len(body.Changes) != 1 || body.Changes[0].Path != "name" {
		t.Fatalf("changes = %+v, want the one field that differs", body.Changes)
	}
	if body.Changes[0].From != "Acme" || body.Changes[0].To != "Acme Two" {
		t.Errorf("change = %+v", body.Changes[0])
	}
}

func TestADiffAgainstSomethingMissingSaysWhichSide(t *testing.T) {
	t.Parallel()
	// "the revision you asked about" and "the one you asked to compare it
	// with" are different mistakes, and a single not_found makes the caller
	// check both.
	s := newSurface(t, nil)
	id := s.seed(t, companyDoc, nil)
	res := s.do(t, http.MethodGet, "/config/revisions/"+id+"/diff?against=nope", "", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", res.Code)
	}
	if decode(t, res)["error"] != "against_not_found" {
		t.Errorf("body = %s", res.Body)
	}
}

func TestADiffAgainstTheActiveOfAnUnconfiguredNode(t *testing.T) {
	t.Parallel()
	// There is no active revision to compare against, which is a different
	// answer from "the revision you named does not exist".
	s := newSurface(t, nil)
	id := s.seed(t, companyDoc, nil)
	// Deactivate everything, which is what a node looks like before its
	// first import.
	if _, err := s.db.SQL().ExecContext(t.Context(),
		`UPDATE company_config SET is_active = 0`); err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodGet, "/config/revisions/"+id+"/diff", "", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", res.Code)
	}
	if decode(t, res)["error"] != "no_active_revision" {
		t.Errorf("body = %s", res.Body)
	}
}

func TestAReshapedKeyListIsRefusedRatherThanStored(t *testing.T) {
	t.Parallel()
	// The end of the round-trip story. A caller who fetched the config,
	// added an api key and sent it back has changed which slot means what,
	// so the masks cannot be matched to what they hid — and this surface
	// refuses rather than storing the literal "__redacted__" as a
	// credential, which would fail at the first provider call with an error
	// naming nothing about where it came from.
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	fetched := s.do(t, http.MethodGet, "/config", "", nil)
	document := decode(t, fetched)
	providers, _ := document["providers"].(map[string]any)
	llm, _ := providers["llm"].(map[string]any)
	zulu, _ := llm["zulu"].(map[string]any)
	keys, _ := zulu["api_keys"].([]any)
	if len(keys) == 0 || keys[0] != config.Redacted {
		t.Fatalf("the fetched document carried no mask to reshape: %v", keys)
	}
	zulu["api_keys"] = append([]any{"${THIRD}"}, keys...)

	edited, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	res := s.do(t, http.MethodPut, "/config", string(edited),
		map[string]string{"X-Summary": "add a key"})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 — a mask that could not be resolved was stored: %s",
			res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "validation_error" {
		t.Fatalf("error = %v (%s)", body["error"], res.Body)
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "redaction marker") {
		t.Errorf("the reason does not explain what happened: %v", body["detail"])
	}
}
