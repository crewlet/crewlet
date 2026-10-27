package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/usage"
)

// readersAsk is one page a readers fake was asked for.
type readersAsk struct {
	q         usage.RepoReadsQuery
	cursor    string
	pageBytes int
}

// readersFake is the usage domain's readers of a repository as the readers
// tool asks them: one scripted page, every ask recorded.
type readersFake struct {
	mu     sync.Mutex
	page   estate.RepoReadersAnswer
	asked  []readersAsk
	answer error
}

func (f *readersFake) RepoReaders(_ context.Context, q usage.RepoReadsQuery, cursor string,
	pageBytes int) (estate.RepoReadersAnswer, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, readersAsk{q: q, cursor: cursor, pageBytes: pageBytes})
	return f.page, f.answer
}

func (f *readersFake) asks() []readersAsk {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]readersAsk(nil), f.asked...)
}

// asAdmin is a person holding admin, signed in through a machine token of
// theirs, bound to the human seat they hold.
func asAdmin() context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee5"),
		Kind: iam.KindPerson, Login: "jane.founder", Seat: "founder", Via: "pat:jane-laptop",
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
	})
}

// readersCatalogue is the operator catalogue over host and readers, the
// company's clock Santiago's at noon of 2026-09-23.
func readersCatalogue(host builtin.CodeReader, readers builtin.RepoReaders) map[string]tools.Callable {
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		panic(err)
	}
	out := map[string]tools.Callable{}
	var deps builtin.ReadersDeps
	if readers != nil {
		deps = builtin.ReadersDeps{Readers: readers, Zone: func() *time.Location { return santiago },
			Now: func() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, santiago) }}
	}
	var codeDeps builtin.CodeDeps
	if host != nil {
		codeDeps = builtin.CodeDeps{Code: host, Actor: builtin.PrincipalActor}
	}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Code: codeDeps, Readers: deps, Authorize: builtin.Decide(chartLeads),
	}) {
		out[tool.Name()] = tool
	}
	return out
}

// WHO READ A REPOSITORY IS A PERSON'S QUESTION, AND AN ADMIN'S: the operator
// catalogue serves code_repo_readers beside the code reads and on their terms
// — not without them, nor without the usage domain's readers — no seat's
// registry ever holds it, and a person who is not an admin is refused it at the
// gate with nothing read.
//
// Mutation: add code_repo_readers to CodeTools, and a seat's registry holds it.
func TestTheReadersToolIsAnAdminsAlone(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	readers := &readersFake{}
	if _, ok := readersCatalogue(host, readers)[builtin.CodeRepoReadersTool]; !ok {
		t.Fatal("the operator catalogue does not serve code_repo_readers beside the code reads")
	}
	if _, ok := readersCatalogue(host, nil)[builtin.CodeRepoReadersTool]; ok {
		t.Error("the operator catalogue served code_repo_readers with no readers to read")
	}
	if _, ok := readersCatalogue(nil, readers)[builtin.CodeRepoReadersTool]; ok {
		t.Error("the operator catalogue served code_repo_readers to a company hosting no code")
	}
	reg := codeRegistry(t, builtin.CodeDeps{Code: host})
	if _, ok := reg.Lookup(builtin.CodeRepoReadersTool); ok {
		t.Error("a seat's registry holds code_repo_readers")
	}
	for _, tool := range builtin.CodeTools(builtin.CodeDeps{Code: host}) {
		if tool.Name() == builtin.CodeRepoReadersTool {
			t.Error("CodeTools, the list a seat's registry takes, lists code_repo_readers")
		}
	}
	if a := builtin.AnnotationsFor(builtin.CodeRepoReadersTool); !mcp.ReadOnlyProven(a) || a.Idempotent != mcp.Yes {
		t.Errorf("code_repo_readers is annotated %+v, want a proven, idempotent read", a)
	}

	tool := readersCatalogue(host, readers)[builtin.CodeRepoReadersTool]
	got, err := tool.Call(asBoundPerson(), map[string]any{"repo": "site"})
	if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden {
		t.Fatalf("a person who is not an admin was answered %v %+v, want a forbidden refusal", err, got)
	}
	if reads, _, _ := host.asked(); len(reads) != 0 || len(readers.asks()) != 0 {
		t.Errorf("a refused call read the repository %d time(s) and its readers %d", len(reads),
			len(readers.asks()))
	}
}

