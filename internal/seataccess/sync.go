package seataccess

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// SyncInput is one kind's sync: which vendor, the document, the ledger, and
// where minted credentials are sealed.
type SyncInput struct {
	Vendor Vendor
	Open   OpenInput
	Ledger Ledger

	// Sink seals minted credentials. NIL IS A DRY RUN, as it is for every
	// pass: the vendor is read back and the ledger's observed state
	// refreshed, and nothing is changed at the vendor.
	Sink provision.TokenSink

	// Pass identifies this sync, recorded on every account it creates.
	Pass string

	// By is who the sync runs for, recorded on every credential it seals.
	By string

	// Asks are one-offs a person asked for, by handle.
	Asks map[string]Ask

	// Handles limits the sync to these seats; empty is every seat.
	Handles []string

	// Disconnect is a sync for an integration being disconnected: every
	// seat is off whatever the document says, so each account is disabled
	// and its credential revoked, and removed where Asks says so.
	Disconnect bool

	// Unmigrated is an integration whose seats the migration onto per-seat
	// access has not written yet. Its document reads every seat as off, and
	// acting on that would disable the very accounts the migration is about
	// to record, so the sync makes no vendor call at all and reports it.
	Unmigrated bool

	// Now is the clock; nil is the wall clock in UTC.
	Now func() time.Time

	// OnDrift is told of every drift a sync restored (D2), for the
	// drift_restored event. Nil records none.
	OnDrift func(ctx context.Context, d DriftRestored)
}

// DriftRestored is one setting a sync put back after somebody changed it at
// the vendor (D2): the vendor's value before, the document's after.
type DriftRestored struct {
	Kind   integration.Kind
	Lane   Lane
	Handle string
	Before Desired
	After  Desired
	At     time.Time
}

// SyncReport is what a sync left: each seat's ledger record, and the findings
// for the integration's status in the shared vocabulary.
type SyncReport struct {
	Records  map[string]Record
	Findings []integration.Finding
}

// Sync holds the vendor to the document for every seat at one kind: it reads
// the vendor back, plans each seat with [Plan]'s rules, takes the steps,
// seals what was minted, and records the outcome in the ledger.
//
// An error is a fault the whole kind shares (the vendor could not be opened
// or read, the ledger could not be read or written); a failure confined to one
// seat is that seat's finding and its lane's sync result, and the next seat is
// synced regardless. An error wrapping [integration.ErrNotConfigured] is a
// company that does not provision this vendor.
func Sync(ctx context.Context, in SyncInput) (SyncReport, error) {
	if in.Vendor == nil || in.Ledger == nil {
		return SyncReport{}, errors.New("seataccess: a sync needs its vendor and the ledger")
	}
	if in.Open.Config == nil {
		return SyncReport{}, integration.ErrNotConfigured
	}
	cat := in.Vendor.Catalog()
	kind := cat.Kind
	if in.Unmigrated {
		// NOT EVEN OPENED: a session may write a company value of its own
		// on the way in (Slack's configuration token).
		return SyncReport{Findings: []integration.Finding{{
			Kind: integration.FindingIdentityMissing,
			Detail: "the migration onto per-seat access has not finished for this " +
				"integration, so its agents' accounts are left exactly as they are",
			Remedy: "It is retried every minute; the log's seat_access_migration_pending " +
				"line says what it is waiting on, usually the vendor not answering",
		}}}, nil
	}
	now := in.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	refs, err := in.Open.Config.References()
	if err != nil {
		return SyncReport{}, fmt.Errorf("seataccess: %s: %w", kind, err)
	}
	sess, err := in.Vendor.Open(ctx, in.Open)
	if err != nil {
		return SyncReport{}, fmt.Errorf("seataccess: open %s: %w", kind, err)
	}
	records, err := in.Ledger.Records(ctx, kind)
	if err != nil {
		return SyncReport{}, fmt.Errorf("seataccess: read the %s ledger: %w", kind, err)
	}
	seats := map[string]Seat{}
	for _, seat := range SeatsOf(in.Open.Config) {
		seats[seat.Handle] = seat
	}
	handles := syncHandles(seats, records, in.Handles)
	observeSeats := make([]SeatRef, 0, len(handles))
	for _, h := range handles {
		if seat, ok := seats[h]; ok {
			observeSeats = append(observeSeats, seat.SeatRef)
		} else {
			observeSeats = append(observeSeats, SeatRef{Handle: h, Name: records[h].Name})
		}
	}
	obs, err := sess.Observe(ctx, ObserveInput{Seats: observeSeats, Records: records})
	if err != nil {
		return SyncReport{}, fmt.Errorf("seataccess: read %s back: %w", kind, err)
	}
	s := &syncer{
		in: in, cat: cat, kind: kind, sess: sess, refs: refs, now: now(),
		options: map[Lane]Options{},
		report:  SyncReport{Records: map[string]Record{}},
	}
	for _, h := range handles {
		if err := ctx.Err(); err != nil {
			return s.report, err
		}
		seat, inDoc := seats[h]
		if err := s.seat(ctx, h, seat, inDoc, records[h], obs[h]); err != nil {
			return s.report, err
		}
	}
	return s.report, nil
}

