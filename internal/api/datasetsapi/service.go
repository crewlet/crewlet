package datasetsapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("datasetsapi")

// Boards is the artifacts surface's admission of a version's content and its
// read of the version's board, each rendering its own refusal.
type Boards interface {
	AdmitContent(w http.ResponseWriter, r *http.Request, n int) (artifactsapi.Admission, bool)
	Board(w http.ResponseWriter, r *http.Request, a artifactsapi.Admission, n int) (*dashspec.Board, bool)
}

// Questions is the question registry, asked on the request's own context, so
// the principal deciding an answer is the viewer the guard resolved.
type Questions interface {
	AnswerWith(ctx context.Context, what string, p queries.Params) (any, error)
	PostureOf(what string) queries.Posture
}

// Data is the datastore's read of a collection's documents, under the read
// rule of the reader a [artifactdata.Reading] names.
type Data interface {
	Docs(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string, since int64,
		cursor string, limit int) (artifactdata.DocPage, error)
}

// Options configure the surface.
type Options struct {
	// Boards admits a read and reads the board. REQUIRED.
	Boards Boards

	// Chart is the chart the request guard decides a route's policy
	// through. REQUIRED; pass authz.NoChart to decide on grants alone.
	Chart authz.Chart

	// Questions answers engine sources. REQUIRED.
	Questions Questions

	// Agents is the agents' overview as `GET /agents` answers it for the
	// principal the context carries, or that route's refusal as a
	// [*queries.Refusal]. Nil is a node with no live roster, whose
	// `agents.status` is answered as not served here.
	Agents func(ctx context.Context) (any, error)

	// Data reads store datasets. REQUIRED.
	Data Data

	// Now is the clock an answer is stamped and cached on; nil is the wall
	// clock.
	Now func() time.Time
}

// Service is the surface.
type Service struct {
	boards    Boards
	chart     authz.Chart
	questions Questions
	agents    func(ctx context.Context) (any, error)
	data      Data
	now       func() time.Time
	budget    *costlimit.Limiter
	cache     *answerCache
}

// New builds the surface, refusing a binding the catalogue does not list, a
// source it does not bind, and a bound question whose posture decides on
// anything but the principal's grants: an answer to one could not be cached
// per principal without the relation its answer depends on.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Boards == nil:
		return nil, errors.New("datasetsapi: no boards: every dataset is read from its version's board")
	case opts.Chart == nil:
		return nil, errors.New("datasetsapi: no chart: pass authz.NoChart to decide on grants alone")
	case opts.Questions == nil:
		return nil, errors.New("datasetsapi: no question registry: an engine dataset is answered by it")
	case opts.Data == nil:
		return nil, errors.New("datasetsapi: no datastore: a store dataset is read from it")
	}
	if err := checkBindings(); err != nil {
		return nil, err
	}
	for name, b := range bindings {
		if b.question == "" {
			continue
		}
		posture := opts.Questions.PostureOf(b.question)
		if posture.Valid() && !posture.IsEverybody() && posture.Grant() == "" {
			return nil, fmt.Errorf("datasetsapi: %s is bound to %q, which is %s: a question decided "+
				"per record cannot be answered from a per-principal cache", name, b.question, posture)
		}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		boards: opts.Boards, chart: opts.Chart, questions: opts.Questions, agents: opts.Agents,
		data: opts.Data, now: now,
		budget: costlimit.New(costlimit.Rates{
			PrincipalRate: artifacts.DatasetReadsPerViewerPerSecond, PrincipalBurst: artifacts.DatasetReadsViewerBurst,
			ScopeRate: artifacts.DatasetReadsPerArtifactPerSecond, ScopeBurst: artifacts.DatasetReadsArtifactBurst,
		}, now),
		cache: newAnswerCache(),
	}, nil
}

