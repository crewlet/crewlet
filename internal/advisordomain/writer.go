package advisordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// The advisor's write path: one record per decision, each decided inside one
// snapshot of this node's rows. Every value is judged here before it is
// published, never by the applier (package doc, "The applier judges no text").

// Call is one write's caller: who is writing and under which operation.
type Call struct {
	// By is the author as iam.ActorFor records a principal: the engine's
	// duty, or the person whose decision it is.
	By iam.Actor

	// OpID is the operation the record is published under.
	OpID string
}

// validate refuses a call the records could not name, and an agent's: an
// agent reaches the advisor only through a person who approves.
func (c Call) validate() error {
	switch {
	case strings.TrimSpace(c.By.Name) == "":
		return invalid("the call names no actor: every record names who wrote it")
	case !c.By.Kind.Valid():
		return invalid("actor kind %q is not one of %s", c.By.Kind, words.Or(iam.ActorKinds))
	case c.By.Kind == iam.ActorAgent:
		return invalid("an agent writes nothing to the advisor: a person approves what it proposes")
	}
	if err := statelog.CheckCallerOpID(c.OpID); err != nil {
		return fmt.Errorf("%w: the operation id %w", ErrInvalid, err)
	}
	return nil
}

// NoIfMatch is a write conditioned on no version of its key.
const NoIfMatch uint64 = 0

// IfNoRecord is a write conditioned on its key holding no record yet, so a
// write somebody made since refuses it as [ErrStaleVersion] rather than landing
// beside it unseen. No version is this value: a version is a log position, far
// below it.
const IfNoRecord uint64 = 1<<64 - 1

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	// Publisher is the domain's write authority, and DB the replicated estate
	// its decisions read.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// Now is the clock a record's authored instant and a hold's lapse are
	// read off; nil is the wall clock in UTC. No row stores it.
	Now func() time.Time
}

// Writer is the advisor's write path.
type Writer struct {
	publisher *statelog.Publisher
	db        store.ReplicatedReader
	now       func() time.Time
}

// NewWriter builds the write path.
func NewWriter(deps WriterDeps) (*Writer, error) {
	switch {
	case deps.Publisher == nil:
		return nil, errors.New("advisordomain: a write authority is required: every write is " +
			"a record on the advisor log")
	case deps.DB.IsZero():
		return nil, errors.New("advisordomain: the replicated estate is required: a write " +
			"decides from this node's own applied rows")
	}
	w := &Writer{publisher: deps.Publisher, db: deps.DB, now: deps.Now}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w, nil
}

// errNothing is a decision that there is nothing to record: the caller is told
// the write succeeded and no record is published.
var errNothing = errors.New("advisordomain: nothing to record")

// decision encodes one record inside the snapshot's own transaction.
func decision(stamp statelog.Stamp, by iam.Actor, subject Subject, op OpKind,
	scope ScopeSet, opID string, payload any, at time.Time, version int64) (statelog.Decision, error) {

	if err := subject.writable(); err != nil {
		return statelog.Decision{}, err
	}
	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("advisordomain: encode the %s payload for %s: %w",
			op, subject, err)
	}
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, CreatedAt: at,
			Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
		},
		Mutation: body, Actor: by.Name, ActorKind: by.Kind, OperatorID: by.OperatorID,
	})
	if err != nil {
		return statelog.Decision{}, err
	}
	return statelog.Decision{Payload: encoded, Version: version}, nil
}

// publish sends one request, naming a lost compare-and-set and a create that
// found its record already there in this domain's words.
func publish(ctx context.Context, publisher *statelog.Publisher,
	req statelog.Request) (statelog.Result, error) {

	res, err := publisher.Publish(ctx, req)
	switch {
	case errors.Is(err, statelog.ErrConflict):
		return res, fmt.Errorf("%w: %s: %w", ErrConflict, req.Subject, err)
	case errors.Is(err, statelog.ErrExists):
		return res, fmt.Errorf("%w: %s: %w", ErrExists, req.Subject, err)
	}
	return res, err
}

