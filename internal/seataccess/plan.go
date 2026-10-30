package seataccess

// THE ONE PLAN FUNCTION.
//
// The preview route and the sync both run [Plan], so a preview cannot promise
// what a sync will not do. The sync runs it lane by lane with the vendor's
// real answers fed back in ([walk]); the preview runs it with each lane's
// effect simulated. Either way the decisions are the ones below, made once for
// every vendor:
//
//   - A remove is refused while any lane of the kind is on (D1).
//   - Off disables an enabled account and keeps it (D1), and once every lane
//     of the kind is off its credential is revoked. A seat that left the
//     document is off.
//   - On with no account creates it; a later lane of the same kind then sees
//     the account the earlier one made (Atlassian's two products).
//   - An account somebody disabled at the vendor stays off until a person
//     asks to turn it back on (D3); one this engine disabled is turned back
//     on with its seat.
//   - A level or scope that differs from the document is moved to it; where
//     the document has not changed since the last converged sync, that is
//     drift, restored and reported (D2).
//   - A credential is minted where none is sealed, and rotated when asked,
//     once per account rather than once per lane.

// SeatInput is one seat at one kind, as a plan reads it.
type SeatInput struct {
	Seat SeatRef

	// Want is the document's setting per lane; a lane it leaves out is off.
	Want map[Lane]Desired

	// Have is what the vendor holds per lane; a lane it leaves out has no
	// account.
	Have map[Lane]Observed

	// Record is the ledger's record for the seat, nil where it has none.
	Record *Record

	// Ask is what a person asked for on this seat, if anything.
	Ask Ask
}

