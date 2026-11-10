package insights

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("insights")

// RelayURL is where a record goes. A constant: the document has no say in it.
const RelayURL = "https://reports.crewlet.ai/v1/insights"

// MaxSendAttempts bounds the sends of one record the relay did not take.
const MaxSendAttempts = 3

// MaxDelay bounds the random wait before a record goes, so no two records of a
// week leave together.
const MaxDelay = 24 * time.Hour

// dayLayout is a company day's label.
const dayLayout = "2006-01-02"

// ErrNotQueued is a decision on a record that is not waiting for one.
var ErrNotQueued = errors.New("insights: the record is not waiting for a decision")

// Sharer moves a company's records from first seen to sent, under its consent
// (ADR-0119). Every move is a claim on the fleet's record at the version read,
// so of any number of nodes one makes it.
type Sharer struct {
	Store  coord.InsightRecords
	Origin issuereport.Origin
	// Findings is the advisor's answer's findings, read now.
	Findings func(ctx context.Context) ([]advisor.Finding, error)
	// Consent is the revision's consent, read on every pass. A consent that
	// cannot be read stops the pass: it is never taken for off.
	Consent func(ctx context.Context) (config.InsightsSharing, error)
	// Day is the company day at an instant, as a `2006-01-02` label.
	Day func(time.Time) string
	// Delay is a fresh random wait within [MaxDelay].
	Delay func() time.Duration
	// Send posts one record's bytes to the relay and answers its status code,
	// 0 when it did not answer. Nil on a build that sends nothing.
	Send func(ctx context.Context, body []byte) (int, error)
	Now  func() time.Time
}

// consenting reports whether the consent shares anything: on, and accepted for
// this build's schema.
func (s *Sharer) consenting(ctx context.Context) (config.InsightsSharing, bool, error) {
	c, err := s.Consent(ctx)
	if err != nil {
		return config.InsightsSharing{}, false, fmt.Errorf("insights: read the consent: %w", err)
	}
	on := c.Share == config.InsightsReview || c.Share == config.InsightsAuto
	return c, on && c.Accepted == SchemaVersion, nil
}

// Evaluate reads the findings once: a class raised on two days in a row of one
// ISO week becomes eligible, queued for review or scheduled. Without consent it
// forgets every record that waits, which is what off means.
func (s *Sharer) Evaluate(ctx context.Context) error {
	consent, ok, err := s.consenting(ctx)
	switch {
	case err != nil:
		return err
	case !ok:
		return s.forget(ctx)
	}
	findings, err := s.Findings(ctx)
	if err != nil {
		return fmt.Errorf("insights: read the findings: %w", err)
	}
	now := s.Now()
	today := s.Day(now)
	for _, r := range Records(findings, now, s.Origin) {
		if err := s.observe(ctx, r, today, consent, now); err != nil {
			return err
		}
	}
	return nil
}

// observe moves one record's entry on a day it was raised.
func (s *Sharer) observe(ctx context.Context, r Record, today string, consent config.InsightsSharing, now time.Time) error {
	held, found, err := s.Store.InsightRecord(ctx, string(r.class), string(r.week))
	if err != nil {
		return fmt.Errorf("insights: read %s's week: %w", r.class, err)
	}
	want := coord.InsightRecord{Class: string(r.class), Week: string(r.week), Status: coord.InsightObserved, Day: today}
	switch {
	case found && held.Status != coord.InsightObserved, found && held.Day == today:
		return nil
	case found && held.Day == previousDay(today):
		if want.Payload, err = json.Marshal(r); err != nil {
			return fmt.Errorf("insights: encode %s: %w", r.class, err)
		}
		want.Status = coord.InsightQueued
		if consent.Share == config.InsightsAuto || slices.Contains(consent.Always, string(r.class)) {
			want.Status, want.SendAt = coord.InsightScheduled, now.Add(s.Delay())
		}
	}
	_, _, err = s.Store.ClaimInsightRecord(ctx, want, atVersion(held, found))
	if err != nil {
		return fmt.Errorf("insights: move %s's week: %w", r.class, err)
	}
	return nil
}

// SendDue sends every scheduled record whose time has come, under consent.
func (s *Sharer) SendDue(ctx context.Context) error {
	// ONLY A PUBLIC BUILD SENDS (ADR-0101's origin rule), whatever another
	// node scheduled in the fleet's store.
	if s.Send == nil || s.Origin.InertReason() != "" {
		return nil
	}
	if _, ok, err := s.consenting(ctx); err != nil || !ok {
		return err
	}
	records, err := s.Store.ListInsightRecords(ctx)
	if err != nil {
		return fmt.Errorf("insights: list the records: %w", err)
	}
	now := s.Now()
	for _, held := range records {
		if stale(held, now) {
			failed := held
			failed.Status = coord.InsightFailed
			if _, _, err := s.Store.ClaimInsightRecord(ctx, failed, atVersion(held, true)); err != nil {
				return fmt.Errorf("insights: fail %s's stale send: %w", held.Class, err)
			}
			continue
		}
		if held.Status != coord.InsightScheduled || held.SendAt.After(now) {
			continue
		}
		sending := held
		sending.Status, sending.ClaimedAt = coord.InsightSending, now
		claimed, won, err := s.Store.ClaimInsightRecord(ctx, sending, atVersion(held, true))
		if err != nil {
			return fmt.Errorf("insights: claim %s's week: %w", held.Class, err)
		}
		if won {
			s.settle(ctx, claimed, now)
		}
	}
	return nil
}