// write publishes one record of op on subject, decided by check in the
// snapshot: check answers the version the decision read, or [errNothing] for
// a write with nothing to record.
func (w *Writer) write(ctx context.Context, c Call, subject Subject, scope ScopeSet,
	pattern statelog.Pattern, op OpKind, payload any,
	check func(tx *sql.Tx) (int64, error)) (statelog.Result, error) {

	return publish(ctx, w.publisher, statelog.Request{
		Subject: subject.framework(), Scope: scope.Resolve(subject), OpID: c.OpID, Pattern: pattern,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			version, err := check(tx)
			switch {
			case errors.Is(err, errNothing):
				return statelog.Decision{Version: version}, nil
			case err != nil:
				return statelog.Decision{}, err
			}
			return decision(stamp, c.By, subject, op, scope, c.OpID, payload, w.now(), version)
		},
	})
}

// ---- a finding ---------------------------------------------------------- //

// finding publishes one record of op on key's subject, decided by check from
// the key's row once ifMatch holds.
func (w *Writer) finding(ctx context.Context, c Call, key string, op OpKind, payload any,
	ifMatch uint64, check func(tx *sql.Tx, k keyState) error) (statelog.Result, error) {

	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := validKey(key); err != nil {
		return statelog.Result{}, err
	}
	subject := FindingSubject(key)
	return w.write(ctx, c, subject, ScopeSet{Subject: true}, statelog.PatternArbitrated, op, payload,
		func(tx *sql.Tx) (int64, error) {
			k, err := loadKey(ctx, tx, subject.ID)
			if err != nil {
				return 0, err
			}
			switch {
			case ifMatch == IfNoRecord && k.found:
				return 0, fmt.Errorf("%w: %s is at version %d and the write was decided where it "+
					"held no record; read it again and decide", ErrStaleVersion, key, k.version)
			case ifMatch != NoIfMatch && ifMatch != IfNoRecord && uint64(k.version) != ifMatch:
				return 0, fmt.Errorf("%w: %s is at version %d and the write was decided from %d; "+
					"read it again and decide", ErrStaleVersion, key, k.version, ifMatch)
			}
			return k.version, check(tx, k)
		})
}

// episodeOf is the episode a write on key names, refused where the key holds
// no such episode or where it is closed.
func episodeOf(ctx context.Context, tx *sql.Tx, key string, k keyState, id string) (episodeState, error) {
	if !k.found {
		return episodeState{}, fmt.Errorf("%w: the log holds no finding %s", ErrNotFound, key)
	}
	e, err := loadEpisode(ctx, tx, id)
	if err != nil {
		return episodeState{}, err
	}
	if !e.found || e.keyID != KeyID(key) {
		return episodeState{}, fmt.Errorf("%w: %s holds no episode %s", ErrNotFound, key, id)
	}
	if e.state.Closed() {
		return episodeState{}, fmt.Errorf("%w: episode %s of %s is %s, and closed is terminal",
			ErrIllegalMove, id, key, e.state)
	}
	return e, nil
}

// Open writes a new episode of o.Key. ifMatch is the key's version the duty
// decided from ([KeyRow.Version]), [IfNoRecord] for a key never recorded, or
// [NoIfMatch]. A key with a non-closed episode answers [ErrExists], and one
// whose hold keeps o.Severity quiet [ErrHeld].
func (w *Writer) Open(ctx context.Context, c Call, o Opening, ifMatch uint64) (statelog.Result, error) {
	if err := o.validate(); err != nil {
		return statelog.Result{}, err
	}
	return w.finding(ctx, c, o.Key, OpOpen, o, ifMatch, func(tx *sql.Tx, k keyState) error {
		switch {
		case k.current != "":
			return fmt.Errorf("%w: %s has episode %s open: revise, refresh or move it", ErrExists,
				o.Key, k.current)
		case k.holds(o.Severity, w.now()):
			return fmt.Errorf("%w: %s is held at %s, and a %s episode is no graver; a person's "+
				"release lifts the hold", ErrHeld, o.Key, k.holdSeverity, o.Severity)
		}
		e, err := loadEpisode(ctx, tx, o.Episode)
		if err != nil {
			return err
		}
		if e.found {
			return fmt.Errorf("%w: episode %s was opened already", ErrExists, o.Episode)
		}
		return nil
	})
}