// Routes mounts the surface. The dataset route admits on its weakest honest
// precondition, as every artifact route does, and the handler decides the
// read on the artifact through the artifacts' own gate before anything is
// read. The catalogue is the engine's own vocabulary, any resolved caller's.
// A source's preview is a builder's read, so it asks the artifacts app as making
// a board does, on a read verb of its own (`artifacts.source.read`); the rows
// themselves are decided by the source's own question, as the viewer, exactly
// as a board's dataset over it is.
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, authz.ContextGuard(s.chart))
	pre := authz.Policy{Action: authz.ActionArtifactList, Object: func(*http.Request) authz.Object {
		return authz.Object{Kind: authz.KindArtifact}
	}}
	preview := authz.Policy{Action: authz.ActionDashboardSourceRead, Object: func(*http.Request) authz.Object {
		return authz.Object{Kind: authz.KindArtifact}
	}}
	return errors.Join(
		router.Handle("GET /artifacts/{id}/versions/{n}/sources/{dataset}", pre, http.HandlerFunc(s.dataset)),
		router.Handle("GET /artifacts/sources", pre, http.HandlerFunc(s.catalogue)),
		router.Handle("GET /artifacts/sources/preview/{name}", preview, http.HandlerFunc(s.preview)),
	)
}

// CatalogueAnswer is the engine source catalogue (`GET /artifacts/sources`).
type CatalogueAnswer struct {
	Sources []dashspec.EngineSource `json:"sources"`
}

func (s *Service) catalogue(w http.ResponseWriter, _ *http.Request) {
	httpjson.Write(w, http.StatusOK, CatalogueAnswer{Sources: dashspec.Catalogue()})
}

// The statuses a dataset is answered with.
const (
	// StatusReady is a dataset whose rows are answered.
	StatusReady = "ready"

	// StatusRefused is a dataset the viewer may not read: a source whose
	// question refuses them, or a calculation over one.
	StatusRefused = "refused"

	// StatusUnanswered is a dataset this node cannot answer for this
	// viewer: a domain it does not run, or a personal scope for a
	// credential that names no person.
	StatusUnanswered = "unanswered"
)

// Answer is one dataset, as its viewer may read it.
type Answer struct {
	Artifact artifacts.ID `json:"artifact"`
	Version  int          `json:"version"`
	Dataset  string       `json:"dataset"`
	Reading
}

// Preview is one engine source read directly, as its viewer may read it
// (`GET /artifacts/sources/preview/{name}`): what a builder draws suggested
// figures from before any board exists.
type Preview struct {
	Source string `json:"source"`
	Reading
}

// Reading is what one read of a dataset or a source answered: its rows, or
// why the viewer is not handed any.
type Reading struct {
	Status string `json:"status"`

	// AsOf is when the rows were true: when the domain answered (a cached
	// answer keeps the instant it was read at), when the datastore was
	// read, and for a calculation its stalest input's.
	AsOf *time.Time `json:"as_of,omitempty"`

	Table *dashspec.Table `json:"table,omitempty"`

	// Refusal is why a refused dataset is closed to the viewer.
	Refusal *Refusal `json:"refusal,omitempty"`

	// Detail is the sentence a refused or unanswered dataset is drawn with.
	Detail string `json:"detail,omitempty"`
}

// Refusal is the rule that closed a dataset, and the grants that would open
// it, as every refusal on authority carries them.
type Refusal struct {
	// Dataset is the dataset whose source refused: the one asked, or an
	// input of a calculation.
	Dataset string       `json:"dataset"`
	Reason  authz.Reason `json:"reason"`
	Grants  []iam.Grant  `json:"grants,omitempty"`
}

// closed is a source the viewer may not read.
type closed struct {
	refusal Refusal
	detail  string
}

func (e *closed) Error() string { return "datasetsapi: " + e.refusal.Dataset + ": " + e.detail }

// unanswered is a source this node cannot answer for the viewer.
type unanswered struct{ detail string }

func (e *unanswered) Error() string { return "datasetsapi: " + e.detail }

// overBudget is a read the cost buckets refused for now.
type overBudget struct{ refusal *costlimit.Refusal }

func (e *overBudget) Error() string { return "datasetsapi: the live read budget is spent for now" }

// read is one request's reading: who asks, on which board, with which filters.
type read struct {
	ctx    context.Context
	a      artifactsapi.Admission
	board  *dashspec.Board
	params map[string]string

	// done memoises the datasets this request already read, so a dataset
	// two calculations stand on is read once.
	done map[string]result
}