// syncHandles is every seat a sync visits: the document's agents and every
// seat the ledger still records, limited to only where that is set, sorted.
func syncHandles(seats map[string]Seat, records map[string]Record, only []string) []string {
	set := map[string]bool{}
	for h := range seats {
		set[h] = true
	}
	for h := range records {
		set[h] = true
	}
	out := slices.Sorted(maps.Keys(set))
	if len(only) == 0 {
		return out
	}
	return slices.DeleteFunc(out, func(h string) bool { return !slices.Contains(only, h) })
}

// syncer is one kind's sync in progress.
type syncer struct {
	in      SyncInput
	cat     Catalog
	kind    integration.Kind
	sess    Session
	refs    config.VariableReferences
	now     time.Time
	options map[Lane]Options
	report  SyncReport
}

// seat syncs one seat. An error is a fault the kind shares.
func (s *syncer) seat(
	ctx context.Context, h string, seat Seat, inDoc bool, rec Record, got map[Lane]Observed,
) error {
	if rec.Handle == "" {
		rec = Record{Kind: s.kind, Handle: h}
	}
	if inDoc {
		rec.Name = seat.Name
	}
	if rec.Lanes == nil {
		rec.Lanes = map[Lane]LaneRecord{}
	}
	ref := SeatRef{Handle: h, Name: rec.Name}
	lanes := LanesOf(s.kind)
	want := map[Lane]Desired{}
	if inDoc && !s.in.Disconnect {
		for _, lane := range lanes {
			want[lane], _ = DesiredOf(seat.Role, lane)
		}
	}
	have := observedFor(lanes, got, rec, s.now)
	s.adopt(&rec, want, have)

	// THE VENDOR SHOWS THE ACCOUNT GONE: the record is closed, its sealed
	// credential is dead and is cleared, and a seat that is on is created
	// afresh by the plan below.
	if rec.Held() && !anyExists(have) {
		rec.Account.State = AccountClosed
		if err := s.forget(ctx, &rec, s.cat.Credentials); err != nil {
			s.faultAll(&rec, lanes, VerbRevoke, err)
			return s.put(ctx, rec)
		}
	}

	// DEFENCE IN DEPTH: never seal into a name something else reads.
	if shared := SharedSinks(s.cat, h, s.refs); len(shared) > 0 {
		f := &Failure{
			Step: VerbMint,
			Said: fmt.Sprintf("%s is named elsewhere in the company document, so a "+
				"credential sealed there would be read by something other than this agent",
				strings.Join(shared, ", ")),
			Remedy: "Remove every reference to " + strings.Join(shared, ", ") +
				" from the company document; the engine binds the agent's own credential itself",
			Actor: ByYou,
		}
		s.failAll(&rec, lanes, f)
		return s.put(ctx, rec)
	}

	// A LEVEL OR SCOPE THE VENDOR DOES NOT OFFER is a finding, never a
	// silent fallback: the lane is left as it is until the document names one
	// the vendor has.
	blocked := map[Lane]*Failure{}
	for _, lane := range lanes {
		d := want[lane]
		if !d.Enabled || s.in.Sink == nil {
			continue
		}
		f, err := s.offered(ctx, lane, d)
		if err != nil {
			return err
		}
		if f != nil {
			blocked[lane] = f
		}
	}

	var removed bool
	err := walk(s.in.Vendor, SeatInput{
		Seat: ref, Want: want, Have: have, Record: &rec, Ask: s.in.Asks[h],
	}, func(lane Lane, w Desired, hv Observed, steps []Step) laneEffect {
		if f := blocked[lane]; f != nil {
			s.finish(&rec, lane, w, hv, SyncResult{At: s.now, Outcome: OutcomeFailed, Error: f})
			s.finding(integration.FindingUnknownTier, h, lane, f)
			return laneEffect{}
		}
		if s.in.Sink == nil {
			// A DRY RUN refreshes what the vendor shows and keeps the last
			// sync's result.
			lr := rec.Lanes[lane]
			lr.Observed = hv
			rec.Lanes[lane] = lr
			return laneEffect{}
		}
		eff := s.take(ctx, &rec, ref, lane, w, hv, steps)
		removed = removed || eff.removed
		return eff
	})
	if err != nil {
		return fmt.Errorf("seataccess: plan %s for %s: %w", s.kind, h, err)
	}
	if removed {
		if err := s.forget(ctx, &rec, s.cat.Credentials); err != nil {
			s.faultAll(&rec, lanes, VerbRemove, err)
			return s.put(ctx, rec)
		}
		if err := s.in.Ledger.Drop(ctx, s.kind, h); err != nil {
			return fmt.Errorf("seataccess: drop %s's %s record: %w", h, s.kind, err)
		}
		return nil
	}
	if !inDoc && !rec.Held() {
		// A SEAT THAT LEFT THE DOCUMENT whose account is gone has nothing
		// left to report or reach.
		if err := s.in.Ledger.Drop(ctx, s.kind, h); err != nil {
			return fmt.Errorf("seataccess: drop %s's %s record: %w", h, s.kind, err)
		}
		return nil
	}
	return s.put(ctx, rec)
}