// Revise writes a moved verdict or fix on the key's non-closed episode: its
// revision is the stored one plus one, or [ErrStaleVersion].
func (w *Writer) Revise(ctx context.Context, c Call, key string, r Revision, ifMatch uint64) (statelog.Result, error) {
	if err := r.validate(); err != nil {
		return statelog.Result{}, err
	}
	return w.finding(ctx, c, key, OpRevise, r, ifMatch, func(tx *sql.Tx, k keyState) error {
		e, err := episodeOf(ctx, tx, key, k, r.Episode)
		switch {
		case err != nil:
			return err
		case r.Revision != e.revision+1:
			return fmt.Errorf("%w: episode %s is at revision %d, and a revision is the stored one "+
				"plus one", ErrStaleVersion, r.Episode, e.revision)
		case r.VerdictDigest == e.verdictDigest && r.FixDigest == e.fixDigest:
			return invalid("revision %d of episode %s moves neither its verdict nor its fix: a "+
				"refresh records facts under the same verdict and fix", r.Revision, r.Episode)
		}
		return nil
	})
}

// Refresh writes facts that moved under the key's same verdict and fix. A
// stored revision newer than r.Revision makes it write nothing.
func (w *Writer) Refresh(ctx context.Context, c Call, key string, r Refresh, ifMatch uint64) (statelog.Result, error) {
	if err := r.validate(); err != nil {
		return statelog.Result{}, err
	}
	return w.finding(ctx, c, key, OpRefresh, r, ifMatch, func(tx *sql.Tx, k keyState) error {
		e, err := episodeOf(ctx, tx, key, k, r.Episode)
		switch {
		case err != nil:
			return err
		case e.revision > r.Revision:
			return errNothing
		case e.revision < r.Revision:
			return invalid("the refresh names revision %d of episode %s, which is at %d",
				r.Revision, r.Episode, e.revision)
		}
		return nil
	})
}

// Move writes one state transition of the key's episode, legal as the
// package doc's "Legal moves" says, or [ErrIllegalMove].
func (w *Writer) Move(ctx context.Context, c Call, key string, m Move, ifMatch uint64) (statelog.Result, error) {
	if err := m.validate(); err != nil {
		return statelog.Result{}, err
	}
	return w.finding(ctx, c, key, OpMove, m, ifMatch, func(tx *sql.Tx, k keyState) error {
		e, err := episodeOf(ctx, tx, key, k, m.Episode)
		switch {
		case err != nil:
			return err
		case !legalFrom(m.Event, e.state):
			return fmt.Errorf("%w: episode %s of %s is %s, and %s does not leave %s",
				ErrIllegalMove, m.Episode, key, e.state, m.Event, e.state)
		case m.Event == EventIneffective && m.FixDigest != e.appliedFix:
			return invalid("an ineffective move names the fix that was applied, %s", e.appliedFix)
		}
		return nil
	})
}

// Explain writes the model's words for the episode's verdict. It is bound to
// the verdict, not to the key's version: a verdict that moved since answers
// [ErrStaleVersion].
func (w *Writer) Explain(ctx context.Context, c Call, key string, e Explanation) (statelog.Result, error) {
	if err := e.validate(); err != nil {
		return statelog.Result{}, err
	}
	return w.finding(ctx, c, key, OpExplain, e, NoIfMatch, func(tx *sql.Tx, k keyState) error {
		stored, err := episodeOf(ctx, tx, key, k, e.Episode)
		switch {
		case err != nil:
			return err
		case stored.verdictDigest != e.VerdictDigest:
			return fmt.Errorf("%w: episode %s's verdict moved since it was explained; explain "+
				"the current one", ErrStaleVersion, e.Episode)
		}
		return nil
	})
}