type result struct {
	table dashspec.Table
	asOf  time.Time
	err   error
}

// dataset answers one dataset as its viewer.
func (s *Service) dataset(w http.ResponseWriter, r *http.Request) {
	n, ok := artifactsapi.PathVersion(w, r)
	if !ok {
		return
	}
	a, ok := s.boards.AdmitContent(w, r, n)
	if !ok {
		return
	}
	b, ok := s.boards.Board(w, r, a, n)
	if !ok {
		return
	}
	id := r.PathValue("dataset")
	if _, known := b.Spec.Dataset(id); !known {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "version's board has no dataset " + id})
		return
	}
	sent, ok := singleValues(w, r, "filter")
	if !ok {
		return
	}
	params, err := b.Spec.ResolveParams(sent)
	if err != nil {
		badParams(w, err)
		return
	}
	rd := &read{ctx: r.Context(), a: a, board: b, params: params, done: map[string]result{}}
	res := s.read(rd, id)
	out := Answer{Artifact: a.Head.ID, Version: n, Dataset: id}
	if !settle(w, r, res, &out.Reading, id, "this board's live data was read too often; it "+
		"refreshes once the budget refills", "artifact", a.Head.ID.String(), "dataset", id) {
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

// preview answers one engine source read directly, as its viewer, for a
// builder drawing figures before any board exists. It is the very read a
// board's dataset over the source is (the same question on the request's own
// context, the same per-principal cache, the viewer's own budget) and touches
// no artifact: nothing is opened, recorded or kept but the cached answer.
func (s *Service) preview(w http.ResponseWriter, r *http.Request) {
	p, how := iam.From(r.Context())
	if how != iam.Resolved {
		operator.RefuseDecision(w, r, authz.ActionDashboardSourceRead, authz.Decision{Err: iam.Reason(r.Context())})
		return
	}
	src, ok := dashspec.Source(r.PathValue("name"))
	if !ok {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "the catalogue has no engine source " + r.PathValue("name")})
		return
	}
	sent, ok := singleValues(w, r, "parameter")
	if !ok {
		return
	}
	args, err := dashspec.SourceArgs(src, sent)
	if err != nil {
		badParams(w, err)
		return
	}
	res := s.engineRows(r.Context(), p, previewScope(p), src.Name, src, args)
	out := Preview{Source: src.Name}
	if !settle(w, r, res, &out.Reading, src.Name, "your previews read live data too often; they "+
		"refresh once the budget refills", "source", src.Name) {
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

// previewScope is the budget's scope for one principal's previews: a preview
// has no artifact whose readers share a bucket, so a principal's previews are
// a scope of their own, held to a viewer's rate on one board.
func previewScope(p iam.Principal) string { return "preview\x00" + principalKey(p) }

// singleValues is the query string as one value per key, refusing a key sent
// twice; `what` names a key in the refusal ("filter", "parameter").
func singleValues(w http.ResponseWriter, r *http.Request, what string) (map[string]string, bool) {
	sent := map[string]string{}
	for key, values := range r.URL.Query() {
		if len(values) != 1 {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams, map[string]string{
				"detail": "the " + what + " " + key + " takes one value"})
			return nil, false
		}
		sent[key] = values[0]
	}
	return sent, true
}

// badParams answers a query string the spec or the catalogue refused.
func badParams(w http.ResponseWriter, err error) {
	httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams, map[string]string{
		"detail": strings.TrimPrefix(err.Error(), dashspec.ErrInvalid.Error()+": ")})
}