// THE READERS TOOL ANSWERS WHO READ A UNIT REPOSITORY: the repository is read by
// name at the seat's level as the caller, and its readers are asked over the
// range — the thirty company days ending today on the company's clock when the
// call names none — a page at the tool's own bound, from the cursor the call
// hands back; the answer is the domain's page whole, the readers the first page
// left out included. A company repository's readers are not recorded, and its
// answer says so without asking; a range or a cursor the read cannot take is
// refused by name before anything is read.
func TestTheReadersToolAnswersWhoReadAUnitRepository(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	host.detail.Visibility = code.VisibilityUnit
	elided := int64(3)
	last := usage.RepoReader{PrincipalKind: "operator", Principal: "pat:jane-laptop", Seat: "founder", Via: "git",
		Reads: 2, Bytes: 1 << 20}
	// THE CURSOR A PAGE ISSUES, which is what a caller hands back: one no
	// page issued is refused before the readers are asked (below).
	next := last.Group().Cursor()
	readers := &readersFake{page: estate.RepoReadersAnswer{
		Readers: []usage.RepoReader{
			{PrincipalKind: "agent", Principal: "dev", Seat: "dev", Via: "tool", Reads: 4, Bytes: 900}, last,
		},
		Elided: &elided, Next: next}}
	tool := readersCatalogue(host, readers)[builtin.CodeRepoReadersTool]

	got, err := tool.Call(asAdmin(), map[string]any{"repo": "site"})
	if err != nil || got.Failed {
		t.Fatalf("an admin's call = %v %q", err, got.Output)
	}
	var answer builtin.ReadersAnswer
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("decode %q: %v", got.Output, err)
	}
	if !answer.Recorded || answer.Repo != "site" || answer.From != "2026-08-25" || answer.To != "2026-09-23" ||
		len(answer.Readers) != 2 || answer.Readers[1].Principal != "pat:jane-laptop" ||
		answer.ReadersElided == nil || *answer.ReadersElided != 3 || answer.Next != next {
		t.Errorf("the answer is %+v, want the domain's page over the thirty days ending today", answer)
	}
	reads, _, _ := host.asked()
	if len(reads) != 1 || reads[0].Kind != code.ReadRepo || reads[0].Fresh.Level != statelog.ReadLinearizable ||
		reads[0].Asker != (code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:jane-laptop",
			Seat: "founder"}) {
		t.Errorf("the repository was read %+v, want once, by name, at linearizable, as the caller", reads)
	}
	asks := readers.asks()
	if len(asks) != 1 || asks[0].q != (usage.RepoReadsQuery{RepoID: siteID, From: "2026-08-25", To: "2026-09-23"}) ||
		asks[0].cursor != "" || asks[0].pageBytes != builtin.RepoReadersPageBytes {
		t.Errorf("the readers were asked %+v, want the repository's id over the range at the tool's page", asks)
	}

	// THE NEXT PAGE, from the cursor handed back, over the range named.
	if got, err := tool.Call(asAdmin(), map[string]any{"repo": "site", "from": "2026-09-01", "to": "2026-09-10",
		"cursor": next}); err != nil || got.Failed {
		t.Fatalf("the next page's call = %v %q", err, got.Output)
	}
	if asks = readers.asks(); len(asks) != 2 || asks[1].cursor != next ||
		asks[1].q.From != "2026-09-01" || asks[1].q.To != "2026-09-10" {
		t.Errorf("the next page was asked %+v, want the cursor and the range named", asks[len(asks)-1])
	}

	for name, args := range map[string]map[string]any{
		"a range longer than the rows keep":  {"repo": "site", "from": "2026-01-01", "to": "2026-09-23"},
		"a range that ends before it starts": {"repo": "site", "from": "2026-09-10", "to": "2026-09-01"},
		"a day that is not one":              {"repo": "site", "to": "yesterday"},
		"a cursor no page issued":            {"repo": "site", "cursor": "not a cursor!"},
	} {
		before := len(readers.asks())
		got, err := tool.Call(asAdmin(), args)
		// IN THE CALLER'S WORDS: the domain's sentence past its sentinel,
		// never a Go package's name for the class.
		if err != nil || !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
			strings.Contains(got.Output, "usage:") {
			t.Errorf("%s was answered %v %+v, want an invalid refusal in the caller's words", name, err, got)
		}
		if len(readers.asks()) != before {
			t.Errorf("%s asked the readers anyway", name)
		}
	}

	host.detail.Visibility = code.VisibilityCompany
	before := len(readers.asks())
	got, err = tool.Call(asAdmin(), map[string]any{"repo": "site"})
	if err != nil || got.Failed {
		t.Fatalf("a company repository's call = %v %q", err, got.Output)
	}
	var company builtin.ReadersAnswer
	if err := json.Unmarshal([]byte(got.Output), &company); err != nil {
		t.Fatal(err)
	}
	if company.Recorded || company.Readers != nil || !strings.Contains(company.Detail, "visibility: unit") {
		t.Errorf("a company repository's answer is %+v, want not recorded, saying how to have it recorded",
			company)
	}
	if len(readers.asks()) != before {
		t.Error("a company repository's readers were asked for")
	}
}
