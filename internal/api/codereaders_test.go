package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// readersAsked is one page the readers route asked the usage domain for.
type readersAsked struct {
	q         usage.RepoReadsQuery
	cursor    string
	pageBytes int
}

// fakeRepoReaders is the usage domain's readers of a repository as the readers
// route asks them: one scripted page, or fail, every ask recorded.
type fakeRepoReaders struct {
	page estate.RepoReadersAnswer
	fail error

	mu    sync.Mutex
	asked []readersAsked
}

func (f *fakeRepoReaders) RepoReaders(_ context.Context, q usage.RepoReadsQuery, cursor string,
	pageBytes int) (estate.RepoReadersAnswer, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, readersAsked{q: q, cursor: cursor, pageBytes: pageBytes})
	return f.page, f.fail
}

func (f *fakeRepoReaders) asks() []readersAsked {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// The repositories the readers route's code reads answer: a unit's, the
// company's, and a name nobody declared.
const (
	unitRepoID    = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e"
	companyRepoID = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9f"
)

// readersRepos is the code reads the readers route resolves a repository
// through: `site` a unit's, `public` the company's, anything else undeclared.
func readersRepos() *fakeCodeReads {
	return &fakeCodeReads{answer: func(q code.ReadQuery) (code.ReadAnswer, error) {
		ref := code.RepoRef{Name: q.Repo, Status: code.RepoStatusActive}
		switch q.Repo {
		case "site":
			ref.ID, ref.Visibility = unitRepoID, code.VisibilityUnit
		case "public":
			ref.ID, ref.Visibility = companyRepoID, code.VisibilityCompany
		default:
			return code.ReadAnswer{}, code.ErrRepoNotFound
		}
		return code.ReadAnswer{Repo: &code.RepoDetail{RepoSummary: code.RepoSummary{RepoRef: ref}},
			Level: q.Fresh.Level}, nil
	}}
}

// readersOn is the readers half over readers, the company's clock Santiago's
// at noon of 2026-09-23 — so a range that names no end is 2026-08-25..
// 2026-09-23, which UTC's day would not be at Santiago's late evening.
func readersOn(t *testing.T, readers builtin.RepoReaders) builtin.ReadersDeps {
	t.Helper()
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	return builtin.ReadersDeps{Readers: readers, Zone: func() *time.Location { return santiago },
		Now: func() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, santiago) }}
}