// settle writes res into out, or answers the request's failure itself and
// reports false: a spent budget (`spent` is its sentence) and a node that
// could not read are the request's, while a refusal and a source this node
// does not answer are the reading's own status. `what` names the read in a
// failure's sentence and `logged` its fields in the log.
func settle(w http.ResponseWriter, r *http.Request, res result, out *Reading, what, spent string,
	logged ...any) bool {

	var refused *closed
	var absent *unanswered
	var budget *overBudget
	switch {
	case res.err == nil:
		at := res.asOf
		table := res.table
		out.Status, out.Table, out.AsOf = StatusReady, &table, &at
	case errors.As(res.err, &refused):
		out.Status, out.Refusal, out.Detail = StatusRefused, &refused.refusal, refused.detail
	case errors.As(res.err, &absent):
		out.Status, out.Detail = StatusUnanswered, absent.detail
	case errors.As(res.err, &budget):
		httpjson.ThrottledWith(w, httpjson.CodeReadBudget, budget.refusal.RetryAfter,
			httpjson.Detail{"detail": spent})
		return false
	case errors.Is(res.err, queries.ErrUnavailable) || isCondition(res.err):
		// A CONDITION OF THE NODE — a source's question it could not answer
		// here, or a store dataset whose read the state log refused — in a
		// person's words for it where there are any
		// ([builtin.ConditionSentence]) and under its own hint, which is
		// none where no wait clears it: every one of these was "could not
		// read X right now" under an election's four seconds, so a log at
		// its ceiling was polled for ever and a refusal's error, wrapped
		// in nothing, fell to the arm below and was logged as a failure.
		operator.LogCondition(r.Context(), log, "dashboard_dataset_unavailable", res.err,
			logged...)
		said, ok := builtin.ConditionSentence(res.err)
		if !ok {
			said = "this node could not read " + what + " right now"
		}
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			httpjson.RetrySeconds(builtin.RetryAfter(res.err, statelog.ElectionRetryHint)),
			httpjson.Detail{"detail": said})
		return false
	default:
		// A FAILURE NO CONDITION CLAIMS is this node's own fault, which no
		// wait clears: it was a 503 under an election's four seconds, so a
		// board polled a fault for ever.
		log.ErrorContext(r.Context(), "dashboard_dataset_failed", append(logged, "error", res.err.Error())...)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return false
	}
	return true
}

// isCondition reports a condition of the node waiting may clear, which is no
// fault of the board or of the node ([builtin.Condition]).
func isCondition(err error) bool {
	_, ok := builtin.Condition(err)
	return ok
}

// read answers dataset id of rd's board, memoised for the request.
func (s *Service) read(rd *read, id string) result {
	if res, ok := rd.done[id]; ok {
		return res
	}
	d, _ := rd.board.Spec.Dataset(id)
	var res result
	switch d.Source {
	case dashspec.SourceFile:
		res = result{table: rd.board.Files[id], asOf: s.now().UTC()}
	case dashspec.SourceStore:
		res = s.readStore(rd, d)
	case dashspec.SourceEngine:
		res = s.readEngine(rd, d)
	case dashspec.SourceCalc:
		oldest := s.now().UTC()
		t, err := dashspec.Run(d, rd.params, func(in string) (dashspec.Table, error) {
			sub := s.read(rd, in)
			if sub.err == nil && sub.asOf.Before(oldest) {
				oldest = sub.asOf
			}
			return sub.table, sub.err
		})
		res = result{table: t, asOf: oldest, err: err}
	default:
		res = result{err: fmt.Errorf("datasetsapi: %s reads the source %q, which this build does not", id, d.Source)}
	}
	rd.done[id] = res
	return res
}

// charge takes one read from the viewer's bucket and the scope's: the
// artifact's, which every viewer of it shares, or a principal's previews.
func (s *Service) charge(scope string, p iam.Principal) error {
	if refusal := s.budget.Take(scope, principalKey(p), 1); refusal != nil {
		return &overBudget{refusal: refusal}
	}
	return nil
}

// storePage is how many documents one datastore read pages by: the dashboard's
// own collection page.
const storePage = 500