// settle sends one claimed record and writes what happened: sent, or another
// attempt at a new delay, or failed after the last. Only a payload that is a
// valid sealed record leaves, whatever the store held.
func (s *Sharer) settle(ctx context.Context, claimed coord.InsightRecord, now time.Time) {
	next := claimed
	next.Answer = 0
	err := validPayload(claimed.Payload)
	if err == nil {
		next.Answer, err = s.Send(ctx, claimed.Payload)
	}
	switch {
	case err == nil:
		next.Status = coord.InsightSent
	case claimed.Attempts+1 >= MaxSendAttempts:
		next.Status, next.Attempts = coord.InsightFailed, claimed.Attempts+1
	default:
		next.Status, next.Attempts, next.SendAt = coord.InsightScheduled, claimed.Attempts+1, now.Add(s.Delay())
	}
	if err != nil {
		log.WarnContext(ctx, "insight_not_sent", "class", claimed.Class, "week", claimed.Week,
			"attempt", next.Attempts, "answer", next.Answer, "error", err.Error())
	}
	if _, _, err := s.Store.ClaimInsightRecord(ctx, next, atVersion(claimed, true)); err != nil {
		log.WarnContext(ctx, "insight_not_settled", "class", claimed.Class, "week", claimed.Week, "error", err.Error())
	}
}

// Decide is an administrator's answer to a queued record: send schedules it at
// a random delay, and anything else declines it.
func (s *Sharer) Decide(ctx context.Context, class, week string, send bool) (coord.InsightRecord, error) {
	consent, ok, err := s.consenting(ctx)
	if err != nil {
		return coord.InsightRecord{}, err
	}
	if !ok || consent.Share != config.InsightsReview {
		return coord.InsightRecord{}, ErrNotQueued
	}
	held, found, err := s.Store.InsightRecord(ctx, class, week)
	if err != nil {
		return coord.InsightRecord{}, fmt.Errorf("insights: read %s's week: %w", class, err)
	}
	if !found || held.Status != coord.InsightQueued {
		return coord.InsightRecord{}, ErrNotQueued
	}
	next := held
	next.Status = coord.InsightDeclined
	if send {
		next.Status, next.SendAt = coord.InsightScheduled, s.Now().Add(s.Delay())
	}
	stored, won, err := s.Store.ClaimInsightRecord(ctx, next, atVersion(held, true))
	switch {
	case err != nil:
		return coord.InsightRecord{}, fmt.Errorf("insights: decide %s's week: %w", class, err)
	case !won:
		return coord.InsightRecord{}, ErrNotQueued
	}
	return stored, nil
}

// forget deletes every record that waits: off means off, across restarts,
// since the consent is the revision's and every pass reads it.
func (s *Sharer) forget(ctx context.Context) error {
	records, err := s.Store.ListInsightRecords(ctx)
	if err != nil {
		return fmt.Errorf("insights: list the records: %w", err)
	}
	now := s.Now()
	for _, r := range records {
		if r.Status == coord.InsightObserved || r.Status == coord.InsightQueued ||
			r.Status == coord.InsightScheduled || stale(r, now) {
			if _, err := s.Store.DeleteInsightRecord(ctx, r); err != nil {
				return fmt.Errorf("insights: forget %s's week: %w", r.Class, err)
			}
		}
	}
	return nil
}

// StaleSend is how long a claim to send may stand before it reads as failed: a
// node that stopped mid-send never settles its claim.
const StaleSend = time.Hour

// stale reports whether r is a send claimed StaleSend or more before now.
func stale(r coord.InsightRecord, now time.Time) bool {
	return r.Status == coord.InsightSending && now.Sub(r.ClaimedAt) >= StaleSend
}

// atVersion admits the held record only at the version read, or an empty entry.
func atVersion(held coord.InsightRecord, found bool) func(coord.InsightRecord) bool {
	return func(now coord.InsightRecord) bool { return found && now.Version == held.Version }
}

// previousDay is the label of the day before a day's label, "" for no label.
func previousDay(day string) string {
	t, err := time.Parse(dayLayout, day)
	if err != nil {
		return ""
	}
	return t.AddDate(0, 0, -1).Format(dayLayout)
}

// validPayload refuses bytes that are not a sealed record the catalogue allows.
func validPayload(raw []byte) error {
	var wire recordJSON
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return fmt.Errorf("insights: a stored record does not decode: %w", err)
	}
	r := Record{schema: wire.Schema, class: wire.Class, week: wire.Week, version: wire.Version, facts: wire.Facts}
	return r.Valid()
}
