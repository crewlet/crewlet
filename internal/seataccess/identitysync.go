package seataccess

import (
	"context"
	"errors"
	"fmt"
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
			Sinks: map[string]string{}}
		if sink := IdentitySink(s.kind, spec.Identity, ref.Handle); sink != "" {
			in.Sinks[spec.Identity.Credential] = sink
		}
		own := d.Enabled && !s.in.Disconnect && spec.Identity.ModeOf(d) == IdentityOwn
		var err error
		switch {
		case s.in.Sink == nil:
			lr.Identity, err = s.checkIdentity(ctx, maker, in)
		case ask != nil && ask.Drop, lr.Identity.Kept() && !own:
			lr.Identity, err = s.dropIdentity(ctx, maker, in, rec.Handle)
		case ask != nil && ask.Keep:
			in.Way, in.Value, in.FileName = ask.Way, ask.Value, ask.FileName
			lr.Identity, err = s.keepIdentity(ctx, maker, spec.Identity, in, rec.Handle)
		default:
			lr.Identity, err = s.checkIdentity(ctx, maker, in)
		}
		if err != nil {
			return fmt.Errorf("seataccess: %s identity for %s: %w", lane, ref.Handle, err)
		}
		rec.Lanes[lane] = lr
	}
	return nil
}

// checkIdentity reads back the kept identity, and the one it replaced while a
// person has still to revoke it. Nothing kept and nothing retiring reads
// nothing.
func (s *syncer) checkIdentity(ctx context.Context, maker IdentityMaker, in IdentityInput) (*IdentityRecord, error) {
	rec := in.Record
	if !rec.Kept() && (rec == nil || rec.Retiring == nil) {
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
	if check.RetiringGone {
		out.Retiring = nil
	}
	return &out, nil
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
	if r, ok := errors.AsType[*Refusal](err); ok {
		out.Attempt = &IdentityCheck{Status: IdentityRefused, Said: r.Said, Remedy: r.Remedy, At: s.now}
		return &out, nil
	}
	if err != nil {
		return in.Record, err
	}
	kept.Check.At = s.now
	if kept.Check.Status != IdentityReady {
		check := kept.Check
		out.Attempt = unrevoked(&check, s.revokeMinted(ctx, kept.Minted))
		return &out, nil
	}
	var cred *Credential
	if sink := in.Sinks[spec.Credential]; sink != "" {
		if s.refs.OutsideSeat(sink, handle) > 0 {
			out.Attempt = unrevoked(&IdentityCheck{Status: IdentityRefused, At: s.now,
				Said: sink + " is named elsewhere in the company document, so a credential sealed there " +
					"would be read by something other than this agent.",
				Remedy: "Remove every reference to " + sink + " from the company document."},
				s.revokeMinted(ctx, kept.Minted))
			return &out, nil
		}
		for _, m := range kept.Minted {
			if m.Field != spec.Credential {
				continue
			}
			if err := s.in.Sink.Record(ctx, sink, m.Value); err != nil {
				return in.Record, errors.Join(fmt.Errorf("seal %s: %w", sink, err), s.revokeMinted(ctx, kept.Minted))
			}
			cred = &Credential{Variable: sink, MintedAt: s.now, By: s.in.By, Source: CredentialExternal}
			if in.Way == WayMade {
				cred.Source = CredentialMinted
			}
		}
	}
	if in.Record.Kept() && in.Record.ID != kept.ID {
		out.Retiring = &RetiringIdentity{ID: in.Record.ID, Name: in.Record.Name, Since: in.Record.Since}
	}
	check := kept.Check
	out.Way, out.ID, out.Name, out.Since = kept.Way, kept.ID, kept.Name, s.now
	out.Credential, out.Check, out.Attempt = cred, &check, nil
	return &out, nil
}

// dropIdentity gives up the kept identity: the vendor undoes what it can,
// the sealed credential is cleared, and an identity only a person can revoke
// stays as retiring until the vendor refuses it.
func (s *syncer) dropIdentity(ctx context.Context, maker IdentityMaker, in IdentityInput,
	handle string) (*IdentityRecord, error) {

	if !in.Record.Kept() {
		return in.Record, nil
	}
	dropped, err := maker.DropIdentity(ctx, in)
	if err != nil {
		return in.Record, err
	}
	var names []string
	for _, name := range in.Sinks {
		names = append(names, name)
	}
	if names = Forgettable(names, handle, s.refs); len(names) > 0 {
		if err := s.in.Sink.Forget(ctx, names...); err != nil {
			return in.Record, fmt.Errorf("clear %v: %w", names, err)
		}
	}
	out := &IdentityRecord{Retiring: in.Record.Retiring}
	if dropped.Waiting != nil {
		out.Retiring = &RetiringIdentity{ID: in.Record.ID, Name: in.Record.Name, Since: in.Record.Since}
	}
	return out, nil
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