// readStore reads a store dataset's collection under its read rule for the
// admitted reader, every page up to [artifacts.MaxDatasetRows].
func (s *Service) readStore(rd *read, d dashspec.Dataset) result {
	if err := s.charge(rd.a.Head.ID.String(), rd.a.Principal); err != nil {
		return result{err: err}
	}
	shape := rd.board.Shapes[d.ID]
	reading := artifactdataapi.Reading(rd.a, statelog.Freshness{
		Level: statelog.DefaultReadLevel(statelog.SurfaceDashboard)})
	t := dashspec.Table{Columns: shape.Columns, Key: shape.Key, Rows: []dashspec.Row{}}
	at := s.now().UTC()
	cursor := ""
	for {
		page, err := s.data.Docs(rd.ctx, rd.a.Head.ID, reading, d.Store.Collection, 0, cursor, storePage)
		switch {
		case errors.Is(err, artifactdata.ErrNotFound):
			// A COLLECTION NOBODY HAS WRITTEN YET is an empty dataset: an
			// agent fills it after the board is published.
			return result{table: t, asOf: at}
		case err != nil:
			return result{err: err}
		}
		for _, doc := range page.Docs {
			if doc.Deleted {
				continue
			}
			if len(t.Rows) == artifacts.MaxDatasetRows {
				t.Truncated = true
				return result{table: t, asOf: at}
			}
			t.Rows = append(t.Rows, storeRow(doc, shape))
		}
		if page.After == "" {
			return result{table: t, asOf: at}
		}
		cursor = page.After
	}
}

// storeRow is one document as a row of shape: its id, and each declared
// column coerced, a value of another type answered as none.
func storeRow(doc artifactdata.Doc, shape dashspec.Shape) dashspec.Row {
	var fields map[string]any
	if len(doc.Fields) > 0 {
		_ = decode(doc.Fields, &fields) // a stored document is canonical JSON
	}
	row := dashspec.Row{dashspec.StoreIDColumn: doc.ID}
	for _, c := range shape.Columns {
		if c.Key == dashspec.StoreIDColumn {
			continue
		}
		v, err := dashspec.Coerce(c.Type, fields[c.Key])
		if err != nil {
			v = nil
		}
		row[c.Key] = v
	}
	return row
}

// readEngine answers an engine dataset as the viewer, from the cache where
// this principal read it with these arguments within the cache's life.
func (s *Service) readEngine(rd *read, d dashspec.Dataset) result {
	src, _ := dashspec.Source(d.Engine.Name)
	args, err := dashspec.EngineArgs(d, rd.params)
	if err != nil {
		return result{err: err}
	}
	return s.engineRows(rd.ctx, rd.a.Principal, rd.a.Head.ID.String(), d.ID, src, args)
}

// engineRows is src's rows with args, read as viewer: from the cache where
// this principal read it with these arguments within the cache's life, else
// charged to the viewer's and the scope's buckets and asked of the bound
// question. `dataset` names the read in a refusal. The one engine read path,
// a board's dataset's and a builder's preview alike.
func (s *Service) engineRows(ctx context.Context, viewer iam.Principal, scope, dataset string,
	src dashspec.EngineSource, args map[string]string) result {

	key := cacheKey(viewer, src.Name, args)
	now := s.now().UTC()
	if hit, ok := s.cache.get(key, now); ok {
		return hit
	}
	if err := s.charge(scope, viewer); err != nil {
		return result{err: err}
	}
	raw, err := s.ask(ctx, src, args, viewer)
	if err != nil {
		return result{err: sourceFailure(dataset, src, err)}
	}
	t, err := project(src, raw)
	if err != nil {
		return result{err: fmt.Errorf("datasetsapi: %s: %w", src.Name, err)}
	}
	res := result{table: t, asOf: now}
	s.cache.put(key, res, now)
	return res
}

