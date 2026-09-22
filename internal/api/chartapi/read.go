package chartapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chart"
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
type seatView struct {
	Handle  string `json:"handle"`
	Kind    string `json:"kind,omitempty"`
	Unit    string `json:"unit,omitempty"`
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	Goal    string `json:"goal,omitempty"`
	Project string `json:"project,omitempty"`
	Space   string `json:"space,omitempty"`

	// FormerHandles are the addresses this seat used to answer to. Public
	// because a reference somebody wrote resolves through them, so a
	// client rendering a stale `manages:` entry can say WHY it still
	// works rather than reporting it broken.
	FormerHandles []string `json:"former_handles,omitempty"`

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

	FormerKeys []string `json:"former_keys,omitempty"`

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
	if err != nil {
		s.readFailed(w, err)
		return
	}
	if got.Unit.Key == "" {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": "this company has no unit " +
				strconv.Quote(r.PathValue("key"))})
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
	if err != nil {
		s.readFailed(w, err)
		return
	}
	if got.Seat.Handle == "" {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": "this company has no seat " +
				strconv.Quote(r.PathValue("handle"))})
		return
	}
	runtime := s.runtimeAllowed(r)
	httpjson.Write(w, http.StatusOK, map[string]any{
		"seat": viewOfSeat(got.Seat, runtime), "manages": got.Manages,
		"history": got.History,
		"answer":  answerOf(got.Answer), "runtime": runtime,
	})
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
func (s *Service) readFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, statelog.ErrUnavailable) {
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable,
			map[string]string{"detail": err.Error()})
		return
	}
	httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeInternalError,
		map[string]string{"detail": err.Error()})
}

// viewOfUnit renders one unit at the posture this request gets.
func viewOfUnit(u chart.Unit, runtime bool) unitView {
	out := unitView{
		Key: u.Key, Name: u.Name, Type: string(u.Type), Purpose: u.Purpose,
		Goals: u.Goals, Parent: u.ParentKey, Lead: u.Lead,
		Channel: u.Channel, Project: u.Project, Space: u.Space,
		FormerKeys: u.FormerKeys,
	}
	if runtime && len(u.Runtime) > 0 {
		out.Runtime = u.Runtime
	}
	return out
}

// viewOfSeat renders one seat at the posture this request gets.
func viewOfSeat(seat chart.Seat, runtime bool) seatView {
	out := seatView{
		Handle: seat.Handle, Kind: string(seat.Kind), Unit: seat.UnitKey,
		Name: seat.Name, Email: seat.Email, Goal: seat.Goal,
		Project: seat.Project, Space: seat.Space,
		FormerHandles: seat.FormerHandles,
	}
	if runtime && len(seat.Runtime) > 0 {
		out.Runtime = seat.Runtime
	}
	return out
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
	got, err := s.reader.Read(r.Context(), fresh)
	if err != nil {
		s.readFailed(w, err)
		return
	}
	runtime := s.runtimeAllowed(r)
	seats := make([]seatView, 0, len(got.Seats))
	for _, seat := range got.Seats {
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
