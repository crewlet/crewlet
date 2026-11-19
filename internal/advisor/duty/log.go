package duty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// LogReads is the advisor log's read side as the duty reads it: this node's
// copy, or the router's from a data node.
type LogReads interface {
	NonClosed(ctx context.Context, fresh statelog.Freshness) ([]advisordomain.FindingRow, error)
	Findings(ctx context.Context, f advisordomain.Filter, fresh statelog.Freshness) ([]advisordomain.FindingRow, string, error)
	PendingRequests(ctx context.Context, fresh statelog.Freshness) ([]advisordomain.RunRow, error)
	LastRunOfClasses(ctx context.Context, fresh statelog.Freshness) (map[string]time.Time, error)
}

// LogWrites is the advisor log's write side as the duty writes it.
type LogWrites interface {
	Open(ctx context.Context, c advisordomain.Call, o advisordomain.Opening, ifMatch uint64) (statelog.Result, error)
	Revise(ctx context.Context, c advisordomain.Call, key string, r advisordomain.Revision, ifMatch uint64) (statelog.Result, error)
	Refresh(ctx context.Context, c advisordomain.Call, key string, r advisordomain.Refresh, ifMatch uint64) (statelog.Result, error)
	Move(ctx context.Context, c advisordomain.Call, key string, m advisordomain.Move, ifMatch uint64) (statelog.Result, error)
	RecordRun(ctx context.Context, c advisordomain.Call, r advisordomain.RunRecord) (statelog.Result, error)
	Expire(ctx context.Context, c advisordomain.Call, day time.Time, e advisordomain.Expiry) (statelog.Result, error)
}

// Engine is who the duty writes as: the engine itself, never a person.
var Engine = iam.Actor{Name: "advisor", Kind: iam.ActorSystem}

// LogOf is the advisor log as the duty reads and writes it, through reads
// and writes, its operations minted on now.
func LogOf(reads LogReads, writes LogWrites, now func() time.Time) Log {
	return advisorLog{reads: reads, writes: writes, now: now}
}

type advisorLog struct {
	reads  LogReads
	writes LogWrites
	now    func() time.Time
}

// fresh is every read the duty makes: its own writes, read back on the node
// that made them.
var fresh = statelog.Freshness{Level: statelog.ReadSession}

func (l advisorLog) call(name string) advisordomain.Call {
	return advisordomain.Call{By: Engine, OpID: statelog.NewOpID(l.now(), name)}
}

// Live is every open and applied finding, as Reconcile compares them.
func (l advisorLog) Live(ctx context.Context) ([]advisor.Stored, error) {
	rows, err := l.reads.NonClosed(ctx, fresh)
	if err != nil {
		return nil, err
	}
	return StoredOf(rows)
}

// Dismissed is every model finding a person dismissed, newest first, a page
// at a time to the bound a pass is told of.
func (l advisorLog) Dismissed(ctx context.Context) ([]advisor.Stored, error) {
	var out []advisor.Stored
	f := advisordomain.Filter{Closed: true, Class: string(advisor.ClassModel), Limit: advisordomain.MaxPageSize}
	for range maxDismissedPages {
		rows, next, err := l.reads.Findings(ctx, f, fresh)
		if err != nil {
			return nil, err
		}
		stored, err := StoredOf(rows)
		if err != nil {
			return nil, err
		}
		for _, s := range stored {
			if s.State == advisor.StateDismissed {
				out = append(out, s)
			}
		}
		if next == "" {
			break
		}
		f.Cursor = next
	}
	return out, nil
}

// maxDismissedPages bounds how many dismissed model findings a pass is told
// of: five pages of the newest, more than a quarter of daily passes lists.
const maxDismissedPages = 5

// LastDone is when a run last concluded about each scope and group.
func (l advisorLog) LastDone(ctx context.Context) (map[string]time.Time, error) {
	return l.reads.LastRunOfClasses(ctx, fresh)
}

// Requested is every Run now no run has answered yet.
func (l advisorLog) Requested(ctx context.Context) ([]Request, error) {
	rows, err := l.reads.PendingRequests(ctx, fresh)
	if err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(rows))
	for _, r := range rows {
		out = append(out, Request{Run: r.ID, By: r.By})
	}
	return out, nil
}

// Record publishes t. A key another writer moved since the run read it, one
// held quiet by a dismissal, and one a peer opened first are left for the
// next run, which reads them as they now are.
func (l advisorLog) Record(ctx context.Context, t advisor.Transition, now time.Time) error {
	err := l.record(ctx, t, now)
	if errors.Is(err, advisordomain.ErrStaleVersion) || errors.Is(err, advisordomain.ErrHeld) ||
		errors.Is(err, advisordomain.ErrExists) {
		return nil
	}
	return err
}