// Plan is every step the next sync takes for one seat at the vendor, by lane,
// in the order they are taken. A lane that needs nothing has no entry.
func Plan(v Vendor, in SeatInput) (map[Lane][]Step, error) {
	out := map[Lane][]Step{}
	err := walk(v, in, func(lane Lane, _ Desired, _ Observed, steps []Step) laneEffect {
		if len(steps) > 0 {
			out[lane] = steps
		}
		return simulated(steps)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// laneEffect is what taking one lane's steps did to the ACCOUNT, which the
// kind's later lanes share: whether it now exists, whether a credential is now
// sealed for it, and whether it was removed.
type laneEffect struct {
	created    bool
	credential *Credential
	removed    bool
	revoked    bool
}

// simulated is the effect a lane's steps would have if every one succeeded,
// for a preview.
func simulated(steps []Step) laneEffect {
	var eff laneEffect
	for _, step := range steps {
		switch step.Op {
		case OpCreate:
			eff.created = true
			eff.credential = &Credential{}
		case OpEnable, OpMint, OpRotate:
			eff.credential = &Credential{}
		case OpRemove:
			eff.removed = true
		case OpRevoke:
			eff.revoked = true
		}
	}
	return eff
}

// walk plans each lane of the seat's kind in order and hands its steps to
// take, which takes them (a sync) or not (a preview) and says what they did
// to the account, so the next lane is planned against it.
func walk(
	v Vendor, in SeatInput,
	take func(lane Lane, want Desired, have Observed, steps []Step) laneEffect,
) error {
	cat := v.Catalog()
	lanes := LanesOf(cat.Kind)
	if in.Ask.Remove {
		for _, lane := range lanes {
			if in.Want[lane].Enabled {
				return ErrStillOn
			}
		}
	}
	// THE LAST LANE THAT HOLDS THE ACCOUNT, where a revoke goes once every
	// lane is off, so it follows each lane's own disable.
	allOff, last := !in.Ask.Remove, -1
	for i, lane := range lanes {
		allOff = allOff && !in.Want[lane].Enabled
		if in.Have[lane].Exists {
			last = i
		}
	}
	var (
		created, removed                     bool
		minted, rotated, removeDone, revoked bool
		cred                                 *Credential
	)
	if in.Record != nil {
		cred = in.Record.Credential
	}
	// A CREDENTIAL THE VENDOR PROVES GONE is not one the seat holds,
	// whatever the ledger recorded sealing: the account's credential is
	// one per account, so any lane's proof speaks for all of them.
	gone := false
	for _, lane := range lanes {
		gone = gone || in.Have[lane].CredentialGone
	}
	for i, lane := range lanes {
		want := in.Want[lane].Normalized()
		have := in.Have[lane]
		switch {
		case removed:
			have = Observed{}
		case created && !have.Exists:
			// AN EARLIER LANE MADE THE ACCOUNT in this same sync: it
			// exists, and this lane has not been granted on it yet.
			have.Exists, have.Enabled, have.DisabledBy = true, false, ""
		}
		if have.Exists && have.Credential == nil && !gone {
			have.Credential = cred
		}
		if gone {
			have.Credential = nil
		}
		var applied *Desired
		if in.Record != nil {
			if rec, ok := in.Record.Lanes[lane]; ok {
				applied = rec.Applied
			}
		}
		var ops []planned
		switch {
		case in.Ask.Remove:
			if have.Exists && !removeDone {
				ops = append(ops, planned{op: OpRemove})
				removeDone = true
			}
		case !want.Enabled:
			if have.Exists && have.Enabled {
				ops = append(ops, planned{op: OpDisable})
			}
			if allOff && i == last && cred != nil && !revoked {
				ops = append(ops, planned{op: OpRevoke})
				revoked = true
			}
		case !have.Exists:
			ops = append(ops, planned{op: OpCreate})
			minted = true
		default:
			ops = onAccount(want, have, applied, in.Ask, lane)
			for _, p := range ops {
				if p.op == OpEnable {
					minted = true
				}
			}
			enabled := have.Enabled || len(ops) > 0 && ops[0].op == OpEnable
			if enabled && (cred == nil || gone) && !minted {
				ops = append(ops, planned{op: OpMint})
				minted = true
			}
			if in.Ask.Rotate && !rotated && have.Enabled && cred != nil && !gone {
				ops = append(ops, planned{op: OpRotate})
				rotated = true
			}
		}
		var steps []Step
		for _, p := range ops {
			got := v.Steps(in.Seat, lane, p.op, want, have)
			for i := range got {
				got[i].Op, got[i].Index = p.op, i
				got[i].Drift = got[i].Drift || p.drift
			}
			steps = append(steps, got...)
		}
		eff := take(lane, want, have, steps)
		created = created || eff.created
		if eff.removed {
			removed, cred = true, nil
		}
		if eff.revoked {
			cred = nil
		}
		if eff.credential != nil {
			cred, gone = eff.credential, false
		}
	}
	return nil
}

// planned is one op the rules chose, and whether it restores drift.
type planned struct {
	op    Op
	drift bool
}

// onAccount is what a lane that is on needs from an account that exists.
func onAccount(want Desired, have Observed, applied *Desired, ask Ask, lane Lane) []planned {
	var ops []planned
	if !have.Enabled {
		reenable := ask.Reenable && (ask.Lane == "" || ask.Lane == lane)
		if have.DisabledBy == DisabledByHand && !reenable {
			// D3: somebody turned it off at the vendor, possibly as
			// incident response. Nothing else is planned on an account
			// nobody may use: the tab offers Turn back on or Keep off.
			return nil
		}
		ops = append(ops, planned{op: OpEnable})
	}
	now := have.Access()
	if !want.SameLevel(now) {
		ops = append(ops, planned{op: OpLevel, drift: unchanged(applied, want, Desired.SameLevel)})
	}
	if !want.SameScope(now) {
		ops = append(ops, planned{op: OpScope, drift: unchanged(applied, want, Desired.SameScope)})
	}
	return ops
}

// unchanged reports a document that still asks for what the last converged
// sync applied, so a difference at the vendor is somebody else's change (D2).
func unchanged(applied *Desired, want Desired, same func(Desired, Desired) bool) bool {
	return applied != nil && applied.Enabled && same(*applied, want)
}
