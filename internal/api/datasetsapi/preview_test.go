package datasetsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// answeringQuestions answers every question with answer, or refuses it with
// refusal, and counts the asks, so a suite can tell a read from a cached one.
type answeringQuestions struct {
	answer  any
	refusal *queries.Refusal
	asks    int
	asked   map[string]any
}

func (q *answeringQuestions) AnswerWith(_ context.Context, _ string, p queries.Params) (any, error) {
	q.asks++
	q.asked = p.Values()
	if q.refusal != nil {
		return nil, q.refusal
	}
	return q.answer, nil
}

func (*answeringQuestions) PostureOf(string) queries.Posture { return queries.Posture{} }

// previewOf is the preview route's answer to path, asked as who.
func previewOf(t *testing.T, s *Service, who *iam.Principal, path string) (*httptest.ResponseRecorder, Preview) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /artifacts/sources/preview/{name}", s.preview)
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if who != nil {
		r = r.WithContext(iam.WithPrincipal(r.Context(), *who))
	} else {
		r = r.WithContext(iam.WithAnonymous(r.Context()))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var out Preview
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", w.Body.String(), err)
		}
	}
	return w, out
}

func previewService(t *testing.T, q *answeringQuestions) *Service {
	t.Helper()
	s, err := New(Options{Boards: nopBoards{}, Chart: authz.NoChart{}, Questions: q, Data: nopData{}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A PREVIEW IS THE BOARD'S OWN ENGINE READ, as the viewer: the source's rows
// projected to its columns, the arguments held to the catalogue and filled
// with its defaults, and a second preview within the cache's life answered
// without asking the domain again.
func TestAPreviewAnswersASourcesRowsAsTheViewer(t *testing.T) {
	q := &answeringQuestions{answer: map[string]any{"items": []worklet.TaskRow{
		{Key: "ENG-1", Title: "Ship it", Status: "in_progress", StatusGroup: "active"},
		{Key: "ENG-2", Title: "Plan it", Status: "todo", StatusGroup: "not_started"},
	}}}
	s := previewService(t, q)
	ana := iam.Principal{ID: uuid.New(), Login: "ana.lee", Kind: iam.KindPerson, Grants: []iam.Grant{iam.GrantWorklet}}

	w, out := previewOf(t, s, &ana, "/artifacts/sources/preview/tracker.items?scope=blocked")
	if w.Code != http.StatusOK || out.Status != StatusReady || out.Source != "tracker.items" {
		t.Fatalf("preview = %d %s", w.Code, w.Body.String())
	}
	if out.Table == nil || len(out.Table.Rows) != 2 || out.Table.Rows[0]["key"] != "ENG-1" || out.AsOf == nil {
		t.Errorf("table = %+v", out.Table)
	}
	if q.asked["preset"] != "blocked" || q.asked["limit"] == nil {
		t.Errorf("the question was asked with %v, want the sent scope and the source's default limit", q.asked)
	}
	if _, _ = previewOf(t, s, &ana, "/artifacts/sources/preview/tracker.items?scope=blocked"); q.asks != 1 {
		t.Errorf("the domain was asked %d times for one principal's same read, want 1 (cached)", q.asks)
	}
}

// A SOURCE THE VIEWER MAY NOT READ IS ANSWERED AS ITS REFUSAL, naming the rule
// and the grants that would admit them, never as rows and never as an HTTP
// failure: the builder draws the source as closed to this reader.
func TestAPreviewOfASourceTheViewerMayNotReadIsItsRefusal(t *testing.T) {
	q := &answeringQuestions{refusal: queries.NewRefusal("work_items", authz.Decision{
		Reason: authz.ReasonNoGrant, Grants: []iam.Grant{iam.GrantWorklet}})}
	s := previewService(t, q)
	bo := iam.Principal{ID: uuid.New(), Login: "bo.k", Kind: iam.KindPerson}

	w, out := previewOf(t, s, &bo, "/artifacts/sources/preview/tracker.items")
	if w.Code != http.StatusOK || out.Status != StatusRefused {
		t.Fatalf("preview = %d %s, want 200 refused", w.Code, w.Body.String())
	}
	if out.Table != nil || out.Refusal == nil || out.Refusal.Reason != authz.ReasonNoGrant ||
		len(out.Refusal.Grants) != 1 || out.Refusal.Grants[0] != iam.GrantWorklet || out.Detail == "" {
		t.Errorf("refused preview = %+v", out)
	}
}

// What a preview may ask is the catalogue's alone: an unknown source, a
// parameter the source does not take, a value outside its type and a key sent
// twice are each refused before any domain is asked, and nobody unresolved is
// answered at all.
func TestAPreviewAsksNothingTheCatalogueDoesNotOffer(t *testing.T) {
	q := &answeringQuestions{answer: map[string]any{"items": []any{}}}
	s := previewService(t, q)
	ana := iam.Principal{ID: uuid.New(), Login: "ana.lee", Kind: iam.KindPerson}
	for _, c := range []struct {
		name string
		who  *iam.Principal
		path string
		want int
	}{
		{"an unknown source", &ana, "/artifacts/sources/preview/tracker.secrets", http.StatusNotFound},
		{"a parameter the source does not take", &ana, "/artifacts/sources/preview/tracker.items?owner=cto",
			http.StatusBadRequest},
		{"a value outside the enum", &ana, "/artifacts/sources/preview/tracker.items?scope=everyone",
			http.StatusBadRequest},
		{"a number past its bound", &ana, "/artifacts/sources/preview/spend.daily?days=4000", http.StatusBadRequest},
		{"a key sent twice", &ana, "/artifacts/sources/preview/tracker.items?scope=all&scope=mine",
			http.StatusBadRequest},
		{"nobody signed in", nil, "/artifacts/sources/preview/tracker.items", http.StatusUnauthorized},
	} {
		if w, _ := previewOf(t, s, c.who, c.path); w.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, w.Code, w.Body.String(), c.want)
		}
	}
	if q.asks != 0 {
		t.Errorf("a refused preview asked the domain %d times, want none", q.asks)
	}
}