// adopt records, rather than duplicates, what the vendor already holds for a
// seat the ledger has no account for: an account under the engine's naming
// (one made before the ledger existed), where the seat is on; and a
// credential sealed outside the sync (a GitHub App's key from the manifest
// route, a Slack bot token from the install landing), wherever it is found.
func (s *syncer) adopt(rec *Record, want map[Lane]Desired, have map[Lane]Observed) {
	on := false
	for _, d := range want {
		on = on || d.Enabled
	}
	for _, lane := range LanesOf(s.kind) {
		o := have[lane]
		if !o.Exists {
			continue
		}
		if on && !rec.Held() && o.AccountID != "" {
			created := o.CreatedAt
			if created.IsZero() {
				created = s.now
			}
			rec.Account = &Account{
				ID: o.AccountID, Name: o.Account, CreatedAt: created,
				CreatedByPass: s.in.Pass, State: AccountActive, Adopted: true,
			}
		}
		if rec.Credential == nil && o.Credential != nil {
			cred := *o.Credential
			if cred.Variable == "" {
				cred.Variable = SinkName(s.kind, s.cat.Credentials[0], rec.Handle)
			}
			rec.Credential = &cred
		}
	}
}

// put records the seat's record and reports it.
func (s *syncer) put(ctx context.Context, rec Record) error {
	if err := s.in.Ledger.Put(ctx, rec); err != nil {
		return fmt.Errorf("seataccess: record %s's %s account: %w", rec.Handle, s.kind, err)
	}
	s.report.Records[rec.Handle] = rec
	return nil
}

