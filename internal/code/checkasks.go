package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// A PRINCIPAL'S CHECK WRITES: a rerun a seat or a person asks for, and a
// person's own CI reporting a check of a commit — the two writes of a
// repository's checks somebody other than the engine asks for, each decided by
// the repository's checks writer ([ChecksWriter]) beside the engine's own, in
// the same single flight and from one snapshot, and each judged by its
// asker's standing there, from the deciding node's chart.
//
// # A rerun runs the definition its latest attempt ran under, again
//
// A rerun queues the attempt after the latest its asker read ([CheckRerun]:
// expected attempt + 1), copying that attempt's queue — its purpose, its
// change, its definition, whether it is required — so it runs what the latest
// ran and nothing a later checks file says: the home's dispatch reads the
// target's file for a NEW head, and a rerun is the same check of the same
// commit again. Decided by the attempt arithmetic every queue is (one attempt
// at a time, none past [MaxCheckAttempts], the attempt queued already
// answered from its row), and by WHO ASKS:
//
//   - A SEAT reruns only an attempt its infrastructure or a stop ended,
//     `errored` or `cancelled` (M5-D23, "meant for an infrastructure error")
//     — a measurement, `passed` or `failed`, is refused `rerun_genuine`: an
//     agent re-rolling a red check until it goes green is how a flaky test
//     reaches a protected branch — and only on a repository its grant may
//     write ([AccessWrite]) or for a change it is an author of: whoever may
//     move the branch, or whose change the check judges. In a turn somebody
//     woke, whoever that is is held at read on the repository — the least
//     access at which this decide may admit a rerun, an author's, as every
//     change write's requester is held ([WriteCeiling.changeRefusal]) — and a
//     turn that works for nobody reruns only a check of a change the seat
//     wrote, or of a repository it owns.
//   - A PERSON reruns any ended attempt: a person maintains every
//     repository (ADR-0055), and the person is who judges a flake.
//   - A credential bound to nobody writes nothing: it reads only (contract
//     §1.8).
//
// A check a person's CI reports is no rerun's: that CI runs it again and
// reports the attempt after ([CheckRefusalNativeCheck]).
//
// # A report is the attempt's claim or its ending, by a person
//
// A person's own CI reports a check of a commit for a target onto the same
// rows the engine's runners write ([CheckReported]) — `running` as the
// attempt's claim, placement `external`; an ending as its ending, reason
// `reported`, naming the person — and the join inserts the row where none is
// held, as the subject's apply states: a report QUEUES NOTHING, since no
// runner of the engine's claims it. Only a person reports — through a
// credential bound to their human seat; a seat runs no CI of its own, and a
// credential bound to nobody reads only — and never a check the engine has
// queued at that commit for that target ([CheckRefusalNativeCheck]): a check
// of a commit is one runner's or one CI's, so a report cannot overwrite what a
// runner measured, nor a runner what a CI said — the home's dispatch queuing
// nothing of a name the commit already holds an attempt of, a CI's included.
//
// The attempt a report names is its CI's run: the latest attempt while that
// one runs, the next once it ended — so a CI that runs a check again
// reports it as a new attempt, and the latest attempt's own ending, sent
// again, is answered from the row.
//
// # Record format
//
// Neither arm is a record of its own: each decides one of the subject's
// existing records (check_queued, check_started, check_finished), whose
// fields are in the base format under the condition the subject states. The
// requests are estate requests, and take no versionedFields row.

// CheckRerun asks for the attempt after the latest its asker read of a check
// of a commit for its target — the key's Attempt is the one queued, the
// latest it read plus one — on behalf of its request's asker
// ([ChecksRequest.Asker]).
type CheckRerun struct {
	CheckKey
	// Ceiling is whoever the seat's turn works for, which the rerun is held
	// to as well ([WriteCeiling]); nil for a seat's own and every person's.
	Ceiling *WriteCeiling `json:"ceiling,omitempty"`
}