// ask is the bound question's answer, as the REST route encodes it.
func (s *Service) ask(ctx context.Context, src dashspec.EngineSource, args map[string]string,
	viewer iam.Principal) ([]byte, error) {

	b := bindings[src.Name]
	var answer any
	var err error
	if b.question == "" {
		if s.agents == nil {
			return nil, &unanswered{detail: "this node keeps no live roster of the agents"}
		}
		answer, err = s.agents(ctx)
	} else {
		answer, err = s.questions.AnswerWith(ctx, b.question, queries.FromMap(b.params(args, viewer)))
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(answer)
}

// sourceFailure is a source's failure in the answer's vocabulary.
//
// A DATASET IS CLOSED ONLY BY A REFUSAL THAT NAMES ITS DECISION — the
// question's own [queries.Refusal], whose reason and grants are the rule's.
// A bare [queries.ErrUnauthorized] names none (only a Refusal produces it),
// and a question asked by nobody cannot reach here, since every dataset is
// read on a request its artifact's gate admitted: closing the dataset as a
// missing grant for either told the viewer a permission would open what no
// permission decided, so both are the fault they would be.
func sourceFailure(dataset string, src dashspec.EngineSource, err error) error {
	var refusal *queries.Refusal
	switch {
	case errors.As(err, &refusal):
		return &closed{refusal: Refusal{Dataset: dataset, Reason: refusal.Reason, Grants: refusal.Grants},
			detail: "You cannot read " + string(src.Domain) + ": " + src.Access + "."}
	case errors.Is(err, queries.ErrUnknown):
		return &unanswered{detail: "This node does not run " + string(src.Domain) + ", so " +
			src.Name + " has no answer here."}
	case errors.Is(err, queries.ErrBadParams):
		return &unanswered{detail: queries.RefusalDetail(err)}
	}
	return err
}

// project is an answer's rows as src's table: every column coerced to its
// type, at most the source's rows and [artifacts.MaxDatasetRows].
func project(src dashspec.EngineSource, raw []byte) (dashspec.Table, error) {
	rows, more, err := bindings[src.Name].rows(raw)
	if err != nil {
		return dashspec.Table{}, err
	}
	limit := min(src.MaxRows, artifacts.MaxDatasetRows)
	t := dashspec.Table{Columns: src.Columns, Key: src.Key, Truncated: more || len(rows) > limit,
		Rows: make([]dashspec.Row, 0, min(len(rows), limit))}
	for _, in := range rows[:min(len(rows), limit)] {
		row := make(dashspec.Row, len(src.Columns))
		for _, c := range src.Columns {
			v, err := dashspec.Coerce(c.Type, in[c.Key])
			if err != nil {
				return dashspec.Table{}, fmt.Errorf("column %q: %w", c.Key, err)
			}
			row[c.Key] = v
		}
		t.Rows = append(t.Rows, row)
	}
	return t, nil
}

// principalKey is everything about p a bound question can read: its
// identity, its kind, its seat (and so its own record), its effective grants,
// its position in the chart and its stage. Two principals that differ in any
// of them never share a key, so no cached answer reaches a second principal,
// and a grant revoked mid-life is a new key at once.
func principalKey(p iam.Principal) string {
	grants := make([]string, 0, len(p.Grants))
	for _, g := range iam.Expand(p.Grants) {
		grants = append(grants, string(g))
	}
	slices.Sort(grants)
	grants = slices.Compact(grants)
	parts := []string{p.ID.String(), p.Login, string(p.Kind), p.Seat, iam.RecordOwner(p),
		strings.Join(grants, ","), p.Position, string(p.Stage)}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// cacheKey is one principal's read of one source with one set of arguments.
func cacheKey(p iam.Principal, source string, args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString(principalKey(p))
	b.WriteString("\x00" + source)
	for _, k := range keys {
		b.WriteString("\x00" + k + "=" + args[k])
	}
	return b.String()
}

// maxCachedAnswers bounds the answers one node keeps: a board of 32 engine
// datasets open by a hundred people at once, with room for their filters.
// Past it, every expired answer is forgotten, then the oldest.
const maxCachedAnswers = 8192

// answerCache keeps engine answers for [artifacts.DatasetCacheTTL].
type answerCache struct {
	mu      sync.Mutex
	entries map[string]cached
}

type cached struct {
	res     result
	expires time.Time
}

func newAnswerCache() *answerCache { return &answerCache{entries: map[string]cached{}} }

func (c *answerCache) get(key string, now time.Time) (result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !now.Before(e.expires) {
		return result{}, false
	}
	return e.res, true
}

func (c *answerCache) put(key string, res result, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCachedAnswers {
		var oldest string
		var oldestAt time.Time
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
				continue
			}
			if oldest == "" || e.expires.Before(oldestAt) {
				oldest, oldestAt = k, e.expires
			}
		}
		if len(c.entries) >= maxCachedAnswers {
			delete(c.entries, oldest)
		}
	}
	c.entries[key] = cached{res: res, expires: now.Add(artifacts.DatasetCacheTTL)}
}