// Release lifts the key's hold: a person's reopen. A key with no hold answers
// [ErrIllegalMove].
func (w *Writer) Release(ctx context.Context, c Call, key string, ifMatch uint64) (statelog.Result, error) {
	return w.finding(ctx, c, key, OpRelease, Release{}, ifMatch, func(_ *sql.Tx, k keyState) error {
		switch {
		case !k.found:
			return fmt.Errorf("%w: the log holds no finding %s", ErrNotFound, key)
		case !k.hold.Valid:
			return fmt.Errorf("%w: %s holds no hold to release", ErrIllegalMove, key)
		}
		return nil
	})
}

// ---- runs, reports and alarms ------------------------------------------- //

// RequestRun writes a person's press of Run now. A run already requested or
// recorded answers [ErrExists].
func (w *Writer) RequestRun(ctx context.Context, c Call, r RunRequest) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := r.validate(); err != nil {
		return statelog.Result{}, err
	}
	return w.write(ctx, c, RunSubject(r.Run), ScopeSet{Subject: true}, statelog.PatternArbitrated,
		OpRequest, r, func(tx *sql.Tx) (int64, error) {
			if _, found, err := runStatus(ctx, tx, r.Run); err != nil || found {
				return 0, existsOr(err, "run %s was requested already", r.Run)
			}
			return 0, nil
		})
}

// RecordRun writes a run's end, once: a run recorded already answers
// [ErrExists].
func (w *Writer) RecordRun(ctx context.Context, c Call, r RunRecord) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if _, err := r.validate(); err != nil {
		return statelog.Result{}, err
	}
	return w.write(ctx, c, RunSubject(r.Run), ScopeSet{Subject: true}, statelog.PatternArbitrated,
		OpRecord, r, func(tx *sql.Tx) (int64, error) {
			status, found, err := runStatus(ctx, tx, r.Run)
			if err != nil || (found && status != "") {
				return 0, existsOr(err, "run %s was recorded already", r.Run)
			}
			return 0, nil
		})
}

// runStatus is a run's status, empty while it is only requested, and whether
// the log holds it at all.
func runStatus(ctx context.Context, tx *sql.Tx, run string) (string, bool, error) {
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM advisor_runs WHERE run_id = ?`, run).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("advisordomain: read run %s: %w", run, err)
	}
	return status, true, nil
}

// existsOr is err where the rows could not be read, and otherwise [ErrExists]
// saying what.
func existsOr(err error, format string, args ...any) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrExists, fmt.Sprintf(format, args...))
}

// WriteReport writes one week's report, once: a week written already answers
// [ErrExists].
func (w *Writer) WriteReport(ctx context.Context, c Call, r Report) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := r.validate(); err != nil {
		return statelog.Result{}, err
	}
	return w.write(ctx, c, ReportSubject(r.Week), ScopeSet{Subject: true}, statelog.PatternCreate,
		OpWrite, r, func(tx *sql.Tx) (int64, error) {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM advisor_reports WHERE week = ?`,
				r.Week).Scan(&n); err != nil || n > 0 {
				return 0, existsOr(err, "the report of %s was written already", r.Week)
			}
			return 0, nil
		})
}

// RaiseAlarm opens an episode of the alarm's series, and writes nothing while
// one is open.
func (w *Writer) RaiseAlarm(ctx context.Context, c Call, a AlarmRaise) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := a.validate(); err != nil {
		return statelog.Result{}, err
	}
	subject := AlarmSubject(a.Node, a.Kind, a.Instance)
	return w.write(ctx, c, subject, ScopeSet{Subject: true}, statelog.PatternArbitrated, OpRaise, a,
		func(tx *sql.Tx) (int64, error) {
			open, err := openEpisode(ctx, tx, subject.ID)
			if err != nil || open {
				return 0, nothingOr(err)
			}
			return 0, nil
		})
}

