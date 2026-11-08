package executordomain

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// These cases apply ends through the shipped applier, as their stamps say, and
// read back the row, the conversation's head and what the end absorbed.

// endStamped is p's end as a record stamped version v, the bytes a writer that
// stamped it so publishes — a relay keeping its writer's version among them —
// for [estate.applyEncoded] to apply.
func (e *estate) endStamped(p End, v int) []byte {
	e.t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		e.t.Fatalf("encode the end payload: %v", err)
	}
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: v, OpID: statelog.NewOpID(monday, "harness"),
			Subject: ConversationSubject(p.Handle, p.ConversationKey, p.Turn), Op: OpEnd,
			CreatedAt: monday, Gen: 1, Writer: "node-a", Scope: ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "executor", ActorKind: iam.ActorSystem,
	})
	if err != nil {
		e.t.Fatalf("encode the end stamped %d: %v", v, err)
	}
	if env, err := DecodeEnvelope(encoded); err != nil || env.V != v {
		e.t.Fatalf("the premise: the end is stamped %d (%v), want %d", env.V, err, v)
	}
	return encoded
}

// AN END WHOSE TURN WENT ON PAST ITS SESSION KEEPS IT. A turn whose row names a
// session it went on past — a capture its job took while it still worked, its
// final capture refused after it; a launch's capture that a later launch
// continued, or started fresh beside, and that kept nothing of its own — ends
// saying so, and the row keeps that session, its ending naming any newer
// capture that was not kept: what the turn did up to it, which holds more than
// the conversation's previous head or a fresh session would. And the end
// absorbs what the turn's read handed out, as any turn that landed a capture
// does, so the conversation's next turn is handed this session as its head —
// unless the session itself ran out of context in the turn, which retires it
// whatever the end says. Released instead, the row named no session and the
// head was handed the conversation's previous one; released and then absorbed,
// as the rule once was, the conversation was left with no head at all.
//
// Mutation: take the row's session off for an unkept attempt whatever the end
// is stamped, and the cases beside a newer capture not kept lose it; absorb
// nothing where the end says the turn went on, and the head the turn was handed
// stays beside it; let went_on shield a session that ran out of context, and the
// last case keeps a session no launch may continue.
func TestAnEndWhoseTurnWentOnPastItsSessionKeepsIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		unkept    *executorkeep.Unkept
		exhausted []string
		retired   bool
	}{
		{name: "a refused final after a save of the same launch",
			unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 2, Reason: "the object store's reservation is spent"}},
		{name: "a later launch that continued the row's capture and kept nothing"},
		{name: "a later launch whose capture was refused",
			unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "the seat's budget is spent"}},
		{name: "a later launch that started a session of its own and ran out of context in it",
			unkept:    &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "the job ran out"},
			exhausted: []string{"s-fresh"}},
		{name: "a later launch that ran out of context in the row's own session",
			unkept:    &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "the job ran out"},
			exhausted: []string{"s-kept"}, retired: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEstate(t)
			head, current := newTurn(), newTurn()
			e.capture(capturing("ana", "chat:room", head, 1, 1, 100, budget))
			e.end(ending("ana", "chat:room", head, "", completed))
			// THE TURN READ THE HEAD, and kept one capture of its first launch.
			e.capture(launching(e.head("ana", "chat:room"), capturingSession("ana", "chat:room", current,
				"s-kept", 1)))
			e.end(ending("ana", "chat:room", current, "", executorkeep.Ending{Outcome: executorkeep.OutcomeStopped,
				StoppedBy: "ana.diaz", WentOn: true, Unkept: c.unkept, Exhausted: c.exhausted}))

			s, found := e.session("ana", current)
			if !found || !s.Ended() || s.Ending == nil || !s.Ending.WentOn {
				t.Fatalf("the turn's row is (%+v, %v), want it ended saying the turn went on", s, found)
			}
			if c.retired {
				if s.HasObject() || s.RetiredBy != current {
					t.Errorf("the row is %+v, want the session that ran out retired by its own turn", s)
				}
				if _, stays := e.session("ana", head); !stays {
					t.Error("the end absorbed the head with no session of its own to stand in for it")
				}
				return
			}
			if !s.HasObject() || s.LaunchSeq != 1 || s.CaptureSeq != 1 || s.SessionID != "s-kept" {
				t.Errorf("the row is %+v, want it still naming the session the turn went on past", s)
			}
			if !s.WentOn() {
				t.Error("the row does not read as one its turn went on past")
			}
			if u := s.Unkept(); (u == nil) != (c.unkept == nil) ||
				(u != nil && (u.LaunchSeq != c.unkept.LaunchSeq || u.CaptureSeq != c.unkept.CaptureSeq)) {
				t.Errorf("the row stands %+v unkept, want %+v beside the session it keeps", u, c.unkept)
			}
			if _, stays := e.session("ana", head); stays {
				t.Error("the end left the head the turn was handed, as a turn that kept no session would")
			}
			if h := e.head("ana", "chat:room"); h.Session == nil || h.Session.Turn != current {
				t.Errorf("the conversation's next turn is handed %+v, want the session the turn went on past",
					h.Session)
			}
		})
	}
}

