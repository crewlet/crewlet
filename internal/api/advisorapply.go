package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN APPLY RE-CHECKS FIRST (ADR-0164): `recheck` runs the finding's class
// again now and answers whether it holds, with the exact request the person
// sends from their own session under the If-Match of the entity it read;
// `applied` is the person's report that it was sent, which the engine checks
// against the configuration before it marks the finding applied. The engine
// never sends the fix itself (ADR-0115).

// RecheckFor is how long a re-check's answer may be sent on: past it, the
// page re-checks again before sending.
const RecheckFor = 10 * time.Minute

// AdvisorConfig is the configuration a re-check builds a fix's request from.
type AdvisorConfig interface {
	EntityAndTag(ctx context.Context, kind, id string) (any, string, error)
	CheckEntity(ctx context.Context, kind, id string, body []byte, create bool, by iam.Actor) (*config.Company, error)
}

// AdvisorWork is the work items a reassignment's re-check reads: each one's
// holder, state and version.
type AdvisorWork interface {
	Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
		fresh statelog.Freshness) (worklet.TaskDetail, error)
}

// AdvisorRecheck is a re-check's answer.
type AdvisorRecheck struct {
	Outcome advisor.RecheckOutcome `json:"outcome"`
	Finding queries.AdvisorFinding `json:"finding"`
	Moved   []advisor.Moved        `json:"moved"`
	// PreviousFix is the fix as it stood, where the re-check rebuilt another.
	PreviousFix *advisor.Fix    `json:"previous_fix"`
	Preview     *RecheckPreview `json:"preview"`
	Send        *RecheckSend    `json:"send"`
	// Sends is every request the option makes, Send first, sent in order:
	// a finding about one cause on many seats changes each of them
	// (ADR-0166). [] where nothing is sent.
	Sends []RecheckSend `json:"sends"`
	// Moves is every item a reassignment moves, asked with `{"action":
	// "reassign"}`: each written by the person's session through the work
	// item's own write (`update_work_item`, at `if_match` its version), the
	// seat it goes to theirs to change. [] where nothing moves.
	Moves []RecheckMove `json:"moves"`
	// Digest binds the answer to the finding's revision and the option it sends.
	Digest    string          `json:"digest"`
	CheckedAt time.Time       `json:"checked_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	Refusal   *RecheckRefusal `json:"refusal"`
}

// RecheckPreview is what the change does, field by field, as the server reads it.
type RecheckPreview struct {
	Rows  []PreviewRow `json:"rows"`
	Notes []string     `json:"notes"`
}

// PreviewRow is one field the change writes, as it is and as it would be.
type PreviewRow struct {
	Field  string  `json:"field"`
	Label  string  `json:"label"`
	Before *string `json:"before"`
	After  *string `json:"after"`
}

// RecheckSend is the one request the person's session sends, its dry run first.
type RecheckSend struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Body       any               `json:"body"`
	Headers    map[string]string `json:"headers"`
	DryRunPath *string           `json:"dry_run_path"`
}

// RecheckMove is one item a reassignment moves: by its key, from the seat
// holding it now to the one the finding suggests, at the version read now,
// with the line its new holder is woken with.
type RecheckMove struct {
	Item    string `json:"item"`
	From    string `json:"from"`
	To      string `json:"to"`
	Version uint64 `json:"version"`
	Reason  string `json:"reason"`
}

// RecheckRefusal is why a change that still holds cannot be sent from here,
// and who can make it: the finding stays open.
type RecheckRefusal struct {
	Reason string `json:"reason"`
	WhoCan string `json:"who_can"`
}

// rechecker is what a re-check reads and writes.
type rechecker struct {
	log     AdvisorLog
	read    duty.Reader
	config  AdvisorConfig
	work    AdvisorWork
	finding func(ctx context.Context, id string) (queries.AdvisorFinding, error)
	now     func() time.Time
}

// errFindingMoved is a finding another writer moved between the read and the
// write: the person presses again.
var errFindingMoved = errors.New("api: the finding changed while it was re-checked; re-check it again")

// recheck runs the finding id's class again now and answers what it found,
// recording what moved as by.
func (rc rechecker) recheck(ctx context.Context, id string, ask recheckAsk, by iam.Actor) (AdvisorRecheck, error) {
	now := rc.now().UTC()
	row, _, err := rc.log.Finding(ctx, id, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		return AdvisorRecheck{}, err
	}
	stored, err := duty.StoredOf([]advisordomain.FindingRow{row})
	if err != nil {
		return AdvisorRecheck{}, err
	}
	s := stored[0]
	if s.State != advisor.StateOpen {
		return AdvisorRecheck{}, notOpenOf(row)
	}
	out := AdvisorRecheck{Moved: []advisor.Moved{}, Sends: []RecheckSend{}, Moves: []RecheckMove{}, CheckedAt: now,
		ExpiresAt: now.Add(RecheckFor)}
	outcome, step, f, err := rc.rederive(ctx, s, now)
	if err != nil {
		return AdvisorRecheck{}, err
	}
	out.Outcome = outcome
	if step != nil {
		if err = duty.RecordFor(ctx, rc.log, rc.log, by, *step, now); err != nil {
			return AdvisorRecheck{}, movedOr(err)
		}
		out.Moved = append(out.Moved, step.Moved...)
		if outcome == advisor.RecheckChanged && advisor.FixDigest(s.Observation.Finding.Fix) != advisor.FixDigest(f.Fix) {
			previous := s.Observation.Finding.Fix
			out.PreviousFix = &previous
		}
	}
	revision := s.Revision
	if outcome == advisor.RecheckChanged {
		revision++
	}
	switch {
	case outcome != advisor.RecheckHolds && outcome != advisor.RecheckChanged:
	case ask.reassign:
		if err = rc.moves(ctx, &out, id, revision, f); err != nil {
			return AdvisorRecheck{}, err
		}
	default:
		if err = rc.build(ctx, &out, id, revision, f, ask.option, by, s, now); err != nil {
			return AdvisorRecheck{}, err
		}
	}
	if out.Finding, err = rc.finding(ctx, id); err != nil {
		return AdvisorRecheck{}, err
	}
	return out, nil
}

// recheckAsk is what a re-check is asked to build: a fix's option, or the
// reassignment beside it.
type recheckAsk struct {
	option   int
	reassign bool
}

// errNothingToMove is a reassignment asked of a finding that offers none.
var errNothingToMove = fmt.Errorf("%w: the finding offers no work to move", advisordomain.ErrInvalid)

// moves fills out's moves for f's reassignment: each item it names that the
// seat it is about still holds and nobody has started, read now, with the
// preview of each and the digest that binds them. An item moved or started
// since is left where it is.
func (rc rechecker) moves(ctx context.Context, out *AdvisorRecheck, id string, revision int, f advisor.Finding) error {
	r := f.Fix.Reassign
	if r == nil || len(r.Items) == 0 || r.To == "" || rc.work == nil {
		return errNothingToMove
	}
	names := map[string]string{f.Subject.Seat: cmp.Or(f.Subject.Name, f.Subject.Seat)}
	for _, seat := range f.Seats {
		names[seat.Seat] = cmp.Or(seat.Name, seat.Seat)
	}
	from, to := f.Subject.Seat, r.To
	reason := fmt.Sprintf("Moved here on the Advisor's finding that %s held several times the open work "+
		"of a seat doing the same job.", cmp.Or(names[from], from))
	out.Preview = &RecheckPreview{Rows: []PreviewRow{}, Notes: []string{}}
	for _, key := range r.Items {
		d, err := rc.work.Task(ctx, key, worklet.DetailWants{}, statelog.Freshness{Level: statelog.ReadSession})
		if errors.Is(err, worklet.ErrNoTask) {
			continue
		}
		if err != nil {
			return err
		}
		t := d.Task
		if t.Assignee != from || t.StatusGroup != worklet.GroupNotStarted {
			continue
		}
		out.Moves = append(out.Moves, RecheckMove{Item: t.Key, From: from, To: to, Version: t.Version, Reason: reason})
		before, after := cmp.Or(names[from], from), cmp.Or(names[to], to)
		out.Preview.Rows = append(out.Preview.Rows, PreviewRow{Field: t.Key + ".assignee",
			Label: t.Key + " " + t.Title, Before: &before, After: &after})
	}
	if len(out.Moves) == 0 {
		out.Preview.Notes = append(out.Preview.Notes, "Every item it would move has moved or started since.")
	}
	out.Digest = advisor.ApplyDigest(id, revision, advisor.ReassignDigest(*r))
	return nil
}

// rederive is what the class says of s now: its outcome, the step that records
// it, and the finding as it now stands. A model finding is re-checked by its
// fix alone: its numbers are read again by the model's next pass.
func (rc rechecker) rederive(ctx context.Context, s advisor.Stored, now time.Time) (advisor.RecheckOutcome,
	*advisor.Transition, advisor.Finding, error) {

	if advisor.GroupOf(s.Observation.Finding) == advisor.GroupModel {
		return advisor.RecheckHolds, nil, s.Observation.Finding, nil
	}
	observed, err := rc.read.Observe(ctx, now)
	if err != nil {
		return "", nil, advisor.Finding{}, err
	}
	reading := advisor.Reading{At: now, Groups: []advisor.Group{advisor.GroupSetup, advisor.GroupEvidence},
		Unread: observed.Unread, Held: observed.Held, Cuts: observed.Partial}
	outcome, step, err := advisor.Recheck(s, observed.Observations, reading)
	if err != nil {
		return "", nil, advisor.Finding{}, err
	}
	f := s.Observation.Finding
	if step != nil && step.Observed != nil {
		f = step.Observed.Finding
	}
	return outcome, step, f, nil
}

// build fills out's preview, request and digest for option of f's fix, or
// closes the finding where its change no longer builds.
func (rc rechecker) build(ctx context.Context, out *AdvisorRecheck, id string, revision int, f advisor.Finding,
	option int, by iam.Actor, s advisor.Stored, now time.Time) error {

	if f.Fix.Kind != advisor.FixConfigChange {
		out.Digest = advisor.ApplyDigest(id, revision, "")
		return nil
	}
	if option < 0 || option >= len(f.Fix.Options) {
		return fmt.Errorf("%w: option %d is not one of the fix's %d", advisordomain.ErrInvalid, option, len(f.Fix.Options))
	}
	out.Preview = &RecheckPreview{Rows: []PreviewRow{}, Notes: []string{}}
	chs := f.Fix.Options[option].Changes()
	for _, ch := range chs {
		entity, tag, err := rc.config.EntityAndTag(ctx, ch.Entity, ch.ID)
		if errors.Is(err, configapi.ErrNoSuchEntity) {
			return rc.retire(ctx, out, s, by, now, advisor.ReasonSubjectGone)
		}
		if err != nil {
			return err
		}
		after, err := advisor.Changed(entity, ch)
		if err != nil {
			return err
		}
		body, err := json.Marshal(after)
		if err != nil {
			return err
		}
		_, err = rc.config.CheckEntity(ctx, ch.Entity, ch.ID, body, false, by)
		var managed *configapi.ManagedError
		switch {
		case errors.As(err, &managed):
			out.Refusal = &RecheckRefusal{Reason: "This company's configuration is managed outside the dashboard, " +
				"so the change is made where it is kept.", WhoCan: "Whoever keeps the company's configuration source."}
			out.Send, out.Sends, out.Preview = nil, []RecheckSend{}, nil
			return nil
		case err != nil:
			return rc.retire(ctx, out, s, by, now, advisor.ReasonFixUnbuildable)
		}
		path := "/config/" + ch.Entity + "/" + ch.ID
		dryRun := path + "?dry_run=true"
		out.Sends = append(out.Sends, RecheckSend{Method: http.MethodPut, Path: path, Body: after,
			Headers: map[string]string{"If-Match": tag, "X-Summary": ch.Summary}, DryRunPath: &dryRun})
		rows, err := previewRows(entity, body)
		if err != nil {
			return err
		}
		// SEVERAL CHANGES SAY WHOSE field each row is.
		for _, row := range rows {
			if len(chs) > 1 {
				row.Field = ch.ID + ": " + row.Field
			}
			out.Preview.Rows = append(out.Preview.Rows, row)
		}
	}
	out.Send = &out.Sends[0]
	out.Digest = advisor.ApplyDigest(id, revision, advisor.OptionDigest(f.Fix.Options[option]))
	return nil
}

// retire closes s as retired for reason, as a re-check found it: nothing is sent.
func (rc rechecker) retire(ctx context.Context, out *AdvisorRecheck, s advisor.Stored, by iam.Actor,
	now time.Time, reason advisor.CloseReason) error {

	t := advisor.Transition{Step: advisor.StepClose, Stored: &s, State: advisor.StateRetired, Reason: reason}
	if err := duty.RecordFor(ctx, rc.log, rc.log, by, t, now); err != nil && !errors.Is(err, advisordomain.ErrIllegalMove) {
		return movedOr(err)
	}
	out.Outcome, out.Send, out.Sends, out.Preview, out.Digest = advisor.RecheckNotApplicable, nil, []RecheckSend{}, nil, ""
	return nil
}

// previewRows is each field the change moves, as entity holds it and as
// after (its JSON) would, labelled and valued in a person's words by the rule
// every change a person approves is shown by ([catalog.ChangeRows]): written
// here by their raw keys and JSON, the rows read as a code diff
// ("health_tools: true").
func previewRows(entity any, after []byte) ([]PreviewRow, error) {
	before, err := json.Marshal(entity)
	if err != nil {
		return nil, err
	}
	changes, err := catalog.ChangeRows(before, after)
	if err != nil {
		return nil, err
	}
	rows := make([]PreviewRow, 0, len(changes))
	for _, c := range changes {
		row := PreviewRow{Field: c.Field, Label: c.Label}
		from, to := c.From.Text(), c.To.Text()
		switch c.Kind {
		case wire.ChangeAdded:
			row.After = &to
		case wire.ChangeRemoved:
			row.Before = &from
		case wire.ChangeFact:
			row.Before, row.After = &to, &to
		default:
			row.Before, row.After = &from, &to
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// movedOr is a stale write as [errFindingMoved], any other error as it is.
func movedOr(err error) error {
	if errors.Is(err, advisordomain.ErrStaleVersion) || errors.Is(err, advisordomain.ErrConflict) {
		return errFindingMoved
	}
	return err
}

// errNotOpen is a finding no longer open when a person re-checks it or
// reports its change sent: another window applied it, or it closed.
var errNotOpen = errors.New("api: the finding is no longer open")

// notOpenError is [errNotOpen] with what became of the finding, which the
// person is told in words rather than as a refused move.
type notOpenError struct{ row advisordomain.FindingRow }

func (e notOpenError) Error() string {
	return fmt.Sprintf("api: the finding is %s, no longer open", e.row.State)
}

func (notOpenError) Is(target error) bool { return target == errNotOpen }

func notOpenOf(row advisordomain.FindingRow) error { return notOpenError{row: row} }

// errRecheckAgain is a report of a send the finding no longer matches, or one
// the configuration does not hold yet: the person re-checks again.
var errRecheckAgain = errors.New("api: re-check the finding again")

// applied marks finding id applied once the configuration holds the option
// digest names, as by reported sending it.
func (rc rechecker) applied(ctx context.Context, id, digest string, by iam.Actor) error {
	now := rc.now().UTC()
	row, _, err := rc.log.Finding(ctx, id, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		return err
	}
	stored, err := duty.StoredOf([]advisordomain.FindingRow{row})
	if err != nil {
		return err
	}
	s := stored[0]
	f := s.Observation.Finding
	switch {
	case s.State != advisor.StateOpen:
		return notOpenOf(row)
	case f.Fix.Reassign != nil && advisor.ApplyDigest(id, s.Revision, advisor.ReassignDigest(*f.Fix.Reassign)) == digest:
		return rc.moved(ctx, row, s, by, now)
	case f.Fix.Kind != advisor.FixConfigChange:
		return errRecheckAgain
	}
	at := slices.IndexFunc(f.Fix.Options, func(o advisor.Option) bool {
		return advisor.ApplyDigest(id, s.Revision, advisor.OptionDigest(o)) == digest
	})
	if at < 0 {
		return errRecheckAgain
	}
	option := f.Fix.Options[at]
	// APPLIED ONLY ONCE EVERY CHANGE HOLDS: one still to send is a send not
	// finished.
	for _, ch := range option.Changes() {
		entity, _, readErr := rc.config.EntityAndTag(ctx, ch.Entity, ch.ID)
		if readErr != nil {
			return readErr
		}
		holds, holdsErr := advisor.Holds(entity, ch)
		if holdsErr != nil || !holds {
			return errors.Join(errRecheckAgain, holdsErr)
		}
	}
	_, err = rc.log.Move(ctx, advisordomain.Call{By: by, OpID: statelog.NewOpID(now, "advisor_applied")}, row.Key,
		advisordomain.Move{Episode: id, Event: advisordomain.EventApplied, State: advisordomain.StateApplied,
			FixDigest: advisor.OptionDigest(option), EffectDueAt: now.Add(advisor.EffectHorizon(f))}, row.KeyVersion)
	return movedOr(err)
}

// moved marks a reassignment applied once the seat it was about holds none of
// the items it named that nobody has started: each moved, or taken up.
func (rc rechecker) moved(ctx context.Context, row advisordomain.FindingRow, s advisor.Stored, by iam.Actor,
	now time.Time) error {
	f := s.Observation.Finding
	if rc.work == nil {
		return errRecheckAgain
	}
	for _, key := range f.Fix.Reassign.Items {
		d, err := rc.work.Task(ctx, key, worklet.DetailWants{}, statelog.Freshness{Level: statelog.ReadSession})
		if errors.Is(err, worklet.ErrNoTask) {
			continue
		}
		if err != nil {
			return err
		}
		if d.Task.Assignee == f.Subject.Seat && d.Task.StatusGroup == worklet.GroupNotStarted {
			return errRecheckAgain
		}
	}
	_, err := rc.log.Move(ctx, advisordomain.Call{By: by, OpID: statelog.NewOpID(now, "advisor_applied")}, row.Key,
		advisordomain.Move{Episode: s.ID, Event: advisordomain.EventApplied, State: advisordomain.StateApplied,
			FixDigest: advisor.ReassignDigest(*f.Fix.Reassign), EffectDueAt: now.Add(advisor.EffectHorizon(f))},
		row.KeyVersion)
	return movedOr(err)
}

// maxAdvisorApplyBytes bounds a re-check's or a report's body.
const maxAdvisorApplyBytes = 1 << 10

func (a *App) rechecker() rechecker {
	return rechecker{log: a.advisor, read: a.advisorDuty.Read, config: a.advisorConfig, work: a.advisorWork,
		finding: a.advisorFinding, now: time.Now}
}

// noRecheck answers on a node that cannot re-check: no advisor log, or no
// configuration to build a fix's request from.
func (a *App) noRecheck(w http.ResponseWriter) bool {
	if a.noAdvisor(w) {
		return true
	}
	if a.advisorDuty != nil && a.advisorConfig != nil {
		return false
	}
	httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable, map[string]string{
		"detail": "this server cannot re-check a finding: it serves no company configuration"})
	return true
}

// serveAdvisorRecheck re-checks one finding before its fix is sent.
func (a *App) serveAdvisorRecheck(w http.ResponseWriter, r *http.Request) {
	if a.noRecheck(w) {
		return
	}
	principal, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	raw, err := httpjson.ReadBody(w, r, maxAdvisorApplyBytes)
	if err != nil {
		return
	}
	var body struct {
		Option int    `json:"option"`
		Action string `json:"action"`
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &body); err != nil || (body.Action != "" && body.Action != "reassign") {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
				"detail": "send {}, the option to re-check, {\"option\": 0}, or the work to move, " +
					"{\"action\": \"reassign\"}"})
			return
		}
	}
	out, err := a.rechecker().recheck(r.Context(), strings.TrimSpace(r.PathValue("id")),
		recheckAsk{option: body.Option, reassign: body.Action == "reassign"}, iam.ActorFor(principal))
	if err != nil {
		refuseApply(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

// serveAdvisorApplied records a person's report that a re-checked change was sent.
func (a *App) serveAdvisorApplied(w http.ResponseWriter, r *http.Request) {
	if a.noRecheck(w) {
		return
	}
	principal, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	raw, err := httpjson.ReadBody(w, r, maxAdvisorApplyBytes)
	if err != nil {
		return
	}
	var body struct {
		Digest string `json:"digest"`
	}
	if err = json.Unmarshal(raw, &body); err != nil || body.Digest == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "send the digest the re-check answered, {\"digest\": \"...\"}"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if err = a.rechecker().applied(r.Context(), id, body.Digest, iam.ActorFor(principal)); err != nil {
		refuseApply(w, r, err)
		return
	}
	finding, err := a.advisorFinding(r.Context(), id)
	if err != nil {
		refuseAdvisorLog(w, r, log, "advisor_finding", err)
		return
	}
	httpjson.Write(w, http.StatusOK, finding)
}

// refuseApply says why a re-check or a report was not taken.
func refuseApply(w http.ResponseWriter, r *http.Request, err error) {
	if notOpen, ok := errors.AsType[notOpenError](err); ok {
		refuseNotOpen(w, notOpen.row)
		return
	}
	switch {
	case errors.Is(err, errRecheckAgain), errors.Is(err, errFindingMoved):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict, httpjson.Detail{
			"outcome": "recheck", "detail": "the finding or its change moved since it was re-checked: re-check it"})
	case errors.Is(err, advisor.ErrUnconcluded):
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable, map[string]string{
			"detail": "what this finding is read from could not be read whole just now, so it cannot be re-checked: " +
				"try again later"})
	case errors.Is(err, advisordomain.ErrNotFound):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "no finding of the Advisor's has this id"})
	default:
		refuseDecision(w, r, err)
	}
}

// refuseNotOpen says what became of a finding that is no longer open: applied,
// with when and until when its effect is checked, or closed, with how.
func refuseNotOpen(w http.ResponseWriter, row advisordomain.FindingRow) {
	fields := httpjson.Detail{"state": string(row.State)}
	if row.State == advisordomain.StateApplied {
		fields["outcome"] = "applied"
		fields["detail"] = "This change was applied already; the Advisor is checking whether it worked."
		if !row.AppliedAt.IsZero() {
			fields["applied_at"] = row.AppliedAt.UTC()
		}
		if !row.EffectDueAt.IsZero() {
			fields["effect_due_at"] = row.EffectDueAt.UTC()
		}
	} else {
		fields["outcome"], fields["detail"] = "closed", closedWords(row.State)
		if !row.ClosedAt.IsZero() {
			fields["closed_at"] = row.ClosedAt.UTC()
		}
	}
	httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict, fields)
}

// closedWords says a closed finding's state to the person who pressed on it:
// a switch, not a table, so the engine allocates nothing for it at init.
func closedWords(state advisordomain.State) string {
	switch state {
	case advisordomain.StateResolved:
		return "This finding is resolved already: what it found no longer holds."
	case advisordomain.StateDismissed:
		return "This finding was dismissed; reopen it to act on it."
	case advisordomain.StateRetired:
		return "This finding was retired: the Advisor no longer checks for it."
	case advisordomain.StateExpired:
		return "This finding expired: it was not seen again for long enough."
	}
	return "This finding is closed already."
}
