package chartapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// RuntimeParam is the query key a caller asks the runtime half with.
//
// OPT-IN, so the stripped posture is what a surface gets by saying nothing.
// The other way round — strip only when asked — is the shape where every
// client that never heard of the parameter is served a company's model
// chains, its mcp_env keys and the NAMES of every credential it holds, which
// is a map of what to attack. A default that leaks is a default nobody
// notices for as long as it works.
const RuntimeParam = "runtime"

// seatView and unitView are what this surface renders.
//
// AN EXPLICIT FIELD LIST rather than the domain's own struct with a tag on
// the half that must not travel. A `json:"-"` is one edit away from being
// removed by somebody who wanted the field in a log line, and the failure is
// silent and total. Here a field reaches a reader because this file names it.
//
// EVERY FIELD A CONTENT WRITE TAKES IS HERE, because a content write is FULL
// POST-STATE ([chart.SeatContent]): the ordinary edit is read, change one
// field, send the object back, and a field the read leaves out is a field the
// write sets to empty. The views carried a seat's goal and not its backstory,
// responsibilities or guidelines, and a unit's purpose and not its knowledge
// references — so a client correcting a goal from this answer erased the rest
// of the seat's prompt, and the round trip /company/export promises dropped
// the four from every file it wrote.
type seatView struct {
	Handle    string `json:"handle"`
	Kind      string `json:"kind,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	Backstory string `json:"backstory,omitempty"`
	Goal      string `json:"goal,omitempty"`

	Responsibilities     []string `json:"responsibilities,omitempty"`
	BehavioralGuidelines []string `json:"behavioral_guidelines,omitempty"`

	Project string `json:"project,omitempty"`
	Space   string `json:"space,omitempty"`

	// FormerHandles are the addresses this seat used to answer to. Public
	// because a reference somebody wrote resolves through them, so a
	// client rendering a stale `manages:` entry can say WHY it still
	// works rather than reporting it broken.
	FormerHandles []string `json:"former_handles,omitempty"`

	// OriginHandle is the handle this seat was CREATED under — its IDENTITY
	// (ADR-0026), which no rename moves and the chart never issues to
	// anything else — served as the row holds it: absent until the first
	// rename, while the seat still answers to it. A client that keeps its own
	// picture of the chart across renames has to be able to say "the same
	// seat" without the address, because an address is not an identity: a
	// retired alias may be claimed by a new seat, and a client that keyed on
	// the address carried an edit of the renamed seat onto the newcomer.
	// Nothing else serves it — the capped `former_handles` drops the origin
	// after enough renames, so it cannot be read off them.
	OriginHandle string `json:"origin_handle,omitempty"`

	// Runtime is the opaque half, present only when the caller asked for
	// it AND carried the grant that reads the company document.
	Runtime any `json:"runtime,omitempty"`
}

type unitView struct {
	Key     string   `json:"key"`
	Name    string   `json:"name,omitempty"`
	Type    string   `json:"type,omitempty"`
	Purpose string   `json:"purpose,omitempty"`
	Goals   []string `json:"goals,omitempty"`
	Parent  string   `json:"parent,omitempty"`
	Lead    string   `json:"lead,omitempty"`
	Channel string   `json:"channel,omitempty"`
	Project string   `json:"project,omitempty"`
	Space   string   `json:"space,omitempty"`

	KnowledgeRefs []string `json:"knowledge_refs,omitempty"`

	FormerKeys []string `json:"former_keys,omitempty"`

	// OriginKey is the key this unit was created under, as [seatView]'s
	// OriginHandle is a seat's: absent until the first rename.
	OriginKey string `json:"origin_key,omitempty"`

	Runtime any `json:"runtime,omitempty"`
}

// getChart answers the whole chart.
func (s *Service) getChart(w http.ResponseWriter, r *http.Request) {
	fresh, ok := s.readParams(w, r)
	if !ok {
		return
	}
	got, err := s.reader.Read(r.Context(), fresh)
	if err != nil {
		s.readFailed(w, err)
		return
	}
	runtime := s.runtimeAllowed(r)
	units := make([]unitView, 0, len(got.Units))
	for _, u := range got.Units {
		units = append(units, viewOfUnit(u, runtime))
	}
	seats := make([]seatView, 0, len(got.Seats))
	for _, seat := range got.Seats {
		seats = append(seats, viewOfSeat(seat, runtime))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"units": units, "seats": seats,
		"manages": got.Manages, "leads": got.Leads,
		// WHAT THIS ANSWER IS, which a client cannot infer: a read
		// reports the position it was served at and whether the level it
		// asked for is the level it got. A surface that dropped this
		// would make a stale answer indistinguishable from a current one.
		"answer":  answerOf(got.Answer),
		"runtime": runtime,
	})
}

// getUnit answers one unit and what it directly holds.
func (s *Service) getUnit(w http.ResponseWriter, r *http.Request) {
	fresh, ok := s.readParams(w, r)
	if !ok {
		return
	}
	got, err := s.reader.Unit(r.Context(), r.PathValue("key"), fresh)
	if errors.Is(err, chart.ErrNotFound) {
		notFound(w, "unit", r.PathValue("key"))
		return
	}
	if err != nil {
		s.readFailed(w, err)
		return
	}
	runtime := s.runtimeAllowed(r)
	children := make([]unitView, 0, len(got.Children))
	for _, u := range got.Children {
		children = append(children, viewOfUnit(u, runtime))
	}
	seats := make([]seatView, 0, len(got.Seats))
	for _, seat := range got.Seats {
		seats = append(seats, viewOfSeat(seat, runtime))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"unit": viewOfUnit(got.Unit, runtime), "children": children,
		"seats": seats, "history": got.History,
		"answer": answerOf(got.Answer), "runtime": runtime,
	})
}

// getSeat answers one seat.
func (s *Service) getSeat(w http.ResponseWriter, r *http.Request) {
	fresh, ok := s.readParams(w, r)
	if !ok {
		return
	}
	got, err := s.reader.Seat(r.Context(), r.PathValue("handle"), fresh)
	if errors.Is(err, chart.ErrNotFound) {
		notFound(w, "seat", r.PathValue("handle"))
		return
	}
	if err != nil {
		s.readFailed(w, err)
		return
	}
	runtime := s.runtimeAllowed(r)
	httpjson.Write(w, http.StatusOK, map[string]any{
		"seat": viewOfSeat(got.Seat, runtime), "manages": got.Manages,
		"history": got.History,
		"answer":  answerOf(got.Answer), "runtime": runtime,
	})
}

// notFound answers a read of one object the chart does not hold.
//
// THE READER SAYS SO WITH AN ERROR, [chart.ErrNotFound], and never with an
// empty object: these routes checked for an empty key instead, which the
// reader never returns, so an address nothing answers to fell through to
// [Service.readFailed] and was answered `500 internal_error` — a screen asking
// about a seat that had been removed was told the node was broken rather than
// that the seat was gone.
func notFound(w http.ResponseWriter, kind, address string) {
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
		map[string]string{"detail": "this company has no " + kind + " " +
			strconv.Quote(address)})
}

// runtimeAllowed reports whether this request gets the opaque half.
//
// BOTH HALVES OF THE QUESTION, and the order matters: a caller who did not
// ask is never told they lack a grant, because they did not want it. A caller
// who asked and may not have it is served the stripped answer rather than
// refused — the rows they asked for are ones they may read, and a 403 over a
// field they will not miss would fail a request that can be answered.
//
// WHICH IS WHY THE ANSWER SAYS `runtime: false`. Without it the two cases are
// one shape: a company whose seats declare no runtime at all renders exactly
// like a caller silently stripped, and a client cannot tell whether to ask
// somebody for a grant.
func (s *Service) runtimeAllowed(r *http.Request) bool {
	if r.URL.Query().Get(RuntimeParam) != "true" {
		return false
	}
	d := s.guard(r, authz.Policy{Action: authz.ActionChartReadRuntime})
	return !d.Unknown() && d.Allowed
}

// freshness resolves what level this read runs at.
//
// THE OPERATOR SURFACE, which is what decides the default: the chart is read
// by somebody managing the company, and an operator reading a structure they
// are about to change from a node that is behind would see a move they
// already made as not having happened. The grammar itself is
// [tracker.ParseFreshness]'s — one grammar for the board, the socket, this
// route and a seat's own tools — and a second reading of these keys here
// would be the one place `read_level` meant something slightly different.
func (s *Service) freshness(r *http.Request) (statelog.Freshness, error) {
	got, err := tracker.ParseFreshness(freshnessParams(r))
	if err != nil {
		return got, err
	}
	got.Level = statelog.LevelFor(statelog.SurfaceOperator, got.Level)
	if got.Level != statelog.ReadStale && got.Bounded() {
		return got, errors.New("a staleness bound belongs to read_level=stale " +
			"and this read resolved to " + string(got.Level))
	}
	return got, nil
}

// readFailed renders a read that could not be served.
//
// A READ THAT COULD NOT REACH THE LEVEL IT ASKED FOR IS NOT A FAULT. The
// framework refuses rather than answering from rows below the position a
// caller named, and a surface that reported that as 500 would have an
// operator chasing a broken engine over a node that is merely catching up.
//
// AND A FLOOR ON ANOTHER LOG IS THE CALLER'S, answered `400 bad_params` like
// every other parameter this surface refuses. Only the read can say it — the
// grammar that parses `min_position` does not know which log a route reads —
// so it is classified here rather than in [Service.readParams]; answered as a
// refusal it told a client to ask another node, which refused it the same.
func (s *Service) readFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, statelog.ErrForeignPosition) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
		return
	}
	if errors.Is(err, statelog.ErrUnavailable) {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, retryAfter(err),
			httpjson.Detail{"detail": err.Error()})
		return
	}
	internalError(w, "api_chart_read_failed", err)
}

// internalError answers a fault: `internal_error`, whose sentence says the
// reason is in this node's log — so the reason is LOGGED here, and never put
// in the body.
//
// A FAULT'S OWN WORDS ARE A STORE'S OR A DRIVER'S — a database path, a
// connection string — and holding a grant on the chart does not make a caller
// somebody they are meant for; the query registry and the socket keep them to
// the log for the same reason. This surface sent them as `detail` and logged
// nothing, so the envelope pointed a reader at a log that did not have them.
func internalError(w http.ResponseWriter, event string, err error) {
	log.Warn(event, "error", err)
	httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
}

// retryAfter is the Retry-After on one of this surface's 503s, in seconds, and
// zero — no header — on one waiting cannot clear.
//
// [statelog.RetryAfter]'s rule, which is every surface's: a refusal that
// derived a hint — a node behind its log, from its backlog over its measured
// drain — says that; one waiting cannot clear says NOTHING, because a node
// holding a record it cannot decode, or evicted from the fleet, answers the
// same however often it is asked; and anything else is
// [authz.RetryUndecidedSeconds], for that constant's reason — the caller waits
// for this node to apply the chart log one batch further, or to read the
// fleet's shared state again. It used to fall back to that constant for every
// refusal without a hint, so a node that would answer nothing until somebody
// upgraded or readmitted it told every client to come back in two seconds.
func retryAfter(err error) int {
	return httpjson.RetrySeconds(statelog.RetryAfter(err,
		authz.RetryUndecidedSeconds*time.Second))
}

// viewOfUnit renders one unit at the posture this request gets.
//
// THE RUNTIME HALF IS MASKED, whoever may read it: every credential in it shows
// only as a whole `${VAR}` reference, and anything else as the mask — the rule
// GET /config applies to the settings document, found by the same tags
// ([chart.MaskRuntime]). The writer seals every literal before a row holds it,
// so what a mask hides here is a composite of references or a row written
// before that rule; what matters is that no read of this surface is where a
// credential leaves, and that the mask it serves is one a write restores.
func viewOfUnit(u chart.Unit, runtime bool) unitView {
	out := unitView{
		Key: u.Key, Name: u.Name, Type: u.Type, Purpose: u.Purpose,
		Goals: u.Goals, Parent: u.ParentKey, Lead: u.Lead,
		Channel: u.Channel, Project: u.Project, Space: u.Space,
		KnowledgeRefs: u.KnowledgeRefs, FormerKeys: u.FormerKeys,
	}
	if origin := u.Origin(); origin != u.Key {
		out.OriginKey = origin
	}
	if runtime && len(u.Runtime) > 0 {
		out.Runtime = maskedRuntime(chart.KindUnit, u.Runtime)
	}
	return out
}

// viewOfSeat renders one seat at the posture this request gets.
//
// ITS ADDRESS IS MASKED LIKE A CREDENTIAL, because the chart seals it like one:
// a sealed address is served as its reference and anything else as the mask,
// which a write hands back and the writer restores. See [viewOfUnit] for the
// runtime half.
func viewOfSeat(seat chart.Seat, runtime bool) seatView {
	out := seatView{
		Handle: seat.Handle, Kind: string(seat.Kind), Unit: seat.UnitKey,
		Name: seat.Name, Email: secrets.Mask(seat.Email), Backstory: seat.Backstory,
		Goal:                 seat.Goal,
		Responsibilities:     seat.Responsibilities,
		BehavioralGuidelines: seat.BehavioralGuidelines,
		Project:              seat.Project, Space: seat.Space,
		FormerHandles: seat.FormerHandles,
	}
	if origin := seat.Origin(); origin != seat.Handle {
		out.OriginHandle = origin
	}
	if runtime && len(seat.Runtime) > 0 {
		out.Runtime = maskedRuntime(chart.KindSeat, seat.Runtime)
	}
	return out
}

// maskedRuntime is one object's runtime half as this surface serves it, or
// nothing — never the half as stored — where it does not decode: a half that
// cannot be walked cannot be said to hold no credential.
func maskedRuntime(kind chart.ObjectKind, runtime json.RawMessage) any {
	masked := chart.MaskRuntime(org.RuntimeShape{}, kind, runtime)
	if len(masked) == 0 {
		return nil
	}
	return masked
}

// answerOf renders what a read reports about itself.
func answerOf(a chart.Answer) map[string]any {
	out := map[string]any{
		"level": string(a.Level), "position": a.Position.String(),
	}
	// LAG IS ABSENT RATHER THAN ZERO when the broker could not say. Zero
	// records behind and "nobody could tell me how far behind" are
	// different facts, and rendering the second as the first shows a
	// caught-up node during exactly the outage in which it is not.
	if a.Lag != nil {
		out["lag"] = *a.Lag
	}
	return out
}

// getUnits and getSeats answer one half of the chart each.
//
// # Why they exist beside GET /chart
//
// Not for the rows — `GET /chart` carries both halves — but for the COST. A
// company with five hundred seats renders an org tree once and then asks
// about people constantly: a seat picker, a mention autocomplete, an
// assignee list. Each of those would otherwise pull every unit, every
// `manages:` edge and every lead with it, on every keystroke's worth of
// staleness.
//
// THEY ARE THE SAME READ under the covers, because the domain's whole-chart
// scope is what makes "is this complete" answerable: a read of half the chart
// that skipped the other half would be a read some deferred record concerns
// and nothing could say so.
func (s *Service) getUnits(w http.ResponseWriter, r *http.Request) {
	fresh, ok := s.readParams(w, r)
	if !ok {
		return
	}
	got, err := s.reader.Read(r.Context(), fresh)
	if err != nil {
		s.readFailed(w, err)
		return
	}
	runtime := s.runtimeAllowed(r)
	units := make([]unitView, 0, len(got.Units))
	for _, u := range got.Units {
		units = append(units, viewOfUnit(u, runtime))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"units": units, "leads": got.Leads,
		"answer": answerOf(got.Answer), "runtime": runtime,
	})
}

func (s *Service) getSeats(w http.ResponseWriter, r *http.Request) {
	fresh, ok := s.readParams(w, r)
	if !ok {
		return
	}
	unheld := r.URL.Query().Get(UnheldParam) == "true"
	// WHO HOLDS NOBODY IS A DIRECTORY READ, asked before anything is read.
	// The filter answers a question about the identity directory — which
	// human seats no person is bound to — and the chart's own read grant
	// says nothing about that estate, so a reader of the board could list
	// every vacancy the directory holds by asking the chart. It takes what
	// the directory's own listing takes: `people:manage`, whoever invites
	// somebody into one of those seats, or `audit:read`.
	if unheld && !s.decide(w, r, authz.Policy{Action: authz.ActionDirectoryRead},
		authz.Object{Kind: authz.KindCompany}) {
		return
	}
	got, err := s.reader.Read(r.Context(), fresh)
	if err != nil {
		s.readFailed(w, err)
		return
	}
	runtime := s.runtimeAllowed(r)
	kind := chart.SeatKind(strings.TrimSpace(r.URL.Query().Get(KindParam)))
	if unheld && s.held == nil {
		// THE FILTER CANNOT BE ANSWERED, so it is REFUSED rather than
		// applied to an empty directory: this surface was stood up with no
		// directory behind it, and filtering against nothing would return
		// EVERY human seat in the company under a parameter that promised
		// the opposite — which is the one failure a screen renders as a
		// finished answer.
		//
		// NO Retry-After, because waiting never changes it: what gives
		// this surface a directory is its wiring, and a header saying
		// "come back" would send the caller back here.
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, 0, httpjson.Detail{
			"detail": "this surface was given no identity directory, so it " +
				"cannot say which seats nobody holds",
		})
		return
	}
	var held map[string]bool
	if unheld {
		if held, err = s.held(r.Context()); err != nil {
			// A DIRECTORY THAT CANNOT BE READ REFUSES THE PAGE, where the
			// report only counts what it could not decide: the report's
			// other findings are still true without this one, while a
			// filtered list is a wrong answer to the only question this
			// parameter asks. The store's own words go to the log, never
			// the body; see [internalError].
			log.WarnContext(r.Context(), "api_chart_holding_unreadable",
				"error", err)
			httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
				retryAfter(err), httpjson.Detail{
					"detail": "this node could not read the identity " +
						"directory, so it cannot say which seats nobody holds",
				})
			return
		}
	}
	seats := make([]seatView, 0, len(got.Seats))
	for _, seat := range got.Seats {
		if kind != "" && seat.Kind != kind {
			continue
		}
		// ASKED BY THE SEAT'S IDENTITY, the handle it was created under,
		// exactly as the report asks: a binding names that (ADR-0027), so
		// asked by the handle it answers to now, a renamed seat somebody
		// holds was listed as one nobody does.
		if unheld && held[seat.Origin()] {
			continue
		}
		seats = append(seats, viewOfSeat(seat, runtime))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"seats": seats, "manages": got.Manages,
		"answer": answerOf(got.Answer), "runtime": runtime,
	})
}

// getHistory answers the company-wide reorganisation feed.
//
// NO RUNTIME POSTURE, because a history entry carries what MOVED as text — a
// summary, the pair of values a field went between — and never a document.
// The opaque half has no delta: it is bytes this domain cannot read, so
// nothing here could describe a change to it even to a caller entitled to
// see one.
func (s *Service) getHistory(w http.ResponseWriter, r *http.Request) {
	fresh, ok := s.readParams(w, r)
	if !ok {
		return
	}
	got, answer, err := s.reader.History(r.Context(), limitOf(r), fresh)
	if err != nil {
		s.readFailed(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"changes": nonNilChanges(got), "answer": answerOf(answer),
	})
}

// nonNilChanges renders an empty feed as `[]` rather than `null`.
//
// A CLIENT READS THE TWO DIFFERENTLY and neither reading is wrong: `null` is
// "no field" and `[]` is "no rows", and a company that has never changed its
// chart is the second. Every list this surface writes is built with make for
// the same reason; this one comes from the domain and needs saying.
func nonNilChanges(in []chart.Change) []chart.Change {
	if in == nil {
		return []chart.Change{}
	}
	return in
}