// ClearAlarm closes the open episode of the series node, kind and instance
// name, and writes nothing where none is open.
func (w *Writer) ClearAlarm(ctx context.Context, c Call, node, kind, instance string,
	a AlarmClear) (statelog.Result, error) {

	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := series(node, kind, instance); err != nil {
		return statelog.Result{}, err
	}
	if a.ClearedAt.IsZero() {
		return statelog.Result{}, invalid("an alarm's clear names when it cleared (cleared_at)")
	}
	subject := AlarmSubject(node, kind, instance)
	return w.write(ctx, c, subject, ScopeSet{Subject: true}, statelog.PatternArbitrated, OpClear, a,
		func(tx *sql.Tx) (int64, error) {
			open, err := openEpisode(ctx, tx, subject.ID)
			if err != nil || !open {
				return 0, nothingOr(err)
			}
			return 0, nil
		})
}

// openEpisode reports whether the series holds an open episode.
func openEpisode(ctx context.Context, tx *sql.Tx, series string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM advisor_alarm_episodes INDEXED BY advisor_alarm_episodes_open_idx
		WHERE series_id = ? AND cleared_position IS NULL`, series).Scan(&n); err != nil {
		return false, fmt.Errorf("advisordomain: read alarm series %s: %w", series, err)
	}
	return n > 0, nil
}

// nothingOr is err where the rows could not be read, and otherwise
// [errNothing].
func nothingOr(err error) error {
	if err != nil {
		return err
	}
	return errNothing
}

// ---- retention ---------------------------------------------------------- //

// Expire removes what is past each retention, once a day: day is the UTC day
// it is recorded for, and a day recorded already answers [ErrExists]. No
// cutoff may reach inside its retention ([ExpiryFor] gives each).
func (w *Writer) Expire(ctx context.Context, c Call, day time.Time, e Expiry) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := e.validate(day); err != nil {
		return statelog.Result{}, err
	}
	return w.write(ctx, c, RetentionSubject(day), domainScope(), statelog.PatternCreate, OpExpire, e,
		func(tx *sql.Tx) (int64, error) {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM advisor_expiries WHERE day = ?`,
				Day(day)).Scan(&n); err != nil || n > 0 {
				return 0, existsOr(err, "the expiry of %s was recorded already", Day(day))
			}
			return 0, nil
		})
}

// ---- what a writer refuses ---------------------------------------------- //

// validKey refuses a key the log cannot name: a class, `:` and a subject key,
// in at most [MaxKeyBytes] of printable text.
func validKey(key string) error {
	class, subject, ok := strings.Cut(key, ":")
	switch {
	case !ok || class == "" || subject == "":
		return invalid("a finding key is its class, `:` and its subject key, and %q is not", key)
	case len(key) > MaxKeyBytes:
		return invalid("a finding key is at most %d bytes", MaxKeyBytes)
	}
	return printable("key", key, MaxKeyBytes)
}

// subjectID refuses a subject id the log cannot keep. Empty is a subject that
// is the only one of its kind (the company, its atlas), which its kind alone
// names; any other is printable.
func subjectID(id string) error {
	if id == "" {
		return nil
	}
	return printable("subject_id", id, MaxSubjectIDBytes)
}

// printable refuses text that is empty, longer than max bytes, not UTF-8, or
// that carries a control character or surrounding space.
func printable(field, s string, max int) error {
	switch {
	case s == "" || strings.TrimSpace(s) != s:
		return invalid("%s is set, with no space around it", field)
	case len(s) > max:
		return invalid("%s is at most %d bytes", field, max)
	case !utf8.ValidString(s):
		return invalid("%s is valid UTF-8", field)
	case strings.ContainsFunc(s, unicode.IsControl):
		return invalid("%s carries no control character", field)
	}
	return nil
}