// WHO READ A REPOSITORY IS ADMIN'S, ON THE ROUTE AS ON THE TOOL: one verb
// (authz.ActionCodeRepoReaders) decides both, so a caller holding every app
// and not admin is refused `403` naming admin before the repository or its
// readers are read — the per-repository grant that admits them to the code
// reads gives them no audit of who read it — and an admin is answered.
//
// Mutation: mount the route on code.read, and the colleague is answered.
func TestTheReadersRouteIsAnAdminsAlone(t *testing.T) {
	t.Parallel()
	const colleague = "a-token-that-carries-every-app-and-not-admin"
	b := closedPosture()
	authorize(&b, config.APIToken{ID: "founder", Token: "secret"},
		config.APIToken{ID: "colleague", Token: colleague, Grants: slices.Clone(iam.Apps)})
	reads, readers := readersRepos(), &fakeRepoReaders{}
	a := newApp(t, api.Options{Bootstrap: &b, Code: fixedCode{reads: reads, readers: readersOn(t, readers)}})

	rec := ask(t, bearing(a, colleague), http.MethodGet, "/code/repos/site/readers", false)
	var body struct {
		Error  string   `json:"error"`
		Grants []string `json:"grants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the colleague was answered %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	if rec.Code != http.StatusForbidden || body.Error != string(httpjson.CodeUnauthorized) ||
		!slices.Equal(body.Grants, []string{string(iam.GrantAdmin)}) {
		t.Errorf("a caller holding every app and not admin was answered %d %s, want 403 %s naming [%s]",
			rec.Code, rec.Body.String(), httpjson.CodeUnauthorized, iam.GrantAdmin)
	}
	if len(reads.queries()) != 0 || len(readers.asks()) != 0 {
		t.Errorf("a refused request read the repository %d time(s) and its readers %d",
			len(reads.queries()), len(readers.asks()))
	}
	if rec := ask(t, a, http.MethodGet, "/code/repos/site/readers", true); rec.Code != http.StatusOK {
		t.Errorf("an admin was answered %d %q, want the readers", rec.Code, rec.Body.String())
	}
}

// THE READERS ROUTE ANSWERS WHO READ A UNIT'S REPOSITORY, as the operator tool
// does (builtin.ReadersOf): the repository read by name as the caller at
// linearizable — never answered undeclared from an old copy — and one page of
// its readers over the range — the thirty company days ending today on the
// company's clock where the request names neither end — at the tool's own page
// bound, from the cursor handed back; the first page with how many readers the
// per-day cap left out. A company repository's readers are not recorded, and
// its answer says so without asking. A range or a cursor the read cannot take
// is `bad_params` before anything is read, as one a peer refuses is; a name
// nobody declared is the code reads' `not_found`; a read no node served is
// `unavailable` with its Retry-After.
//
// Mutation: answer ErrBadRange through codeRefusal, and the range rows answer
// 500 query_failed.
func TestTheReadersRouteAnswersWhoReadAUnitRepository(t *testing.T) {
	t.Parallel()
	elided := int64(2)
	last := usage.RepoReader{PrincipalKind: "operator", Principal: "pat:jane-laptop", Seat: "founder",
		Via: "git", Reads: 3, Bytes: 4 << 20, LastAt: time.Date(2026, 9, 22, 18, 4, 0, 0, time.UTC)}
	next := last.Group().Cursor()
	readers := &fakeRepoReaders{page: estate.RepoReadersAnswer{Readers: []usage.RepoReader{
		{PrincipalKind: "agent", Principal: "dev", Seat: "dev", Via: "tool", Reads: 9, Bytes: 7_000}, last,
	}, Elided: &elided, Next: next}}
	reads := readersRepos()
	a := codeApp(t, nil, fixedCode{reads: reads, readers: readersOn(t, readers)})

	rec := ask(t, a, http.MethodGet, "/code/repos/site/readers", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /code/repos/site/readers answered %d %q", rec.Code, rec.Body.String())
	}
	var answer builtin.ReadersAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if !answer.Recorded || answer.Repo != "site" || answer.From != "2026-08-25" || answer.To != "2026-09-23" ||
		len(answer.Readers) != 2 || answer.Readers[1] != last || answer.ReadersElided == nil ||
		*answer.ReadersElided != 2 || answer.Next != next {
		t.Errorf("the answer is %+v, want the domain's page over the thirty days ending today", answer)
	}
	asked := reads.queries()
	if len(asked) != 1 || asked[0].Kind != code.ReadRepo || asked[0].Repo != "site" ||
		asked[0].Fresh.Level != statelog.ReadLinearizable ||
		asked[0].Asker != (code.Principal{Kind: code.PrincipalOperator, OperatorID: iam.TokenLogin("founder")}) {
		t.Errorf("the repository was read %+v, want once, by name, at linearizable, as the caller", asked)
	}
	if got := readers.asks(); len(got) != 1 || got[0].q != (usage.RepoReadsQuery{RepoID: unitRepoID,
		From: "2026-08-25", To: "2026-09-23"}) || got[0].cursor != "" ||
		got[0].pageBytes != builtin.RepoReadersPageBytes {
		t.Errorf("the readers were asked %+v, want the repository's id over the range at the tool's page", got)
	}

	// THE NEXT PAGE, from the cursor handed back, over the range named.
	path := "/code/repos/site/readers?from=2026-09-01&to=2026-09-10&cursor=" + next
	if rec := ask(t, a, http.MethodGet, path, true); rec.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d %q", path, rec.Code, rec.Body.String())
	}
	if got := readers.asks(); len(got) != 2 || got[1].cursor != next || got[1].q.From != "2026-09-01" ||
		got[1].q.To != "2026-09-10" {
		t.Errorf("the next page was asked %+v, want the cursor and the range named", got[len(got)-1])
	}

	for _, tc := range []struct {
		name, path string
		status     int
		code       string
		detail     string
		retryAfter string
	}{
		{"a range longer than the rows keep", "/code/repos/site/readers?from=2026-01-01&to=2026-09-23",
			http.StatusBadRequest, "bad_params", "longer than", ""},
		{"a range that ends before it starts", "/code/repos/site/readers?from=2026-09-10&to=2026-09-01",
			http.StatusBadRequest, "bad_params", "ends before it starts", ""},
		{"a day that is not one", "/code/repos/site/readers?to=yesterday",
			http.StatusBadRequest, "bad_params", "yesterday", ""},
		{"a cursor no page issued", "/code/repos/site/readers?cursor=not-a-cursor",
			http.StatusBadRequest, "bad_params", "cursor", ""},
		{"a name nobody declared", "/code/repos/web/readers",
			http.StatusNotFound, "not_found", `"web" is declared in code.repos`, ""},
	} {
		beforeReads, beforeReaders := len(reads.queries()), len(readers.asks())
		rec := ask(t, a, http.MethodGet, tc.path, true)
		code, detail := errorBody(t, rec)
		// IN THE CALLER'S WORDS, the tool's own: the domain's sentence past
		// its sentinel, never a Go package's name for the class.
		if rec.Code != tc.status || code != tc.code || !strings.Contains(detail, tc.detail) ||
			strings.Contains(detail, "usage:") || rec.Header().Get("Retry-After") != tc.retryAfter {
			t.Errorf("%s answered %d %s %q (Retry-After %q), want %d %s naming %q", tc.name, rec.Code, code,
				detail, rec.Header().Get("Retry-After"), tc.status, tc.code, tc.detail)
		}
		if len(readers.asks()) != beforeReaders {
			t.Errorf("%s asked the readers", tc.name)
		}
		if tc.status == http.StatusBadRequest && len(reads.queries()) != beforeReads {
			t.Errorf("%s read the repository before it was refused", tc.name)
		}
	}

	// A COMPANY REPOSITORY'S READERS ARE NOT RECORDED, and its answer says
	// so — and how to have them recorded — without asking for them.
	before := len(readers.asks())
	rec = ask(t, a, http.MethodGet, "/code/repos/public/readers", true)
	var company builtin.ReadersAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &company); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("a company repository was answered %d %q (%v)", rec.Code, rec.Body.String(), err)
	}
	if company.Recorded || company.Readers != nil || !strings.Contains(company.Detail, "visibility: unit") {
		t.Errorf("a company repository's answer is %+v, want not recorded, saying how to have it recorded",
			company)
	}
	if len(readers.asks()) != before {
		t.Error("a company repository's readers were asked for")
	}

	// WHAT THE USAGE DOMAIN COULD NOT ANSWER: no data node serving is a
	// read this node could not serve now, and a cursor a peer refused the
	// caller's mistake, on every node alike.
	for _, tc := range []struct {
		name       string
		fail       error
		status     int
		code       string
		detail     string
		retryAfter string
	}{
		{"no data node serving", &estate.ErrUnserved{Detail: "no data node answered"},
			http.StatusServiceUnavailable, "unavailable", "", "5"},
		{"a cursor a peer refused", fmt.Errorf("estate: data node n2 answered: %w",
			fmt.Errorf("%w: a repo-readers page resumes after a group of 4 columns, not 2", usage.ErrBadCursor)),
			http.StatusBadRequest, "bad_params", "4 columns, not 2", ""},
	} {
		failing := &fakeRepoReaders{fail: tc.fail}
		a := codeApp(t, nil, fixedCode{reads: readersRepos(), readers: readersOn(t, failing)})
		rec := ask(t, a, http.MethodGet, "/code/repos/site/readers", true)
		if code, detail := errorBody(t, rec); rec.Code != tc.status || code != tc.code ||
			!strings.Contains(detail, tc.detail) || strings.Contains(detail, "usage:") ||
			rec.Header().Get("Retry-After") != tc.retryAfter {
			t.Errorf("%s answered %d %s %q (Retry-After %q), want %d %s naming %q (Retry-After %q)", tc.name,
				rec.Code, code, detail, rec.Header().Get("Retry-After"), tc.status, tc.code, tc.detail,
				tc.retryAfter)
		}
	}
}

// A NODE WHOSE READERS ARE ABSENT SAYS WHY, as its code reads do: the readers
// stand with the code reads, so a surface that serves the reads and not the
// readers answers the code host's own reason rather than a 500.
func TestTheReadersRouteAnswersWhyItServesNone(t *testing.T) {
	t.Parallel()
	a := codeApp(t, nil, fixedCode{reads: readersRepos(), why: engine.ErrCodeApplying})
	rec := ask(t, a, http.MethodGet, "/code/repos/site/readers", true)
	if code, _ := errorBody(t, rec); rec.Code != http.StatusServiceUnavailable || code != "applying" ||
		rec.Header().Get("Retry-After") != "5" {
		t.Errorf("a node serving no readers answered %d %s (Retry-After %q), want 503 applying (Retry-After 5)",
			rec.Code, code, rec.Header().Get("Retry-After"))
	}
}
