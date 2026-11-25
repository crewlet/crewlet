package seataccess

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
)

// identities keeps, gives up and checks each seat identity of rec's lanes whose
// vendor offers one, as a person asked and the document says: an identity
// asked for is checked and kept, one whose seat turned off or back to the
// company's is given up, and every kept one is read back each pass. A dry run
// (no sink) only reads back. An error is a fault the kind shares.
func (s *syncer) identities(ctx context.Context, rec *Record, ref SeatRef, want map[Lane]Desired) error {
	maker, ok := s.sess.(IdentityMaker)
	if !ok {
		return nil
	}
	for _, lane := range LanesOf(s.kind) {
		spec, ok := s.cat.Lane(lane)
		if !ok || !spec.Identity.Offered() {
			continue
		}
		lr := rec.Lanes[lane]
		d := want[lane]
		var ask *IdentityAsk
		if a := s.in.Asks[ref.Handle]; a.Identity != nil && a.Lane == lane {
			ask = a.Identity
		}
		in := IdentityInput{Seat: ref, Lane: lane, Want: d, Record: lr.Identity, Records: s.records,
			Sinks: identitySinks(s.kind, spec.Identity, ref.Handle, lr.Identity)}
		own := d.Enabled && !s.in.Disconnect && spec.Identity.ModeOf(d) == IdentityOwn
		standing := spec.Identity.Standing
		var err error
		switch {
		case standing && own:
			// A DRY RUN READS ONLY: the identity as a person added it, never an add.
			in.Way = WayInvited
			if ask != nil && ask.Keep && s.in.Sink != nil {
				in.Way = ask.Way
			}
			lr.Identity, err = s.standIdentity(ctx, maker, in)
		case s.in.Sink == nil:
			lr.Identity, err = s.checkIdentity(ctx, maker, spec.Identity, in, rec.Handle)
		case standing && lr.Identity != nil && (lr.Identity.Kept() || lr.Identity.Attempt != nil):
			in.Want.Scope = scopeHeld(d, lr.Identity)
			lr.Identity, err = s.dropStanding(ctx, maker, in)
		case ask != nil && ask.Drop, lr.Identity.Kept() && !own:
			lr.Identity, err = s.dropIdentity(ctx, maker, spec.Identity, in, rec.Handle)
		case ask != nil && ask.Keep:
			in.Way, in.Value, in.FileName, in.ID, in.Detail = ask.Way, ask.Value, ask.FileName, ask.ID, ask.Detail
			lr.Identity, err = s.keepIdentity(ctx, maker, spec.Identity, in, rec.Handle)
		default:
			var unretired error
			if refresher, ok := maker.(IdentityRefresher); ok && own && lr.Identity.Kept() {
				lr.Identity, unretired, err = s.refreshIdentity(ctx, refresher, spec.Identity, in, rec.Handle)
				in.Record = lr.Identity
			}
			if err == nil {
				lr.Identity, err = s.checkIdentity(ctx, maker, spec.Identity, in, rec.Handle)
			}
			if unretired != nil && lr.Identity.Kept() && lr.Identity.Check != nil {
				lr.Identity.Check.Said = strings.TrimSpace(lr.Identity.Check.Said + " Crewlet could not revoke the " +
					"credential it replaced (" + unretired.Error() + "); it ends on its own.")
			}
		}
		switch {
		case err != nil && ask == nil:
			// ONE SEAT'S READ-BACK IS ONE SEAT'S FAULT: its record stands as it
			// was, the integration says why, and the next pass reads it again.
			s.report.Findings = append(s.report.Findings, s.seatFinding(integration.FindingIdentityFailed,
				ref.Handle, lane, &Failure{Step: VerbObserve, Actor: ByCrewlet,
					Said:   "Crewlet could not read the agent's own identity back: " + err.Error(),
					Remedy: "The next pass reads it again"}, false))
			continue
		case err != nil:
			return fmt.Errorf("seataccess: %s identity for %s: %w", lane, ref.Handle, err)
		}
		rec.Lanes[lane] = lr
	}
	return nil
}