// validate refuses a rerun by asker no correct asker forms.
func (r CheckRerun) validate(asker Principal) error {
	key := r.CheckKey
	if key.Attempt > MaxCheckAttempts {
		// AN ATTEMPT PAST THE LAST is judged against the rows, which refuse
		// it attempts_exhausted.
		key.Attempt = MaxCheckAttempts
	}
	if err := key.validateAs("rerun."); err != nil {
		return err
	}
	if r.Attempt < 2 {
		return checkField("rerun.attempt", "%d: a rerun follows an attempt that ended, so it queues the "+
			"second at the earliest", r.Attempt)
	}
	if _, kind := asker.party(); kind == "" {
		return checkField("asker", "a rerun is asked by a seat, or by a person through a credential bound "+
			"to their human seat; a credential bound to nobody reads only")
	}
	if err := asker.Validate(); err != nil {
		return fmt.Errorf("asker: %w", err)
	}
	if r.Ceiling != nil {
		if why := r.Ceiling.invalid(asker); why != "" {
			return fmt.Errorf("%w: rerun.%s", ErrInvalid, why)
		}
	}
	return nil
}

// CheckReported is a person's CI reporting one attempt of a check of a commit
// for its target: State `running`, or the ending it reached.
type CheckReported struct {
	CheckKey
	// State is running, passed, failed, errored or cancelled.
	State CheckState `json:"state"`
	// Summary, Tail, Failures and FailuresOmitted are what an ending says,
	// each held to the bound a runner's is ([CheckFinished]); a passed run
	// says its summary alone, and a running one nothing.
	Summary         string         `json:"summary,omitempty"`
	Tail            string         `json:"tail,omitempty"`
	Failures        []CheckFailure `json:"failures,omitempty"`
	FailuresOmitted int            `json:"failures_omitted,omitempty"`
	// DurationMs is how long the CI's run took, where it says.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// URL is the CI's own page of the run, at most MaxCheckURLBytes.
	URL string `json:"url,omitempty"`
}

// record is the record r, reported by reporter, decides at instant at: the
// attempt's claim while it runs, its ending once it ended.
func (r CheckReported) record(reporter Principal, at time.Time) (OpKind, any) {
	if r.State == CheckStateRunning {
		return OpCheckStarted, CheckStarted{CheckKey: r.CheckKey, Placement: PlacementExternal, StartedAt: at}
	}
	return OpCheckFinished, CheckFinished{CheckKey: r.CheckKey, State: r.State, Reason: CheckReasonReported,
		DurationMs: r.DurationMs, Summary: r.Summary, Tail: r.Tail, Failures: r.Failures,
		FailuresOmitted: r.FailuresOmitted, Reporter: &reporter, URL: r.URL, FinishedAt: at}
}

// validate refuses a report by reporter no correct asker forms: its
// record's own rules, at any instant, and a reporter who is no person.
func (r CheckReported) validate(reporter Principal) error {
	switch {
	case reporter.Kind != PrincipalOperator || reporter.Seat == "":
		return checkField("asker", "a check is reported by a person, through a credential bound to "+
			"their human seat: a seat runs no CI of its own, and a credential bound to nobody reads only")
	case r.State != CheckStateRunning && (!r.State.Ended() || r.State == CheckStateSkipped):
		return checkField("report.state", "%q is not one a CI reports: running, passed, failed, errored or "+
			"cancelled", r.State)
	case r.State == CheckStateRunning && (r.Summary != "" || r.Tail != "" || len(r.Failures) > 0 ||
		r.FailuresOmitted > 0 || r.DurationMs != 0):
		return checkField("report.summary", "a running check says nothing yet: its ending carries what it said")
	}
	if err := reporter.Validate(); err != nil {
		return fmt.Errorf("asker: %w", err)
	}
	if r.Attempt > MaxCheckAttempts {
		// AN ATTEMPT PAST THE LAST is judged against the rows, which refuse
		// it attempts_exhausted; its other fields are held to the record's
		// rules at the last.
		r.Attempt = MaxCheckAttempts
	}
	_, payload := r.record(reporter, time.Unix(1, 0).UTC())
	if err := payload.(interface{ Validate() error }).Validate(); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	return nil
}