// isDigest reports a lowercase hex SHA-256.
func isDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// digests refuses a verdict or fix digest not spelled as a SHA-256.
func digests(verdict, fix string) error {
	switch {
	case !isDigest(verdict):
		return invalid("verdict_digest is a lowercase hex SHA-256 of the verdict's canonical JSON")
	case !isDigest(fix):
		return invalid("fix_digest is a lowercase hex SHA-256 of the fix's request")
	}
	return nil
}

// impacts refuses a negative impact.
func impacts(minutes, tokens *int64) error {
	if (minutes != nil && *minutes < 0) || (tokens != nil && *tokens < 0) {
		return invalid("an impact is a count per week, never below zero")
	}
	return nil
}

// jsonOf refuses a body that is not JSON of the kind open names (`{` or `[`),
// or is longer than max bytes; optional allows none at all.
func jsonOf(field string, raw json.RawMessage, open byte, max int, optional bool) error {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "" && optional:
		return nil
	case len(raw) > max:
		return invalid("%s is at most %d bytes", field, max)
	case trimmed == "" || trimmed[0] != open || !json.Valid(raw):
		kind := "an object"
		if open == '[' {
			kind = "an array"
		}
		return invalid("%s is JSON, %s", field, kind)
	}
	return nil
}

// episode refuses an episode id not spelled as a uuid.
func episode(id string) error {
	if !IsUUID(id) {
		return invalid("episode is a uuid in its canonical lowercase form, and %q is not", id)
	}
	return nil
}

// validate refuses an opening the log cannot keep.
func (o Opening) validate() error {
	if err := episode(o.Episode); err != nil {
		return err
	}
	if err := validKey(o.Key); err != nil {
		return err
	}
	if err := printable("class", o.Class, MaxClassBytes); err != nil {
		return err
	}
	switch {
	case !strings.HasPrefix(o.Key, o.Class+":"):
		return invalid("key %q is not of class %q: a key is its class, `:` and its subject key", o.Key, o.Class)
	case !o.Source.Valid():
		return invalid("source %q is not one of %s", o.Source, words.Or(Sources))
	case !o.Actor.Valid():
		return invalid("actor %q is not one of %s", o.Actor, words.Or(Actors))
	case !o.Severity.Valid():
		return invalid("severity %q is not one of %s", o.Severity, words.Or(Severities))
	case o.Revision != 1:
		return invalid("an episode opens at revision 1, and this one names %d", o.Revision)
	}
	for _, check := range []error{
		printable("area", o.Area, MaxWordBytes),
		printable("subject_kind", o.SubjectKind, MaxWordBytes),
		subjectID(o.SubjectID),
		digests(o.VerdictDigest, o.FixDigest),
		impacts(o.ImpactMinutes, o.ImpactTokens),
		jsonOf("body", o.Body, '{', MaxBodyBytes, false),
	} {
		if check != nil {
			return check
		}
	}
	return nil
}

// validate refuses a revision the log cannot keep.
func (r Revision) validate() error {
	switch {
	case r.Revision < 2:
		return invalid("a revision follows the episode's opening, so it is 2 or more, and this one is %d", r.Revision)
	case !r.Severity.Valid():
		return invalid("severity %q is not one of %s", r.Severity, words.Or(Severities))
	}
	for _, check := range []error{
		episode(r.Episode),
		digests(r.VerdictDigest, r.FixDigest),
		impacts(r.ImpactMinutes, r.ImpactTokens),
		jsonOf("body", r.Body, '{', MaxBodyBytes, false),
		jsonOf("moved", r.Moved, '[', MaxMovedBytes, true),
	} {
		if check != nil {
			return check
		}
	}
	return nil
}