// offered is the failure for a lane whose level or scope the vendor does not
// offer now, or nil.
func (s *syncer) offered(ctx context.Context, lane Lane, d Desired) (*Failure, error) {
	spec, ok := s.cat.Lane(lane)
	if !ok || (!spec.Levels && !spec.Scoped) {
		return nil, nil
	}
	opts, ok := s.options[lane]
	if !ok {
		var err error
		if opts, err = s.sess.Options(ctx, lane); err != nil {
			return nil, fmt.Errorf("seataccess: read what %s offers: %w", LaneName(lane), err)
		}
		s.options[lane] = opts
	}
	if spec.Levels {
		if d.Level == "" {
			return &Failure{
				Step: VerbGrant, Actor: ByYou,
				Said:   fmt.Sprintf("no %s access is set for this agent", LaneName(lane)),
				Remedy: "Choose its access on the integration's Agents tab",
			}, nil
		}
		if _, _, ok := opts.Level(d.Level); !ok {
			return &Failure{
				Step: VerbGrant, Actor: ByYou,
				Said:   fmt.Sprintf("%s does not offer the access %q any more", LaneName(lane), d.Level),
				Remedy: "Choose one it offers on the integration's Agents tab",
			}, nil
		}
	}
	if spec.Scoped && !opts.ScopeOpen {
		for _, entry := range d.Scope {
			if !slices.Contains(opts.Scope, entry) {
				return &Failure{
					Step: VerbScope, Actor: ByYou,
					Said:   fmt.Sprintf("%s is outside what the company's %s connection reaches", entry, LaneName(lane)),
					Remedy: "Add it to the company's connection, or remove it from this agent",
				}, nil
			}
		}
	}
	return nil, nil
}

// take takes one lane's steps in order, stopping at the first that fails or
// waits on a person, and records how the lane ended.
func (s *syncer) take(
	ctx context.Context, rec *Record, ref SeatRef, lane Lane, want Desired, have Observed, steps []Step,
) laneEffect {
	result := SyncResult{At: s.now, Outcome: OutcomeOK, Steps: []StepOutcome{}}
	var eff laneEffect
	if len(steps) == 0 && want.Enabled && have.Exists && !have.Enabled && have.DisabledBy == DisabledByHand {
		// D3, REPORTED: the plan leaves it off, and a person decides.
		f := &Failure{
			Step: VerbEnable, Actor: ByYou,
			Said: fmt.Sprintf("the account was turned off at %s by somebody else", LaneName(lane)),
			Remedy: "Turn it back on from the integration's Agents tab, or keep it off, " +
				"which turns the agent off there",
		}
		result.Outcome, result.Error = OutcomeWaiting, f
		s.finish(rec, lane, want, have, result)
		s.finding(integration.FindingIdentityFailed, ref.Handle, lane, f)
		return eff
	}
	sinks := SinkNames(s.cat, ref.Handle)
	var prior []StepResult
	for i, step := range steps {
		if i == 0 || steps[i-1].Op != step.Op {
			prior = nil
		}
		res, err := s.sess.Apply(ctx, ApplyInput{
			Seat: ref, Lane: lane, Step: step, Want: want, Have: have, Record: rec, Sinks: sinks,
			Prior: slices.Clone(prior),
		})
		if err == nil && res.Waiting != nil {
			result.Steps = append(result.Steps, StepOutcome{Step: step})
			result.Outcome = OutcomeWaiting
			result.Error = &Failure{
				Step: step.Verb, Actor: ByYou, ActionURL: firstNonEmpty(res.Waiting.ActionURL, step.ActionURL),
				Said:   firstNonEmpty(res.Waiting.Detail, step.Text),
				Remedy: "Take this step at " + LaneName(lane) + ", then choose Done, check again",
			}
			break
		}
		if err == nil {
			err = s.absorb(ctx, rec, step, res, &eff)
		}
		if err != nil {
			result.Steps = append(result.Steps, StepOutcome{Step: step})
			result.Outcome = OutcomeFailed
			result.Error = failureOf(step, lane, err)
			break
		}
		prior = append(prior, res)
		done := StepOutcome{Step: step, Done: true}
		if res.Did != "" {
			done.Text = res.Did
		}
		result.Steps = append(result.Steps, done)
	}
	s.finish(rec, lane, want, have, result)
	switch result.Outcome {
	case OutcomeFailed:
		eff.removed = false
		s.finding(integration.FindingIdentityFailed, ref.Handle, lane, result.Error)
	case OutcomeWaiting:
		eff.removed = false
		s.finding(integration.FindingApprovalRequired, ref.Handle, lane, result.Error)
	case OutcomeOK:
		s.drifted(ctx, ref.Handle, lane, want, have, result.Steps)
	}
	return eff
}

