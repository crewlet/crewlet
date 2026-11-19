package api

import (
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
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
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

// AdvisorRecheck is a re-check's answer.
type AdvisorRecheck struct {
	Outcome advisor.RecheckOutcome `json:"outcome"`
	Finding queries.AdvisorFinding `json:"finding"`
	Moved   []advisor.Moved        `json:"moved"`
	// PreviousFix is the fix as it stood, where the re-check rebuilt another.
	PreviousFix *advisor.Fix    `json:"previous_fix"`
	Preview     *RecheckPreview `json:"preview"`
	Send        *RecheckSend    `json:"send"`
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
	finding func(ctx context.Context, id string) (queries.AdvisorFinding, error)
	now     func() time.Time
}

// errFindingMoved is a finding another writer moved between the read and the
// write: the person presses again.
var errFindingMoved = errors.New("api: the finding changed while it was re-checked; re-check it again")

// recheck runs the finding id's class again now and answers what it found,
// recording what moved as by.
func (rc rechecker) recheck(ctx context.Context, id string, option int, by iam.Actor) (AdvisorRecheck, error) {
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
		return AdvisorRecheck{}, fmt.Errorf("%w: a finding that is %s is not applied", advisordomain.ErrIllegalMove, s.State)
	}
	out := AdvisorRecheck{Moved: []advisor.Moved{}, CheckedAt: now, ExpiresAt: now.Add(RecheckFor)}
	outcome, step, f, err := rc.rederive(ctx, s, now)
	if err != nil {
		return AdvisorRecheck{}, err
	}
	out.Outcome = outcome
	if step != nil {
		if err := duty.RecordFor(ctx, rc.log, rc.log, by, *step, now); err != nil {
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
	if outcome == advisor.RecheckHolds || outcome == advisor.RecheckChanged {
		if err := rc.build(ctx, &out, id, revision, f, option, by, s, now); err != nil {
			return AdvisorRecheck{}, err
		}
	}
	if out.Finding, err = rc.finding(ctx, id); err != nil {
		return AdvisorRecheck{}, err
	}
	return out, nil
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
		Unread: observed.Unread, Held: observed.Held}
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
	ch := f.Fix.Options[option].Change
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
		return nil
	case err != nil:
		return rc.retire(ctx, out, s, by, now, advisor.ReasonFixUnbuildable)
	}
	path := "/config/" + ch.Entity + "/" + ch.ID
	dryRun := path + "?dry_run=true"
	out.Send = &RecheckSend{Method: http.MethodPut, Path: path, Body: after,
		Headers: map[string]string{"If-Match": tag, "X-Summary": ch.Summary}, DryRunPath: &dryRun}
	out.Preview = &RecheckPreview{Rows: previewRows(entity, after, ch), Notes: []string{}}
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
	out.Outcome, out.Send, out.Preview, out.Digest = advisor.RecheckNotApplicable, nil, nil, ""
	return nil
}

// previewRows is each field ch writes, as entity holds it and as after would.
func previewRows(entity any, after map[string]any, ch advisor.Change) []PreviewRow {
	before := map[string]any{}
	if raw, err := json.Marshal(entity); err == nil {
		_ = json.Unmarshal(raw, &before)
	}
	var rows []PreviewRow
	add := func(field string, was, now any) {
		rows = append(rows, PreviewRow{Field: field, Label: field, Before: rendered(was), After: rendered(now)})
	}
	for _, k := range sortedKeys(ch.Set) {
		add(k, before[k], after[k])
	}
	for _, k := range sortedKeys(ch.Merge) {
		nested, _ := ch.Merge[k].(map[string]any)
		was, _ := before[k].(map[string]any)
		now, _ := after[k].(map[string]any)
		for _, n := range sortedKeys(nested) {
			add(k+"."+n, was[n], now[n])
		}
	}
	if rows == nil {
		rows = []PreviewRow{}
	}
	return rows
}

// rendered is v as a row shows it, nil for a field the entity does not hold.
func rendered(v any) *string {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	s := string(raw)
	return &s
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// movedOr is a stale write as [errFindingMoved], any other error as it is.
func movedOr(err error) error {
	if errors.Is(err, advisordomain.ErrStaleVersion) || errors.Is(err, advisordomain.ErrConflict) {
		return errFindingMoved
	}
	return err
}

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
	if s.State != advisor.StateOpen || f.Fix.Kind != advisor.FixConfigChange {
		return errRecheckAgain
	}
	at := slices.IndexFunc(f.Fix.Options, func(o advisor.Option) bool {
		return advisor.ApplyDigest(id, s.Revision, advisor.OptionDigest(o)) == digest
	})
	if at < 0 {
		return errRecheckAgain
	}
	option := f.Fix.Options[at]
	entity, _, err := rc.config.EntityAndTag(ctx, option.Change.Entity, option.Change.ID)
	if err != nil {
		return err
	}
	if holds, err := advisor.Holds(entity, option.Change); err != nil || !holds {
		return errors.Join(errRecheckAgain, err)
	}
	_, err = rc.log.Move(ctx, advisordomain.Call{By: by, OpID: statelog.NewOpID(now, "advisor_applied")}, row.Key,
		advisordomain.Move{Episode: id, Event: advisordomain.EventApplied, State: advisordomain.StateApplied,
			FixDigest: advisor.OptionDigest(option), EffectDueAt: now.Add(advisor.EffectHorizon(f))}, row.KeyVersion)
	return movedOr(err)
}

// maxAdvisorApplyBytes bounds a re-check's or a report's body.
const maxAdvisorApplyBytes = 1 << 10

func (a *App) rechecker() rechecker {
	return rechecker{log: a.advisor, read: a.advisorDuty.Read, config: a.advisorConfig, finding: a.advisorFinding,
		now: time.Now}
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
		Option int `json:"option"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
				"detail": "send {} or the option to re-check, {\"option\": 0}"})
			return
		}
	}
	out, err := a.rechecker().recheck(r.Context(), strings.TrimSpace(r.PathValue("id")), body.Option,
		iam.ActorFor(principal))
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
	if err := json.Unmarshal(raw, &body); err != nil || body.Digest == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "send the digest the re-check answered, {\"digest\": \"...\"}"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if err := a.rechecker().applied(r.Context(), id, body.Digest, iam.ActorFor(principal)); err != nil {
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
	switch {
	case errors.Is(err, errRecheckAgain), errors.Is(err, errFindingMoved):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict, httpjson.Detail{
			"outcome": "recheck", "detail": "the finding or its change moved since it was re-checked: re-check it"})
	case errors.Is(err, advisor.ErrUnconcluded):
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable, map[string]string{
			"detail": "what this finding is read from could not be read just now, so it cannot be re-checked: " +
				"try again in a minute"})
	case errors.Is(err, advisordomain.ErrNotFound):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "no finding of the Advisor's has this id"})
	default:
		refuseDecision(w, r, err)
	}
}