// validate refuses a refresh the log cannot keep.
func (r Refresh) validate() error {
	if r.Revision < 1 {
		return invalid("a refresh names the revision it refreshes, 1 or more, and this one is %d", r.Revision)
	}
	for _, check := range []error{
		episode(r.Episode),
		impacts(r.ImpactMinutes, r.ImpactTokens),
		jsonOf("body", r.Body, '{', MaxBodyBytes, false),
	} {
		if check != nil {
			return check
		}
	}
	return nil
}

// validate refuses a move whose event, state and fields disagree.
func (m Move) validate() error {
	if err := episode(m.Episode); err != nil {
		return err
	}
	if err := jsonOf("moved", m.Moved, '[', MaxMovedBytes, true); err != nil {
		return err
	}
	closing := m.Event == EventClosed
	switch {
	case !slices.Contains(MoveEvents, m.Event):
		return invalid("event %q is not one of %s", m.Event, words.Or(MoveEvents))
	case !m.State.Valid():
		return invalid("state %q is not one of %s", m.State, words.Or(States))
	case closing != m.State.Closed():
		return invalid("a closed move ends in a closed state and every other in open or applied, "+
			"and this one is %s to %s", m.Event, m.State)
	case closing && !m.Reason.Closes(m.State):
		return invalid("reason %q does not close a finding as %s: pass one of %s", m.Reason, m.State,
			words.Or(closeReasons[m.State]))
	case !closing && m.Reason != "":
		return invalid("only a closed move names a reason")
	case m.Hold != nil && m.State != StateDismissed:
		return invalid("only a dismissal holds its key")
	}
	switch m.Event {
	case EventApplied:
		switch {
		case m.State != StateApplied:
			return invalid("an applied move ends in applied")
		case !isDigest(m.FixDigest):
			return invalid("an applied move names the fix it applied by its digest (fix_digest)")
		case m.EffectDueAt.IsZero():
			return invalid("an applied move names when its effect is checked (effect_due_at)")
		}
	case EventIneffective:
		switch {
		case m.State != StateOpen:
			return invalid("an ineffective move ends in open")
		case !isDigest(m.FixDigest):
			return invalid("an ineffective move names the fix that did not take by its digest (fix_digest)")
		}
	case EventSnoozed:
		if m.State != StateOpen || m.SnoozedUntil.IsZero() {
			return invalid("a snoozed move stays open and names when it opens again (snoozed_until)")
		}
	case EventUnsnoozed, EventUseful:
		if m.State != StateOpen {
			return invalid("a %s move stays open", m.Event)
		}
	}
	if m.Hold != nil {
		if !m.Hold.Severity.Valid() {
			return invalid("a hold's severity %q is not one of %s", m.Hold.Severity, words.Or(Severities))
		}
		if m.Hold.Reason != "" {
			if err := printable("hold.reason", m.Hold.Reason, MaxWordBytes); err != nil {
				return err
			}
		}
	}
	return nil
}

// validate refuses an explanation the log cannot keep.
func (e Explanation) validate() error {
	switch {
	case !isDigest(e.VerdictDigest):
		return invalid("an explanation names the verdict it explains by its digest (verdict_digest)")
	case strings.TrimSpace(e.Text) == "" || len(e.Text) > MaxExplanationBytes:
		return invalid("an explanation is one to three sentences, in at most %d bytes", MaxExplanationBytes)
	case !utf8.ValidString(e.Text):
		return invalid("an explanation is valid UTF-8")
	}
	return episode(e.Episode)
}

// validate refuses a run request the log cannot keep.
func (r RunRequest) validate() error {
	if !IsUUID(r.Run) {
		return invalid("run is a uuid in its canonical lowercase form, and %q is not", r.Run)
	}
	return printable("by", r.By, MaxWordBytes)
}

// classLine is the part of one class's line of a run the log reads.
type classLine struct {
	Class  string      `json:"class"`
	Status ClassStatus `json:"status"`
}