// absorb records what one step did: the account it created, the credentials
// it minted (sealed here, and revoked if they cannot be), the values it made
// dead, and the account's state.
func (s *syncer) absorb(ctx context.Context, rec *Record, step Step, res StepResult, eff *laneEffect) error {
	if res.AccountID != "" {
		rec.Account = &Account{
			ID: res.AccountID, Name: res.Account, CreatedAt: s.now,
			CreatedByPass: s.in.Pass, State: AccountActive,
		}
		eff.created = true
	}
	if len(res.Minted) > 0 {
		cred, err := s.seal(ctx, rec.Handle, res.Minted)
		if err != nil {
			return err
		}
		if cred != nil {
			rec.Credential = cred
			eff.credential = cred
		}
	}
	if len(res.Revoked) > 0 {
		if err := s.forget(ctx, rec, res.Revoked); err != nil {
			return err
		}
		eff.revoked = eff.revoked || slices.Contains(res.Revoked, s.cat.Credentials[0])
	}
	if rec.Account != nil {
		switch {
		case res.Gone:
			rec.Account.State = AccountClosed
		case res.Disabled:
			rec.Account.State = AccountDisabled
		case step.Op == OpEnable || step.Op == OpCreate:
			rec.Account.State = AccountActive
		}
	}
	if step.Op == OpRemove {
		eff.removed = true
	}
	return nil
}

// seal records every minted value under the seat's engine-owned name.
//
// A FAILED RECORD REVOKES WHAT WAS MINTED, for every vendor: a credential that
// exists at the vendor and is sealed nowhere is one nobody can use and nobody
// will remember to remove. The values this call did record are forgotten
// again, so a dead value never reads like a live one.
func (s *syncer) seal(ctx context.Context, handle string, minted []Minted) (*Credential, error) {
	var recorded []string
	var err error
	for _, m := range minted {
		if !slices.Contains(s.cat.Credentials, m.Field) {
			err = fmt.Errorf("seataccess: %s minted %q, which is not one of its credential fields", s.kind, m.Field)
			break
		}
		name := SinkName(s.kind, m.Field, handle)
		if err = s.in.Sink.Record(ctx, name, m.Value); err != nil {
			err = fmt.Errorf("seal %s: %w", name, err)
			break
		}
		recorded = append(recorded, name)
	}
	if err != nil {
		cleanup := context.WithoutCancel(ctx)
		var undo []error
		for _, m := range minted {
			if m.Revoke == nil {
				continue
			}
			if rerr := m.Revoke(cleanup); rerr != nil {
				undo = append(undo, fmt.Errorf("revoke the minted %s: %w", m.Field, rerr))
			}
		}
		if len(recorded) > 0 {
			if ferr := s.in.Sink.Forget(cleanup, recorded...); ferr != nil {
				undo = append(undo, fmt.Errorf("clear %s: %w", strings.Join(recorded, ", "), ferr))
			}
		}
		return nil, errors.Join(append([]error{err}, undo...)...)
	}
	// THE CREDENTIAL ITSELF, OR NOTHING TO RECORD: a step that minted only
	// the account's other values (Slack's app secrets before a person
	// installs it) has sealed no credential the seat can use yet.
	if !slices.ContainsFunc(minted, func(m Minted) bool { return m.Field == s.cat.Credentials[0] }) {
		return nil, nil
	}
	sinks := SinkNames(s.cat, handle)
	cred := &Credential{Variable: sinks[s.cat.Credentials[0]], MintedAt: s.now, By: s.in.By}
	for _, field := range s.cat.Credentials[1:] {
		cred.Others = append(cred.Others, sinks[field])
	}
	return cred, nil
}