// decideRerun is a rerun's decide — see the file's doc — over the snapshot
// tx, for repository repo, at instant now.
func decideRerun(ctx context.Context, tx *sql.Tx, r CheckRerun, asker Principal, repo pushRepo, seats SeatRules,
	d checksDecision, named func(CheckKey) string, now time.Time) (checksDecision, error) {

	key := r.CheckKey
	prev := key
	prev.Attempt--
	if key.Attempt <= MaxCheckAttempts {
		row, held, err := checkRowIn(ctx, tx, key)
		if err != nil {
			return checksDecision{}, err
		}
		if held {
			d.row = &row
			if row.Queued.IsZero() {
				return d.refuse(CheckRefusalNativeCheck, fmt.Sprintf("%s is a person's CI's, which runs it "+
					"again and reports it; a rerun queues a check the engine runs", named(key))), nil
			}
			// THE ATTEMPT IS QUEUED ALREADY — another rerun of the same
			// latest landed first, or this one asked again: answered from
			// its row.
			d.unchanged = true
			return d, nil
		}
	}
	latest, found, err := latestCheckIn(ctx, tx, key)
	if err != nil {
		return checksDecision{}, err
	}
	if found {
		d.row = &latest
	}
	switch {
	case !found:
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("no attempt of check %s of %s for %s is held on "+
			"%s, so there is nothing to run again", key.Name, key.OID, key.Target, repo.name)), nil
	case latest.Queued.IsZero():
		return d.refuse(CheckRefusalNativeCheck, fmt.Sprintf("%s is a person's CI's, which runs it again and "+
			"reports it; a rerun queues a check the engine runs", named(latest.CheckKey))), nil
	case latest.Attempt != prev.Attempt:
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("the check's latest attempt is %d, not the %d this "+
			"rerun follows; read the check again", latest.Attempt, prev.Attempt)), nil
	case !latest.State.Ended():
		return d.refuse(CheckRefusalNotFinished, fmt.Sprintf("%s is still %s; a check of a commit runs one "+
			"attempt at a time", named(prev), latest.State)), nil
	case key.Attempt > MaxCheckAttempts:
		return d.refuse(CheckRefusalAttemptsExhausted, fmt.Sprintf("%s was the last: a check of a commit runs "+
			"at most %d attempts (code.MaxCheckAttempts)", named(prev), MaxCheckAttempts)), nil
	case asker.Kind == PrincipalAgent && latest.State != CheckStateErrored && latest.State != CheckStateCancelled:
		return d.refuse(CheckRefusalRerunGenuine, fmt.Sprintf("%s ended %s (%s): a seat runs a check again only "+
			"where its infrastructure or a stop ended it, errored or cancelled, and a measurement stands — fix "+
			"what it measured and push, or ask a person, who may run it again", named(prev), latest.State,
			latest.Reason)), nil
	}
	ref := repo.grantRef(key.RepoID)
	author := false
	if handle, kind := asker.party(); kind == PartyAgent && latest.ChangeID != "" {
		authors, err := authorsAmong(ctx, tx, latest.ChangeID, []string{handle})
		if err != nil {
			return checksDecision{}, err
		}
		_, author = authors[handle]
	}
	if g := GrantOf(asker, ref, seats.Roster); g.Access < AccessWrite && !author {
		return d.refuse(CheckRefusalNotPermitted, fmt.Sprintf("%s holds %s on %s, and a check is run again by "+
			"whoever may write the repository, or by an author of the change it checks", principalNamed(asker),
			g.Access, repo.name)), nil
	}
	if why := r.Ceiling.changeRefusal(asker, ref, author, AccessRead, "run "+named(prev)+" again",
		seats.Roster); why != "" {
		return d.refuse(CheckRefusalNotPermitted, why), nil
	}
	held, _, err := readCheckRow(ctx, tx, latest.CheckKey)
	if err != nil {
		return checksDecision{}, err
	}
	if held.queue.def == "" {
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("%s was errored at its dispatch: its target's "+
			"checks file defines it in no form that runs, and the home's dispatch queues it again once the file "+
			"does", named(prev))), nil
	}
	var def CheckDef
	if err := json.Unmarshal([]byte(held.queue.def), &def); err != nil {
		return checksDecision{}, fmt.Errorf("code: decode the definition %s ran under: %w", named(prev), err)
	}
	by := asker
	d.payload = CheckQueued{CheckKey: key, Purpose: latest.Purpose, ChangeID: latest.ChangeID,
		Revision: latest.Revision, CandidateID: latest.CandidateID, EntryID: latest.EntryID, Base: latest.Base,
		DefsOID: latest.DefsOID, Def: &def, Required: latest.Required, Reason: QueueRerun, By: &by, QueuedAt: now}
	return d, nil
}