// identitySinks is every sealed name of the seat's identity on a lane, by
// field: its credential's, and each retiring identity's ([RetiringField]).
func identitySinks(kind integration.Kind, spec IdentitySpec, handle string, rec *IdentityRecord) map[string]string {
	out := map[string]string{}
	if spec.Credential != "" {
		out[spec.Credential] = SinkName(kind, spec.Credential, handle)
	}
	if rec != nil {
		for _, r := range rec.Retiring {
			if field := RetiringField(spec, r.ID); field != "" {
				out[field] = SinkName(kind, field, handle)
			}
		}
	}
	return out
}

// checkIdentity reads back the kept identity, and each one it replaced while a
// person has still to revoke it. Nothing kept and nothing retiring reads
// nothing.
func (s *syncer) checkIdentity(ctx context.Context, maker IdentityMaker, spec IdentitySpec, in IdentityInput,
	handle string) (*IdentityRecord, error) {
	rec := in.Record
	if !rec.Kept() && (rec == nil || len(rec.Retiring) == 0) {
		return rec, nil
	}
	check, err := maker.CheckIdentity(ctx, in)
	if err != nil {
		return rec, err
	}
	out := *rec
	check.At = s.now
	if rec.Kept() {
		out.Check = &check
	}
	// A DRY RUN CLEARS NOTHING: the next writing pass reads it again.
	if len(check.RetiringGone) > 0 && s.in.Sink != nil {
		out.Retiring = nil
		for _, r := range rec.Retiring {
			if !slices.Contains(check.RetiringGone, r.ID) {
				out.Retiring = append(out.Retiring, r)
				continue
			}
			if err := s.clear(ctx, s.sinkOf(spec, r.ID, handle), handle); err != nil {
				return rec, err
			}
		}
	}
	return &out, nil
}

// sinkOf is where a retiring identity's credential is sealed, "" for none.
func (s *syncer) sinkOf(spec IdentitySpec, id, handle string) string {
	if field := RetiringField(spec, id); field != "" {
		return SinkName(s.kind, field, handle)
	}
	return ""
}