// forget clears the sealed values for fields, never one something outside the
// seat names, and drops the record's credential when the credential itself
// went.
func (s *syncer) forget(ctx context.Context, rec *Record, fields []string) error {
	if s.in.Sink == nil {
		return nil
	}
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, SinkName(s.kind, field, rec.Handle))
	}
	names = Forgettable(names, rec.Handle, s.refs)
	if len(names) > 0 {
		if err := s.in.Sink.Forget(ctx, names...); err != nil {
			return fmt.Errorf("clear %s: %w", strings.Join(names, ", "), err)
		}
	}
	if slices.Contains(fields, s.cat.Credentials[0]) {
		rec.Credential = nil
	}
	return nil
}

// finish records how a lane's sync ended: on success the document's setting
// is what was applied and what the vendor now holds.
func (s *syncer) finish(rec *Record, lane Lane, want Desired, have Observed, result SyncResult) {
	lr := rec.Lanes[lane]
	lr.Sync = &result
	lr.Observed = have
	if result.Outcome == OutcomeOK {
		applied := want
		lr.Applied = &applied
		lr.Observed = converged(rec, want, have)
	}
	rec.Lanes[lane] = lr
}

// converged is what the vendor holds once a lane's steps all succeeded.
func converged(rec *Record, want Desired, have Observed) Observed {
	out := have
	out.Exists = rec.Held()
	out.Credential = rec.Credential
	if rec.Account != nil {
		out.AccountID, out.Account, out.CreatedAt = rec.Account.ID, rec.Account.Name, rec.Account.CreatedAt
	}
	if !out.Exists {
		return Observed{}
	}
	out.Enabled = want.Enabled && (have.Enabled || have.DisabledBy != DisabledByHand)
	if !out.Enabled && want.Enabled {
		return out
	}
	if want.Enabled {
		out.Level, out.Permissions, out.Scope = want.Level, want.Permissions, want.Scope
		out.DisabledBy = ""
		out.ChangedByHandAt = time.Time{}
	} else {
		out.DisabledBy = DisabledByCrewlet
	}
	return out
}

// drifted tells OnDrift of every drift step the lane took.
func (s *syncer) drifted(ctx context.Context, h string, lane Lane, want Desired, have Observed, steps []StepOutcome) {
	if s.in.OnDrift == nil {
		return
	}
	for _, step := range steps {
		if step.Drift && step.Done {
			s.in.OnDrift(ctx, DriftRestored{
				Kind: s.kind, Lane: lane, Handle: h,
				Before: have.Access(), After: want, At: s.now,
			})
			return
		}
	}
}

// failAll records one failure on every lane of a seat that cannot be synced
// at all.
func (s *syncer) failAll(rec *Record, lanes []Lane, f *Failure) {
	for _, lane := range lanes {
		lr := rec.Lanes[lane]
		lr.Sync = &SyncResult{At: s.now, Outcome: OutcomeFailed, Steps: []StepOutcome{}, Error: f}
		rec.Lanes[lane] = lr
	}
	s.finding(integration.FindingIdentityFailed, rec.Handle, "", f)
}