// AN END STAMPED BELOW FOUR IS APPLIED BY THE RULE IT WAS WRITTEN UNDER. A
// record's meaning is fixed by its stamp. An end naming its turn's newest
// capture as not kept, newer than the capture its row names, and stamped below
// version 4 — every end a build before it wrote, an end of this build that
// carries nothing newer than the base format or a session that ran out, a relay
// keeping its writer's version — is applied as those builds apply it on every
// copy they hold: the older session goes off the row, and the end absorbs
// nothing, so the head the turn was handed stays. One stamped 4 is applied by
// the rule version 4 brought, whether or not its ending says the turn went on:
// the row keeps its session, and the end absorbs the head. Decided by the field
// instead, a node of the build before and a node of this one would apply one
// end two ways, and hold rows no other copy holds.
//
// Mutation: branch on whether the ending carries went_on rather than on the
// stamp, and the end stamped 4 without it releases the session; apply every end
// by the new rule, and the ends stamped 1 and 3 keep theirs and absorb the head.
func TestAnEndStampedBelowFourIsAppliedByTheRuleItWasWrittenUnder(t *testing.T) {
	t.Parallel()
	unkept := &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "the object store's reservation is spent"}
	for _, c := range []struct {
		name    string
		stamped int
		keeps   bool
	}{
		{name: "stamped 1", stamped: baseRecordVersion},
		{name: "stamped 3", stamped: 3},
		{name: "stamped 4", stamped: wentOnVersion, keeps: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEstate(t)
			head, current := newTurn(), newTurn()
			e.capture(capturing("ana", "chat:room", head, 1, 1, 100, budget))
			e.end(ending("ana", "chat:room", head, "", completed))
			e.capture(launching(e.head("ana", "chat:room"), capturing("ana", "chat:room", current, 1, 1, 100,
				budget)))
			e.applyEncoded(OpEnd, e.endStamped(ending("ana", "chat:room", current, "",
				executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, Unkept: unkept}), c.stamped))

			s, found := e.session("ana", current)
			if !found || !s.Ended() {
				t.Fatalf("the turn's row is (%+v, %v), want it ended", s, found)
			}
			if s.HasObject() != c.keeps {
				t.Errorf("the row names a session: %v, want %v for an end stamped %d", s.HasObject(), c.keeps,
					c.stamped)
			}
			if _, stays := e.session("ana", head); stays == c.keeps {
				t.Errorf("the head the turn was handed stays: %v, want %v for an end stamped %d", stays, !c.keeps,
					c.stamped)
			}
			if u := s.Unkept(); u == nil || u.LaunchSeq != unkept.LaunchSeq {
				t.Errorf("the row stands %+v unkept, want launch 2's capture whichever rule applied", u)
			}
		})
	}
}