// sealIdentity seals what a keep or a drop minted under the seat's identity
// names: each retiring identity's first, then the credential, so a failure
// leaves the identity the record still names sealed as it was. It answers the
// credential's sealed name, "" where none was minted.
func (s *syncer) sealIdentity(ctx context.Context, spec IdentitySpec, minted []Minted, handle string) (string, error) {
	ordered := slices.Clone(minted)
	slices.SortStableFunc(ordered, func(a, b Minted) int {
		return boolRank(a.Field == spec.Credential) - boolRank(b.Field == spec.Credential)
	})
	var credential string
	for _, m := range ordered {
		retiring := spec.Retiring != "" && strings.HasPrefix(m.Field, spec.Retiring+"_")
		if m.Field == "" || (m.Field != spec.Credential && !retiring) {
			continue
		}
		name := SinkName(s.kind, m.Field, handle)
		if s.refs.OutsideSeat(name, handle) > 0 {
			return "", fmt.Errorf("%s is named elsewhere in the company document", name)
		}
		if err := s.in.Sink.Record(ctx, name, m.Value); err != nil {
			return "", fmt.Errorf("seal %s: %w", name, err)
		}
		if m.Field == spec.Credential {
			credential = name
		}
	}
	return credential, nil
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// refreshIdentity seals what a refresh minted over the kept credential, then
// revokes the one it replaced. A revoke that fails is answered apart, for the
// check's words: the new credential is sealed and works either way.
func (s *syncer) refreshIdentity(ctx context.Context, r IdentityRefresher, spec IdentitySpec, in IdentityInput,
	handle string) (rec *IdentityRecord, unretired, err error) {

	got, err := r.RefreshIdentity(ctx, in)
	if err != nil {
		return in.Record, nil, err
	}
	if len(got.Minted) == 0 || spec.Credential == "" {
		return in.Record, nil, s.revokeMinted(ctx, got.Minted)
	}
	sealed, err := s.sealIdentity(ctx, spec, got.Minted, handle)
	if err != nil {
		return in.Record, nil, errors.Join(err, s.revokeMinted(ctx, got.Minted))
	}
	out := *in.Record
	if sealed != "" {
		out.Credential = &Credential{Variable: sealed, MintedAt: s.now, By: s.in.By, Source: CredentialMinted}
	}
	if got.Retire != nil {
		unretired = got.Retire(context.WithoutCancel(ctx))
	}
	return &out, unretired, nil
}

// keepIdentity checks what a person handed in, invited or asked Crewlet to
// make, and keeps it only where the check finds it ready: sealed at the
// seat's engine-owned name, and the identity it replaces left for a person to
// revoke. Anything else is the record's attempt and is kept nowhere.
func (s *syncer) keepIdentity(ctx context.Context, maker IdentityMaker, spec IdentitySpec, in IdentityInput,
	handle string) (*IdentityRecord, error) {

	var out IdentityRecord
	if in.Record != nil {
		out = *in.Record
	}
	kept, err := maker.KeepIdentity(ctx, in)
	if r, ok := RefusalOf(err); ok {
		out.Attempt = &IdentityCheck{Status: IdentityRefused, Said: r.Said, Remedy: r.Remedy, At: s.now}
		return &out, nil
	}
	if err != nil {
		return in.Record, err
	}
	kept.Check.At = s.now
	if !kept.Check.Status.Keeps() {
		check := kept.Check
		out.Attempt = unrevoked(&check, s.revokeMinted(ctx, kept.Minted))
		return &out, nil
	}
	if sink := in.Sinks[spec.Credential]; sink != "" && s.refs.OutsideSeat(sink, handle) > 0 {
		out.Attempt = unrevoked(&IdentityCheck{Status: IdentityRefused, At: s.now,
			Said: sink + " is named elsewhere in the company document, so a credential sealed there " +
				"would be read by something other than this agent.",
			Remedy: "Remove every reference to " + sink + " from the company document."},
			s.revokeMinted(ctx, kept.Minted))
		return &out, nil
	}
	sealed, err := s.sealIdentity(ctx, spec, kept.Minted, handle)
	if err != nil {
		return in.Record, errors.Join(err, s.revokeMinted(ctx, kept.Minted))
	}
	var cred *Credential
	if sealed != "" {
		cred = &Credential{Variable: sealed, MintedAt: s.now, By: s.in.By, Source: CredentialExternal}
		if in.Way == WayMade {
			cred.Source = CredentialMinted
		}
	}
	check := kept.Check
	var unretired error
	if kept.Retire != nil && in.Record.Kept() {
		// THE NEW ONE IS SEALED: only now may what it replaced stop working.
		unretired = kept.Retire(context.WithoutCancel(ctx))
		if unretired != nil {
			check.Said = strings.TrimSpace(check.Said + " Crewlet could not revoke what it replaced (" +
				unretired.Error() + ").")
		}
	}
	switch {
	case in.Record.Kept() && in.Record.ID != kept.ID && (kept.Retire == nil || unretired != nil):
		out.Retiring = append(slices.Clone(out.Retiring),
			RetiringIdentity{ID: in.Record.ID, Name: in.Record.Name, Since: in.Record.Since})
	case in.Record.Kept() && in.Record.ID != kept.ID:
		// NOTHING RETIRES: what the lane sealed to revoke on a later pass is dead.
		if err := s.clear(ctx, s.sinkOf(spec, in.Record.ID, handle), handle); err != nil {
			return in.Record, err
		}
	}
	out.Way, out.ID, out.Name, out.Since, out.Detail = kept.Way, kept.ID, kept.Name, s.now, kept.Detail
	out.Credential, out.Check, out.Attempt = cred, &check, nil
	return &out, nil
}

// dropIdentity gives up the kept identity: the vendor undoes what it can,
// the sealed credential is cleared, and an identity only a person can revoke
// stays as retiring until the vendor refuses it.
func (s *syncer) dropIdentity(ctx context.Context, maker IdentityMaker, spec IdentitySpec, in IdentityInput,
	handle string) (*IdentityRecord, error) {

	if !in.Record.Kept() {
		return in.Record, nil
	}
	dropped, err := maker.DropIdentity(ctx, in)
	if err != nil {
		return in.Record, err
	}
	out := &IdentityRecord{Retiring: slices.Clone(in.Record.Retiring)}
	if dropped.Waiting != nil {
		out.Retiring = append(out.Retiring, RetiringIdentity{ID: in.Record.ID, Name: in.Record.Name,
			Since: in.Record.Since})
	}
	if _, err := s.sealIdentity(ctx, spec, dropped.Minted, handle); err != nil {
		return in.Record, err
	}
	if err := s.clear(ctx, in.Sinks[spec.Credential], handle); err != nil {
		return in.Record, err
	}
	return out, nil
}

// clear forgets one sealed name of the seat's, never one something outside
// the seat names; "" clears nothing.
func (s *syncer) clear(ctx context.Context, name, handle string) error {
	if name == "" || s.in.Sink == nil {
		return nil
	}
	if names := Forgettable([]string{name}, handle, s.refs); len(names) > 0 {
		if err := s.in.Sink.Forget(ctx, names...); err != nil {
			return fmt.Errorf("clear %s: %w", name, err)
		}
	}
	return nil
}

// standIdentity checks a standing identity, after adding it at the vendor
// where a person asked Crewlet to (in.Way), and keeps it exactly while it
// passes: one that fails is the record's attempt, so the box says what to do
// and the seat Needs you, never Refused, since it is still the seat's own.
func (s *syncer) standIdentity(ctx context.Context, maker IdentityMaker, in IdentityInput) (*IdentityRecord, error) {
	kept, err := maker.KeepIdentity(ctx, in)
	if r, ok := RefusalOf(err); ok {
		out := &IdentityRecord{Attempt: &IdentityCheck{Status: IdentityRefused, Said: r.Said, Remedy: r.Remedy,
			At: s.now}, Scope: slices.Clone(in.Want.Scope)}
		if in.Record.Kept() {
			// A REFUSED ADD LEAVES WHAT WORKED: the last check still stands.
			kept := *in.Record
			kept.Attempt = out.Attempt
			return &kept, nil
		}
		return out, nil
	}
	if err != nil {
		return in.Record, err
	}
	check := kept.Check
	check.At = s.now
	scope := slices.Clone(in.Want.Scope)
	if !check.Status.Keeps() {
		return &IdentityRecord{Attempt: &check, Scope: scope}, nil
	}
	out := &IdentityRecord{Way: kept.Way, ID: kept.ID, Name: kept.Name, Since: s.now, Check: &check, Scope: scope}
	if prior := in.Record; prior.Kept() && prior.ID == kept.ID {
		out.Since = prior.Since
		if kept.Way == WayInvited {
			out.Way = prior.Way
		}
	}
	return out, nil
}

// scopeHeld is every entry a standing identity may reach: the document's,
// and what it was last checked against, so a seat that left the document or
// a disconnect still names what to take it off.
func scopeHeld(d Desired, rec *IdentityRecord) []string {
	out := append(slices.Clone(d.Scope), rec.Scope...)
	slices.Sort(out)
	return slices.Compact(out)
}

// dropStanding gives up a standing identity as its seat turns off: the
// vendor takes it off what it can, and the record is cleared.
func (s *syncer) dropStanding(ctx context.Context, maker IdentityMaker, in IdentityInput) (*IdentityRecord, error) {
	if _, err := maker.DropIdentity(ctx, in); err != nil {
		return in.Record, err
	}
	return nil, nil
}

// revokeMinted undoes what a keep minted and is not keeping, with a context
// that outlives the pass's, and answers what it could not undo.
func (s *syncer) revokeMinted(ctx context.Context, minted []Minted) error {
	cleanup := context.WithoutCancel(ctx)
	var errs []error
	for _, m := range minted {
		if m.Revoke == nil {
			continue
		}
		if err := m.Revoke(cleanup); err != nil {
			errs = append(errs, fmt.Errorf("revoke the %s it made: %w", m.Field, err))
		}
	}
	return errors.Join(errs...)
}

// unrevoked is attempt with what a keep could not undo at the vendor said.
func unrevoked(attempt *IdentityCheck, err error) *IdentityCheck {
	if err != nil {
		attempt.Said += " Crewlet could not undo what it made at the vendor (" + err.Error() +
			"); remove it there."
	}
	return attempt
}