// faultAll is failAll for a fault rather than a refusal.
func (s *syncer) faultAll(rec *Record, lanes []Lane, verb StepVerb, err error) {
	s.failAll(rec, lanes, failureOf(Step{Verb: verb}, "", err))
}

// finding adds one seat's finding to the report.
func (s *syncer) finding(kind integration.FindingKind, h string, lane Lane, f *Failure) {
	detail := f.Said
	if lane != "" {
		detail = LaneName(lane) + ": " + f.Said
	}
	s.report.Findings = append(s.report.Findings, integration.Finding{
		Kind: kind, Subject: h, Detail: detail, Remedy: f.Remedy, ActionURL: f.ActionURL,
	})
}

// failureOf is the failure a step's error reports: the vendor's own refusal,
// or a fault the next pass retries.
func failureOf(step Step, lane Lane, err error) *Failure {
	if r, ok := errors.AsType[*Refusal](err); ok {
		actor := r.Actor
		if !actor.Valid() {
			actor = ByYou
		}
		return &Failure{Step: step.Verb, Said: r.Said, Remedy: r.Remedy, Actor: actor, ActionURL: r.ActionURL}
	}
	where := "the vendor"
	if lane != "" {
		where = LaneName(lane)
	}
	return &Failure{
		Step: step.Verb, Actor: ByCrewlet,
		Said: err.Error(),
		Remedy: fmt.Sprintf("Crewlet tries again on the next sync; if %s keeps failing, "+
			"choose Retry once it answers", where),
	}
}

// observedFor is what the vendor showed for each lane, with the ledger's
// half filled in: the account's identity, the sealed credential, who disabled
// it (D3) and when it was changed by hand (D2).
func observedFor(lanes []Lane, got map[Lane]Observed, rec Record, now time.Time) map[Lane]Observed {
	out := make(map[Lane]Observed, len(lanes))
	for _, lane := range lanes {
		o := got[lane]
		if !o.Exists {
			out[lane] = o
			continue
		}
		if rec.Account != nil {
			o.AccountID = firstNonEmpty(o.AccountID, rec.Account.ID)
			o.Account = firstNonEmpty(o.Account, rec.Account.Name)
			if o.CreatedAt.IsZero() {
				o.CreatedAt = rec.Account.CreatedAt
			}
		}
		if o.Credential == nil {
			o.Credential = rec.Credential
		}
		prior := rec.Lanes[lane]
		applied := prior.Applied
		if !o.Enabled && o.DisabledBy == "" {
			switch {
			case rec.Account != nil && rec.Account.State == AccountDisabled,
				applied != nil && !applied.Enabled:
				o.DisabledBy = DisabledByCrewlet
			case applied != nil && applied.Enabled:
				o.DisabledBy = DisabledByHand
			}
		}
		if o.Enabled && applied != nil && applied.Enabled && !applied.SameAccess(o.Access()) {
			o.ChangedByHandAt = prior.Observed.ChangedByHandAt
			if o.ChangedByHandAt.IsZero() {
				o.ChangedByHandAt = now
			}
		}
		out[lane] = o
	}
	return out
}

func anyExists(have map[Lane]Observed) bool {
	for _, o := range have {
		if o.Exists {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// LaneName is a lane as its vendor names itself, for a sentence.
func LaneName(lane Lane) string {
	switch lane {
	case LaneDatadog:
		return "Datadog"
	case LaneGitHub:
		return "GitHub"
	case LaneGitLab:
		return "GitLab"
	case LaneJira:
		return "Jira"
	case LaneConfluence:
		return "Confluence"
	case LaneSlack:
		return "Slack"
	case LaneMattermost:
		return "Mattermost"
	default:
		return string(lane)
	}
}