func (l advisorLog) record(ctx context.Context, t advisor.Transition, now time.Time) error {
	switch t.Step {
	case advisor.StepOpen:
		o := *t.Observed
		body, err := json.Marshal(o)
		if err != nil {
			return err
		}
		f := o.Finding
		_, err = l.writes.Open(ctx, l.call("advisor.open"), advisordomain.Opening{
			Episode: advisordomain.EpisodeID(o.Key(), now), Key: o.Key(), Class: string(f.Class),
			Source: sourceOf(f), Actor: advisordomain.Actor(f.Actor), Area: string(f.Area),
			Severity: advisordomain.Severity(f.Severity), SubjectKind: string(f.Subject.Kind),
			SubjectID: advisor.SubjectID(f.Subject), Revision: 1,
			VerdictDigest: advisor.VerdictDigest(f), FixDigest: advisor.FixDigest(f.Fix), Body: body,
		}, advisordomain.NoIfMatch)
		return err
	case advisor.StepRevise:
		o, s := *t.Observed, t.Stored
		body, moved, err := bodyAndMoved(o, t.Moved)
		if err != nil {
			return err
		}
		_, err = l.writes.Revise(ctx, l.call("advisor.revise"), o.Key(), advisordomain.Revision{
			Episode: s.ID, Revision: s.Revision + 1, Severity: advisordomain.Severity(o.Finding.Severity),
			VerdictDigest: advisor.VerdictDigest(o.Finding), FixDigest: advisor.FixDigest(o.Finding.Fix),
			Body: body, Moved: moved,
		}, s.Version)
		return err
	case advisor.StepRefresh:
		o, s := *t.Observed, t.Stored
		body, err := json.Marshal(o)
		if err != nil {
			return err
		}
		_, err = l.writes.Refresh(ctx, l.call("advisor.refresh"), o.Key(), advisordomain.Refresh{
			Episode: s.ID, Revision: s.Revision, Body: body}, s.Version)
		return err
	case advisor.StepClose:
		s := t.Stored
		_, moved, err := bodyAndMoved(s.Observation, t.Moved)
		if err != nil {
			return err
		}
		_, err = l.writes.Move(ctx, l.call("advisor.close"), s.Observation.Key(), advisordomain.Move{
			Episode: s.ID, Event: advisordomain.EventClosed, State: advisordomain.State(t.State),
			Reason: advisordomain.CloseReason(t.Reason), Moved: moved}, s.Version)
		return err
	case advisor.StepIneffective:
		s := t.Stored
		_, moved, err := bodyAndMoved(s.Observation, t.Moved)
		if err != nil {
			return err
		}
		_, err = l.writes.Move(ctx, l.call("advisor.ineffective"), s.Observation.Key(), advisordomain.Move{
			Episode: s.ID, Event: advisordomain.EventIneffective, State: advisordomain.StateOpen,
			FixDigest: s.AppliedFix, Moved: moved}, s.Version)
		return err
	}
	return fmt.Errorf("duty: %q is not a step the log records", t.Step)
}

// bodyAndMoved is o and moved as the log keeps them.
func bodyAndMoved(o advisor.Observation, moved []advisor.Moved) (json.RawMessage, json.RawMessage, error) {
	body, err := json.Marshal(o)
	if err != nil {
		return nil, nil, err
	}
	if len(moved) == 0 {
		return body, nil, nil
	}
	m, err := json.Marshal(moved)
	return body, m, err
}

// sourceOf is f's source as the log keeps it: the model's, or a rule's.
func sourceOf(f advisor.Finding) advisordomain.Source {
	if f.Source == advisor.SourceModel {
		return advisordomain.SourceModel
	}
	return advisordomain.SourceRule
}

// ScopeRecord is one scope as a run record keeps it: the class key names the
// scope, and the counts are how many of its findings each step took.
type ScopeRecord struct {
	Class    string                    `json:"class"`
	Status   advisordomain.ClassStatus `json:"status"`
	Opened   int                       `json:"opened"`
	Revised  int                       `json:"revised"`
	Resolved int                       `json:"resolved"`
}

// RecordRun publishes how r ended.
func (l advisorLog) RecordRun(ctx context.Context, r Run) error {
	scopes := make([]ScopeRecord, 0, len(r.Scopes))
	for _, s := range r.Scopes {
		status := advisordomain.ClassDone
		if s.Status == ScopeUnread {
			status = advisordomain.ClassUnread
		}
		scopes = append(scopes, ScopeRecord{Class: s.Scope, Status: status, Opened: s.Steps[advisor.StepOpen],
			Revised: s.Steps[advisor.StepRevise], Resolved: s.Steps[advisor.StepClose]})
	}
	classes, err := json.Marshal(scopes)
	if err != nil {
		return err
	}
	model := json.RawMessage("null")
	if r.Model != nil {
		if model, err = json.Marshal(r.Model); err != nil {
			return err
		}
	}
	status := advisordomain.RunDone
	if r.Status == RunFailed {
		status = advisordomain.RunFailed
	}
	_, err = l.writes.RecordRun(ctx, l.call("advisor.run"), advisordomain.RunRecord{
		Run: r.ID, Trigger: advisordomain.Trigger(r.Trigger), By: r.By, StartedAt: r.StartedAt,
		EndedAt: r.EndedAt, Status: status, Classes: classes, Model: model})
	return err
}

// Expire publishes the day's retention once; a day already expired is done.
func (l advisorLog) Expire(ctx context.Context, now time.Time) error {
	day, e := advisordomain.ExpiryFor(now)
	_, err := l.writes.Expire(ctx, l.call("advisor.expire"), day, e)
	if errors.Is(err, advisordomain.ErrExists) {
		return nil
	}
	return err
}

// StoredOf is rows as Reconcile reads them, each body decoded.
func StoredOf(rows []advisordomain.FindingRow) ([]advisor.Stored, error) {
	out := make([]advisor.Stored, 0, len(rows))
	for _, r := range rows {
		var o advisor.Observation
		if err := json.Unmarshal(r.Body, &o); err != nil {
			return nil, fmt.Errorf("duty: decode finding %s: %w", r.ID, err)
		}
		out = append(out, advisor.Stored{ID: r.ID, Version: r.KeyVersion, State: advisor.State(r.State),
			Revision: r.Revision, EffectDueAt: r.EffectDueAt, AppliedFix: r.AppliedFix,
			Reason: advisor.CloseReason(r.CloseReason), ClosedAt: r.ClosedAt,
			Observation: o, Body: r.Body})
	}
	return out, nil
}