// decideReport is a report's decide — see the file's doc — over the snapshot
// tx, at instant now.
func decideReport(ctx context.Context, tx *sql.Tx, r CheckReported, reporter Principal, d checksDecision,
	named func(CheckKey) string, now time.Time) (checksDecision, error) {

	key := r.CheckKey
	latest, found, err := latestCheckIn(ctx, tx, key)
	if err != nil {
		return checksDecision{}, err
	}
	if found {
		d.row = &latest
	}
	_, payload := r.record(reporter, now)
	switch {
	case found && !latest.Queued.IsZero():
		return d.refuse(CheckRefusalNativeCheck, fmt.Sprintf("check %s of %s for %s is the engine's: it queued "+
			"%s, and a check of a commit is one runner's or one CI's; name your CI's check differently in "+
			"its report", key.Name, key.OID, key.Target, named(latest.CheckKey))), nil
	case !found && key.Attempt != 1:
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("no attempt of check %s of %s for %s is held, so "+
			"a report names the first", key.Name, key.OID, key.Target)), nil
	case !found:
	case latest.Attempt == key.Attempt && !latest.State.Ended():
		if r.State == CheckStateRunning {
			d.unchanged = true
			return d, nil
		}
	case latest.Attempt == key.Attempt:
		if latest.State == r.State && latest.URL == r.URL && latest.Summary == r.Summary {
			// THE ENDING SENT AGAIN — a CI's retried delivery — is answered
			// from the row.
			d.unchanged = true
			return d, nil
		}
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("%s has ended %s; a run of the check again is "+
			"reported as attempt %d", named(key), latest.State, key.Attempt+1)), nil
	case latest.Attempt == key.Attempt-1 && !latest.State.Ended():
		return d.refuse(CheckRefusalNotFinished, fmt.Sprintf("%s is still %s; report its ending before a "+
			"run after it", named(latest.CheckKey), latest.State)), nil
	case key.Attempt > MaxCheckAttempts:
		return d.refuse(CheckRefusalAttemptsExhausted, fmt.Sprintf("%s was the last: a check of a commit runs "+
			"at most %d attempts (code.MaxCheckAttempts)", named(latest.CheckKey), MaxCheckAttempts)), nil
	case latest.Attempt == key.Attempt-1:
	default:
		return d.refuse(CheckRefusalNoSuchCheck, fmt.Sprintf("the check's latest attempt is %d, and a report "+
			"names it while it runs or the one after it once it ended — not %d", latest.Attempt,
			key.Attempt)), nil
	}
	d.payload = payload
	return d, nil
}

// principalNamed is who p is, as a refusal names them: a seat by its handle,
// a person by the human seat they hold.
func principalNamed(p Principal) string {
	switch handle, kind := p.party(); kind {
	case PartyAgent:
		return "seat " + handle
	case PartyPerson:
		return "the person holding seat " + handle
	}
	return "this credential"
}