// validate refuses a run record the log cannot keep, and answers its classes.
func (r RunRecord) validate() ([]classLine, error) {
	switch {
	case !IsUUID(r.Run):
		return nil, invalid("run is a uuid in its canonical lowercase form, and %q is not", r.Run)
	case !r.Trigger.Valid():
		return nil, invalid("trigger %q is not one of %s", r.Trigger, words.Or(Triggers))
	case !r.Status.Valid():
		return nil, invalid("status %q is not one of %s", r.Status, words.Or(RunStatuses))
	case r.StartedAt.IsZero() || r.EndedAt.Before(r.StartedAt):
		return nil, invalid("a run names when it started and when it ended, no earlier (started_at, ended_at)")
	case r.Trigger == TriggerPerson && r.By == "":
		return nil, invalid("a run a person started names them (by)")
	}
	if r.By != "" {
		if err := printable("by", r.By, MaxWordBytes); err != nil {
			return nil, err
		}
	}
	if err := jsonOf("classes", r.Classes, '[', MaxClassesBytes, false); err != nil {
		return nil, err
	}
	if trimmed := strings.TrimSpace(string(r.Model)); trimmed != "" && trimmed != "null" {
		if err := jsonOf("model", r.Model, '{', MaxModelBytes, false); err != nil {
			return nil, err
		}
	}
	return runClasses(r.Classes)
}

// runClasses reads a run's class lines, refusing one with no class or an
// unknown status.
func runClasses(raw json.RawMessage) ([]classLine, error) {
	var lines []classLine
	if err := json.Unmarshal(raw, &lines); err != nil {
		return nil, invalid("classes is a list of {class, status, ...}: %v", err)
	}
	for _, l := range lines {
		if err := printable("classes[].class", l.Class, MaxClassBytes); err != nil {
			return nil, err
		}
		if !l.Status.Valid() {
			return nil, invalid("class %s's status %q is not one of %s", l.Class, l.Status,
				words.Or(ClassStatuses))
		}
	}
	return lines, nil
}

// validate refuses a report the log cannot keep.
func (r Report) validate() error {
	switch {
	case !IsWeek(r.Week):
		return invalid("week %q is not an ISO week such as 2026-W47", r.Week)
	case r.From.IsZero() || !r.To.After(r.From):
		return invalid("a report names the period it covers, from before to (from, to)")
	}
	return jsonOf("body", r.Body, '{', MaxReportBytes, false)
}

// validate refuses an alarm raise the log cannot keep.
func (a AlarmRaise) validate() error {
	if err := series(a.Node, a.Kind, a.Instance); err != nil {
		return err
	}
	if a.RaisedAt.IsZero() {
		return invalid("an alarm's raise names when it was raised (raised_at)")
	}
	return nil
}

// series refuses an alarm series the log cannot name: a node and a kind, and
// an instance where the alarm has one.
func series(node, kind, instance string) error {
	if err := printable("node", node, MaxWordBytes); err != nil {
		return err
	}
	if err := printable("kind", kind, MaxWordBytes); err != nil {
		return err
	}
	if instance != "" {
		return printable("instance", instance, MaxSubjectIDBytes)
	}
	return nil
}

// validate refuses an expiry for day that is not a UTC day's start, or whose
// cutoff reaches inside its retention.
func (e Expiry) validate(day time.Time) error {
	if !day.Equal(day.UTC().Truncate(24 * time.Hour)) {
		return invalid("an expiry names the start of a UTC day, and %s is not one", day)
	}
	end := day.UTC().Add(24 * time.Hour)
	for _, cut := range []struct {
		field  string
		at     time.Time
		retain time.Duration
	}{
		{"closed_before", e.ClosedBefore, ClosedRetention},
		{"runs_before", e.RunsBefore, RunRetention},
		{"reports_before", e.ReportsBefore, ReportRetention},
		{"alarms_before", e.AlarmsBefore, AlarmRetention},
	} {
		if cut.at.IsZero() || cut.at.After(end.Add(-cut.retain)) {
			return invalid("%s is set, and no later than %s before the end of %s", cut.field,
				cut.retain, Day(day))
		}
	}
	return nil
}
