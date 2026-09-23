package session_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FULL SESSION TABLE, EVERY ROW REACHED BY A REAL BEARER.
//
// Not the map read back — that asserts nothing — but seven states of the world
// driven through [Signer.Validate], each landing on the row the design names,
// with the cell each row permits for each of the three kinds of request.
//
// THE CONTROL IS THE TWO ABSENT-ROW ARMS. `gone` and `behind` are the same
// observation — no row — separated only by whether this node's applied
// position covers the bearer's start position. Collapse them and one of the
// two verdicts is wrong every time: answering 401 signs out every person whose
// request reached a node a second behind, and serving honours a session that
// ended. TestCollapsingTheAbsentRowArmsIsAStampede is that mutation, written
// out rather than described.
func TestTheFullSessionTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		world  func(*signedIn)
		row    session.Row
		reads  session.Answer
		writes session.Answer
		stepUp session.Answer
	}{{
		name:  "everything checks out",
		world: func(*signedIn) {},
		row:   session.RowValid,
		reads: session.AnswerServe, writes: session.AnswerServe,
		stepUp: session.AnswerStepUp,
	}, {
		name: "the session row says it ended",
		world: func(s *signedIn) {
			s.dir.identity.Session.Ended = true
		},
		row:   session.RowEnded,
		reads: session.AnswerRefuse, writes: session.AnswerRefuse,
		stepUp: session.AnswerRefuse,
	}, {
		name: "the person's revocation epoch has moved",
		world: func(s *signedIn) {
			s.dir.identity.Person.Epoch = 4
		},
		row:   session.RowEnded,
		reads: session.AnswerRefuse, writes: session.AnswerRefuse,
		stepUp: session.AnswerRefuse,
	}, {
		name: "the fleet-wide generation has moved",
		world: func(s *signedIn) {
			s.dir.identity.Generation = 2
		},
		row:   session.RowEnded,
		reads: session.AnswerRefuse, writes: session.AnswerRefuse,
		stepUp: session.AnswerRefuse,
	}, {
		name:  "the rotation index is ahead of the window",
		world: aheadOfTheWindow,
		row:   session.RowReuse,
		reads: session.AnswerRefuse, writes: session.AnswerRefuse,
		stepUp: session.AnswerRefuse,
	}, {
		name: "no row, and this node covers the start position",
		world: func(s *signedIn) {
			s.dir.identity.Session.Found = false
			s.dir.identity.Applied = startPos + 1
		},
		row:   session.RowGone,
		reads: session.AnswerRefuse, writes: session.AnswerRefuse,
		stepUp: session.AnswerRefuse,
	}, {
		name: "no row, and this node is below the start position",
		world: func(s *signedIn) {
			s.dir.identity.Session.Found = false
			s.dir.identity.Applied = startPos - 1
			s.dir.identity.Lag = 10 * time.Second
		},
		row:   session.RowBehind,
		reads: session.AnswerServe, writes: session.AnswerUnavailable,
		stepUp: session.AnswerUnavailable,
	}, {
		name: "the applier has stalled past the grace",
		world: func(s *signedIn) {
			s.dir.identity.Lag = statelog.StallGrace + time.Second
		},
		row:   session.RowStalled,
		reads: session.AnswerUnavailable, writes: session.AnswerUnavailable,
		stepUp: session.AnswerUnavailable,
	}, {
		name: "the person's bucket is deferred",
		world: func(s *signedIn) {
			s.dir.identity.Deferred = true
		},
		row:   session.RowStalled,
		reads: session.AnswerUnavailable, writes: session.AnswerUnavailable,
		stepUp: session.AnswerUnavailable,
	}, {
		name: "the replicated estate cannot be read",
		world: func(s *signedIn) {
			s.dir.err = errors.New("the replicated estate is not open")
		},
		row:   session.RowStalled,
		reads: session.AnswerUnavailable, writes: session.AnswerUnavailable,
		stepUp: session.AnswerUnavailable,
	}, {
		name: "the cookie is not a bearer at all",
		world: func(s *signedIn) {
			s.cookie = "v2.nonsense"
		},
		row:   session.RowMalformed,
		reads: session.AnswerRefuse, writes: session.AnswerRefuse,
		stepUp: session.AnswerRefuse,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			tc.world(rig)
			got := rig.validate()
			if got.Row != tc.row {
				t.Fatalf("landed on %q, want %q (%s)", got.Row, tc.row, got.Detail)
			}
			for need, want := range map[session.Need]session.Answer{
				session.NeedRead:   tc.reads,
				session.NeedWrite:  tc.writes,
				session.NeedStepUp: tc.stepUp,
			} {
				if answer := got.Answer(need); answer != want {
					t.Errorf("%s of a %q session answers %q, want %q",
						need, got.Row, answer, want)
				}
			}
		})
	}
}

// EVERY ROW IS COVERED AND EVERY ROW HAS A POLICY.
//
// Two-sided, because each omission is silent in its own way: a row with no
// entry in the table defaults to refusing, which is safe and invisible until
// somebody's session is refused for no reason; and a row the case above never
// reaches is one whose cells nothing checks.
func TestEveryRowHasAPolicyAndTheSuiteReachesThemAll(t *testing.T) {
	t.Parallel()
	reached := map[session.Row]bool{}
	for _, world := range worlds() {
		rig := newSignedIn(t)
		world(rig)
		reached[rig.validate().Row] = true
	}
	for _, row := range session.Rows {
		if !reached[row] {
			t.Errorf("no case in this suite lands on %q, so its cells are a "+
				"claim rather than coverage", row)
		}
		for _, need := range session.Needs {
			v := session.Validation{Row: row}
			if v.Answer(need) == "" {
				t.Errorf("%q has no answer for %s", row, need)
			}
		}
	}
	// AND A ROW THIS BUILD CANNOT NAME REFUSES. Serving by default is how
	// a new arm ships ungated and ships looking correct.
	unknown := session.Validation{Row: session.Row("somethingnewer")}
	for _, need := range session.Needs {
		if got := unknown.Answer(need); got != session.AnswerRefuse {
			t.Errorf("an unnamed row answers %s with %q, want %q", need, got,
				session.AnswerRefuse)
		}
	}
}

// aheadOfTheWindow leaves the rig holding a cookie three windows ahead of what
// its own clock says, which is the node-whose-clock-ran-fast case: mint, let
// three hours pass so a re-issue stamps index 3, then put the clock back.
func aheadOfTheWindow(s *signedIn) {
	s.clock.advance(3 * time.Hour)
	s.cookie = mustReissue(s.t, s)
	s.clock.advance(-3 * time.Hour)
}

// worlds is every state the table case drives, so the coverage check above and
// the table itself cannot drift apart.
func worlds() []func(*signedIn) {
	return []func(*signedIn){
		func(*signedIn) {},
		func(s *signedIn) { s.dir.identity.Session.Ended = true },
		aheadOfTheWindow,
		func(s *signedIn) {
			s.dir.identity.Session.Found = false
			s.dir.identity.Applied = startPos + 1
		},
		func(s *signedIn) {
			s.dir.identity.Session.Found = false
			s.dir.identity.Applied = startPos - 1
		},
		func(s *signedIn) {
			s.dir.identity.Lag = statelog.StallGrace + time.Second
		},
		func(s *signedIn) { s.dir.identity.Deferred = true },
		func(s *signedIn) { s.cookie = "v2.nonsense" },
	}
}

// A NODE THAT IS MERELY BEHIND ANSWERS 503 AND NEVER 401, ON EVERY ARM THAT
// CANNOT SERVE.
//
// THE CASE THE WHOLE TABLE IS SHAPED BY. A browser reads 401 as "sign in
// again" and discards the cookie, so one stalled applier answering 401 signs
// out every person whose request reached that node and stampedes the identity
// provider with the re-authentications — while the node is, by definition,
// already alarmed and about to catch up.
func TestANodeThatIsBehindNeverAnswers401(t *testing.T) {
	t.Parallel()
	for name, world := range map[string]func(*signedIn){
		"below the start position": func(s *signedIn) {
			s.dir.identity.Session.Found = false
			s.dir.identity.Applied = startPos - 1
		},
		"stalled past the grace": func(s *signedIn) {
			s.dir.identity.Lag = statelog.StallGrace + time.Second
		},
		"the estate is not open": func(s *signedIn) {
			s.dir.err = errors.New("ErrNoEstate")
		},
		"the person's bucket is deferred": func(s *signedIn) {
			s.dir.identity.Deferred = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			world(rig)
			got := rig.validate()
			for _, need := range session.Needs {
				if answer := got.Answer(need); answer == session.AnswerRefuse {
					t.Errorf("a node that is behind refuses %s outright, which "+
						"a browser reads as 'sign in again'", need)
				}
				if code := got.Code(need); code == session.CodeRevoked {
					t.Errorf("%s carries %q from a node that is merely behind",
						need, code)
				}
			}
		})
	}
}

// COLLAPSING THE TWO ABSENT-ROW ARMS IS A STAMPEDE, AND THIS IS THE CONTROL
// THE DESIGN NAMES.
//
// It asserts the two arms are DIFFERENT from one observation — no row — which
// is the whole content of carrying the start position in the bearer. An
// implementation that dropped the position comparison lands both on one row,
// and whichever row that is, is wrong half the time.
func TestCollapsingTheAbsentRowArmsIsAStampede(t *testing.T) {
	t.Parallel()
	covered, behind := newSignedIn(t), newSignedIn(t)
	for _, rig := range []*signedIn{covered, behind} {
		rig.dir.identity.Session.Found = false
	}
	covered.dir.identity.Applied = startPos + 1
	behind.dir.identity.Applied = startPos - 1

	a, b := covered.validate(), behind.validate()
	if a.Row == b.Row {
		t.Fatalf("both absent-row arms landed on %q — one observation, two "+
			"opposite answers, and no way left to tell them apart", a.Row)
	}
	if a.Answer(session.NeedRead) != session.AnswerRefuse {
		t.Error("a node that has seen the session's own start record and has " +
			"no row for it served the request, which honours a session that " +
			"ended")
	}
	if b.Answer(session.NeedRead) != session.AnswerServe {
		t.Error("a node below the session's start position refused a read, " +
			"which signs out every person whose request reached a node that " +
			"is a second behind")
	}
	if b.Answer(session.NeedWrite) != session.AnswerUnavailable {
		t.Error("a node below the session's start position accepted a write, " +
			"and there is no grace for one: the row it would be checked " +
			"against has not arrived")
	}
}

// THE ABSOLUTE DEADLINE BEATS A RENEWED IDLE CLOCK.
//
// Every request moves the idle deadline out, and nothing moves the absolute
// one — so a session somebody uses continuously still ends on time. The
// opposite reading, a deadline any activity extends, is a sign-in that lasts
// for ever as long as a tab is open.
func TestTheAbsoluteDeadlineBeatsARenewedIdleClock(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)

	// Used every half hour, right up to the absolute deadline.
	for elapsed := time.Duration(0); elapsed < absolute; elapsed += 30 * time.Minute {
		got := rig.validate()
		if got.Row != session.RowValid {
			t.Fatalf("at %s the session landed on %q: %s", elapsed, got.Row,
				got.Detail)
		}
		if got.Reissue != "" {
			rig.cookie = got.Reissue
		}
		rig.clock.advance(30 * time.Minute)
	}
	got := rig.validate()
	if got.Row != session.RowEnded {
		t.Fatalf("past the absolute deadline the session landed on %q: %s",
			got.Row, got.Detail)
	}
	if !strings.Contains(got.Detail, "absolute") {
		t.Errorf("it ended for the reason %q, want the absolute deadline",
			got.Detail)
	}
}

// AN IDLE SESSION IS SERVED AND RE-ISSUED, NOT CALLED THEFT.
//
// THE DECISION rotate.go argues, asserted. The rule this replaces treated an
// index more than one window behind the clock as reuse — so a person who
// stopped at noon and came back at two would have had their revocation epoch
// bumped, every session they hold ended, and a WARN alarm raised naming them.
// It also made the twelve-hour idle deadline unreachable, which is a mechanism
// the design spends a whole field in the bearer on.
func TestALongIdleSessionIsServedRatherThanCalledReuse(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	// Six hours away: six rotation windows, and well inside both the idle
	// deadline and the absolute one.
	rig.clock.advance(6 * time.Hour)

	got := rig.validate()
	if got.Row != session.RowValid {
		t.Fatalf("a session idle for six hours landed on %q: %s", got.Row,
			got.Detail)
	}
	if got.Reuse {
		t.Error("coming back from lunch bumped the person's revocation epoch, " +
			"which ends every session they hold and fires a theft alarm " +
			"naming them")
	}
	if got.Reissue == "" {
		t.Fatal("no re-issue, so the cookie stays on an old window for ever")
	}
	// And the re-issued cookie is current: the next request does not
	// re-issue again inside the throttle.
	rig.cookie = got.Reissue
	if next := rig.validate(); next.Reissue != "" {
		t.Error("a cookie issued a moment ago was re-issued again, so every " +
			"response carries a Set-Cookie header")
	}
}

// AN INDEX AHEAD OF THE WINDOW BUMPS THE EPOCH AND ENDS EVERY SESSION.
//
// What survives as POSITIVE theft evidence once a lagging index no longer
// counts: the engine issues an index for the window it is in, so a bearer
// claiming one that has not begun was not issued in the ordinary way. The
// overlap absorbs a couple of minutes of clock skew between two ingress nodes,
// and anything past it is a fault worth ending sessions over — a clock that
// far out has also been minting deadlines that are wrong.
func TestAnIndexPastTheOverlapBumpsTheEpochAndEndsEverySession(t *testing.T) {
	t.Parallel()

	// Inside the overlap: a node whose clock trails the minting node's by
	// a minute, a second after a boundary.
	inside := newSignedIn(t)
	inside.clock.advance(time.Hour)
	inside.cookie = mustReissue(t, inside)
	inside.clock.advance(-time.Minute)
	if got := inside.validate(); got.Row != session.RowValid || got.Reuse {
		t.Errorf("a cookie one minute across a boundary landed on %q (reuse "+
			"%v): %s — four parallel requests from one page would be refused "+
			"three times", got.Row, got.Reuse, got.Detail)
	}

	// Past it: three hours of skew.
	past := newSignedIn(t)
	past.clock.advance(3 * time.Hour)
	past.cookie = mustReissue(t, past)
	past.clock.advance(-3 * time.Hour)
	got := past.validate()
	if got.Row != session.RowReuse {
		t.Fatalf("an index three windows ahead landed on %q: %s", got.Row,
			got.Detail)
	}
	if !got.Reuse {
		t.Error("the reuse row did not ask the caller to bump the epoch, so " +
			"detection would be a log line and the captured cookie would go " +
			"on working")
	}
	if got.Answer(session.NeedRead) != session.AnswerRefuse {
		t.Error("a reused bearer was served")
	}
}

// FOUR CONCURRENT REQUESTS ACROSS A ROTATION BOUNDARY ALL SUCCEED.
//
// A page that loads at 10:59:58 fires several requests at once; the window
// turns over between them. Every one must be served — and on a node that has
// applied NOTHING since the login, which is the arm where the bearer's own
// signature and epoch are the only proof the sign-in happened.
func TestFourConcurrentRequestsAcrossARotationBoundaryAllSucceed(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	// A node that has applied nothing since the login: no session row, and
	// an applied position below the bearer's start.
	rig.dir.identity.Session.Found = false
	rig.dir.identity.Applied = startPos - 1
	// One second before the first boundary.
	rig.clock.advance(time.Hour - time.Second)

	var wg sync.WaitGroup
	rows := make([]session.Validation, 4)
	for i := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The window turns over underneath the fan-out.
			if i == 1 {
				rig.clock.advance(2 * time.Second)
			}
			rows[i] = rig.signer.Validate(t.Context(), rig.dir, rig.cookie)
		}()
	}
	wg.Wait()
	for i, got := range rows {
		if got.Answer(session.NeedRead) != session.AnswerServe {
			t.Errorf("request %d answered %q on row %q: %s", i,
				got.Answer(session.NeedRead), got.Row, got.Detail)
		}
		if got.Reuse {
			t.Errorf("request %d was called theft: %s", i, got.Detail)
		}
	}
}

// A PRE-LOGIN COOKIE IS NEVER HONOURED.
//
// The engine sets values in a browser before anybody has authenticated — the
// sealed state a sign-in round trip carries, and whatever else a deployment
// puts on the same host. None of them is a session, and every one of them
// arrives in the same cookie jar. What makes them safe is not where they are
// stored but that a bearer is only honoured when it PARSES as one and verifies
// under a key this node holds.
func TestAPreLoginCookieIsNeverHonoured(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	live := rig.validate()
	if live.Row != session.RowValid {
		t.Fatalf("the rig's own session is not valid: %s", live.Detail)
	}
	for name, value := range map[string]string{
		"an empty value":       "",
		"another app's cookie": "sessionid=abc123",
		"a bearer with no mac": strings.Join(strings.Split(rig.cookie, ".")[:8], "."),
		"a bearer with a forged mac": strings.Join(
			append(strings.Split(rig.cookie, ".")[:8], "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), "."),
		"a bearer at another version": "v1" +
			strings.TrimPrefix(rig.cookie, "v2"),
		"a bearer whose person was swapped": swapPerson(t, rig.cookie),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := rig.signer.Validate(t.Context(), rig.dir, value)
			if got.Row != session.RowMalformed {
				t.Errorf("%q landed on %q, want %q", value, got.Row,
					session.RowMalformed)
			}
			if got.Answer(session.NeedRead) != session.AnswerRefuse {
				t.Errorf("%q was served", value)
			}
		})
	}
}

// swapPerson rewrites the person inside a bearer's triple without re-signing,
// which is the edit an attacker holding somebody else's cookie would make.
func swapPerson(t *testing.T, cookie string) string {
	t.Helper()
	parts := strings.Split(cookie, ".")
	triple := strings.Split(parts[3], "~")
	triple[2] = "018f3a9c-0000-7000-8000-0000000000bb"
	parts[3] = strings.Join(triple, "~")
	return strings.Join(parts, ".")
}

// mustReissue drives one validation and returns the cookie it handed back.
func mustReissue(t *testing.T, rig *signedIn) string {
	t.Helper()
	got := rig.validate()
	if got.Reissue == "" {
		t.Fatalf("no re-issue on row %q: %s", got.Row, got.Detail)
	}
	return got.Reissue
}

// --- the seat table ---------------------------------------------------------- //

// THE FULL SEAT TABLE, EVERY ROW, AND THE 403 NAMES THE SEAT.
//
// It is the session table's shape at one remove, because a seat is a row in a
// domain that lags INDEPENDENTLY: a node can be current on identity and a
// minute behind on the chart, so a missing seat is three answers rather than
// one. The arm that must never exist is a silent fall-through to an empty
// handle — that principal writes audit rows under nobody's name and reads
// every person-scoped query as empty.
func TestTheFullSeatTable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		world  func(*signedIn)
		row    session.SeatRow
		answer session.Answer
		handle string
	}{{
		name:   "the person holds no seat",
		world:  func(s *signedIn) { s.dir.identity.Person.Seat = "" },
		row:    session.SeatRowSeatless,
		answer: session.AnswerServe,
	}, {
		name:   "the seat is in the view, human, not tombstoned",
		world:  func(*signedIn) {},
		row:    session.SeatRowHeld,
		answer: session.AnswerServe,
		handle: "platform-lead",
	}, {
		name: "the seat is gone and this node covers the binding",
		world: func(s *signedIn) {
			delete(s.chart.seats, "platform-lead")
			s.chart.position = 1000
		},
		row:    session.SeatRowGone,
		answer: session.AnswerRefuse,
	}, {
		name: "the seat is tombstoned",
		world: func(s *signedIn) {
			s.chart.seats["platform-lead"] = session.Seat{
				Handle: "platform-lead", Kind: "human", Tombstoned: true,
			}
		},
		row:    session.SeatRowGone,
		answer: session.AnswerRefuse,
	}, {
		name: "the seat is an agent seat",
		world: func(s *signedIn) {
			s.chart.seats["platform-lead"] = session.Seat{
				Handle: "platform-lead", Kind: "agent",
			}
		},
		row:    session.SeatRowGone,
		answer: session.AnswerRefuse,
	}, {
		name: "the seat is absent and this node is below the binding",
		world: func(s *signedIn) {
			delete(s.chart.seats, "platform-lead")
			s.chart.position = 899
		},
		row:    session.SeatRowBehind,
		answer: session.AnswerUnavailable,
	}, {
		name: "the chart applier has stalled",
		world: func(s *signedIn) {
			s.chart.lag = statelog.StallGrace + time.Second
		},
		row:    session.SeatRowStalled,
		answer: session.AnswerUnavailable,
	}, {
		name:   "the chart view cannot be read",
		world:  func(s *signedIn) { s.chart.err = errors.New("no view") },
		row:    session.SeatRowStalled,
		answer: session.AnswerUnavailable,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			tc.world(rig)
			got := session.ResolveSeat(t.Context(), rig.chart,
				rig.dir.identity.Person)
			if got.Row != tc.row {
				t.Fatalf("landed on %q, want %q (%s)", got.Row, tc.row, got.Detail)
			}
			if answer := got.Answer(); answer != tc.answer {
				t.Errorf("%q answers %q, want %q", got.Row, answer, tc.answer)
			}
			if got.Handle() != tc.handle {
				t.Errorf("the handle is %q, want %q", got.Handle(), tc.handle)
			}
			// A REFUSAL NAMES THE SEAT. A person locked out because
			// somebody removed a seat needs to know which one, and so
			// does whoever removed it.
			if tc.answer == session.AnswerRefuse &&
				!strings.Contains(got.Detail, "platform-lead") {
				t.Errorf("the refusal %q does not name the seat", got.Detail)
			}
		})
	}
}

// A SEAT THAT COULD NOT BE RESOLVED NEVER FALLS THROUGH TO AN EMPTY HANDLE.
//
// The one arm that is allowed to answer with no handle is the seatless one.
// Everything else either resolves the seat or refuses — because a principal
// carrying an empty handle is one whose every write lands under nobody's name
// and whose every person-scoped read comes back empty, which reads exactly
// like a person with nothing assigned to them.
func TestOnlyTheSeatlessArmEverAnswersWithNoHandle(t *testing.T) {
	t.Parallel()
	for _, row := range session.SeatRows {
		if row == session.SeatRowSeatless || row == session.SeatRowHeld {
			continue
		}
		b := session.Binding{Row: row}
		if b.Answer() == session.AnswerServe {
			t.Errorf("%q serves with the handle %q", row, b.Handle())
		}
	}
	// And a node with no chart seam at all is the unknown arm rather than
	// the seatless one.
	got := session.ResolveSeat(t.Context(), nil, session.PersonRow{
		Found: true, Seat: "platform-lead",
	})
	if got.Row != session.SeatRowStalled {
		t.Errorf("a node with no chart view landed on %q, want %q", got.Row,
			session.SeatRowStalled)
	}
}

// EVERY SEAT ROW HAS A POLICY, AND AN UNNAMED ONE REFUSES.
func TestEverySeatRowHasAPolicy(t *testing.T) {
	t.Parallel()
	for _, row := range session.SeatRows {
		if (session.Binding{Row: row}).Answer() == "" {
			t.Errorf("%q has no answer", row)
		}
	}
	if got := (session.Binding{Row: session.SeatRow("newer")}).Answer(); got !=
		session.AnswerRefuse {
		t.Errorf("an unnamed seat row answers %q, want %q", got,
			session.AnswerRefuse)
	}
	if !slices.Contains(session.SeatRows, session.SeatRowHeld) {
		t.Error("the row list does not carry the row that serves")
	}
}

// A SUSPENDED PERSON IS REFUSED BY THE SESSION TABLE, NOT LEFT TO A GRANT
// CHECK.
//
// A grant answers what somebody may DO; this answers whether they are somebody
// at all. The stage allowlist is one value wide, so a stage a newer peer wrote
// that this build cannot name refuses — a denylist would have admitted it.
func TestASuspendedOrUnknownStageIsRefused(t *testing.T) {
	t.Parallel()
	for _, stage := range []iam.Stage{
		iam.StageInvited, iam.StageEnrolling, iam.StageSuspended,
		iam.StageRetired, iam.Stage("somethingnewer"),
	} {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			rig.dir.identity.Person.Stage = stage
			got := rig.validate()
			if got.Row != session.RowEnded {
				t.Errorf("a %q person landed on %q: %s", stage, got.Row,
					got.Detail)
			}
		})
	}
}

// BOTH BEHIND ARMS MOVE THE IDLE DEADLINE, BECAUSE BOTH SERVE.
//
// A node can hold the session's row without the person's, or the person's
// without the session's — they are records on different subjects. Whichever
// half is missing, the request is served on the bearer's own proof, so
// whichever half is missing the deadline has to move: an arm that served
// without re-issuing would let a session in continuous use on a lagging node
// expire on the deadline it was minted with.
func TestBothBehindArmsReissueTheCookie(t *testing.T) {
	t.Parallel()
	for name, world := range map[string]func(*signedIn){
		"the session's row has not arrived": func(s *signedIn) {
			s.dir.identity.Session.Found = false
		},
		"the person's row has not arrived": func(s *signedIn) {
			s.dir.identity.Person.Found = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			world(rig)
			rig.dir.identity.Applied = startPos - 1
			// Past the re-issue throttle, so a served arm must hand
			// one back.
			rig.clock.advance(session.ReissueAfter + time.Minute)

			got := rig.validate()
			if got.Row != session.RowBehind {
				t.Fatalf("landed on %q: %s", got.Row, got.Detail)
			}
			if got.Answer(session.NeedRead) != session.AnswerServe {
				t.Fatal("the read was not served")
			}
			if got.Reissue == "" {
				t.Error("served without a re-issue, so a session in " +
					"continuous use on a lagging node expires on the deadline " +
					"it was minted with")
			}
		})
	}
}

// A DEADLINE SAYS WHICH DEADLINE, AND NOTHING ELSE DOES.
//
// The idle deadline lives in the bearer and nowhere else, so the validation
// that notices it passed is the only frame that can ever report a session
// ending that way — the audit trail's `idle` and `absolute` reasons are read
// from here. An end a RECORD decided (the row says ended, the epoch moved)
// must NOT carry one: that end was already announced by whoever wrote the
// record, and a second announcement from every node a stale cookie reaches
// would name the wrong cause.
//
// Mutation: drop the Deadline from the idle arm and the first case fails; set
// one on the row-ended arm and the last does.
func TestADeadlineSaysWhichDeadlineEndedTheSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		world func(*signedIn)
		want  session.Deadline
	}{
		{"idle past the idle window", func(s *signedIn) {
			// A LIFETIME LONGER THAN THE IDLE WINDOW, which the rig's own
			// is not: this is the session a person leaves open overnight.
			cookie, err := s.signer.Mint(session.Mint{
				Lineage: s.lineage, Person: personID, Epoch: 3, Generation: 1,
				StartPosition:     startPos,
				AbsoluteExpiresAt: s.clock.at.Add(30 * 24 * time.Hour),
			})
			if err != nil {
				s.t.Fatalf("mint: %v", err)
			}
			s.cookie = cookie
			s.clock.advance(session.Idle + time.Minute)
		}, session.DeadlineIdle},
		{"past the absolute lifetime", func(s *signedIn) {
			s.clock.advance(absolute + time.Minute)
		}, session.DeadlineAbsolute},
		{"ended by a record", func(s *signedIn) {
			s.dir.identity.Session.Ended = true
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			tc.world(rig)
			got := rig.validate()
			if got.Row != session.RowEnded {
				t.Fatalf("landed on %q, want ended: %s", got.Row, got.Detail)
			}
			if got.Deadline != tc.want {
				t.Errorf("deadline = %q, want %q (%s)", got.Deadline, tc.want, got.Detail)
			}
			if !got.Deadline.Valid() {
				t.Errorf("deadline %q is not one this build names", got.Deadline)
			}
		})
	}
}

// A DEADLINE'S REFUSAL READS NOTHING, AND STANDING IS THE READ IT SKIPPED.
//
// The deadlines are decided before any row is looked up, so a cookie past its
// deadline lands on `ended` whether it was live until then or a record ended it
// a day earlier — and only the first is the deadline's to announce. Standing
// answers that from the rows alone, through the same half of the table
// Validate uses, so the two can never disagree about what a row means.
//
// Mutation: consult the bearer's deadlines inside Standing and the live case
// lands on `ended`; skip the rows and every record case lands on `valid`.
func TestStandingIsWhatTheRowsSayPastADeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		world func(*signedIn)
		want  session.Row
	}{
		{"live until the deadline", func(*signedIn) {}, session.RowValid},
		{"a record ended it", func(s *signedIn) { s.dir.identity.Session.Ended = true }, session.RowEnded},
		{"the person's epoch moved", func(s *signedIn) { s.dir.identity.Person.Epoch = 4 }, session.RowEnded},
		{"the generation moved", func(s *signedIn) { s.dir.identity.Generation = 2 }, session.RowEnded},
		{"the sweep collected it", func(s *signedIn) {
			s.dir.identity.Session.Found = false
			s.dir.identity.Applied = startPos + 1
		}, session.RowGone},
		{"this node has not applied it", func(s *signedIn) {
			s.dir.identity.Session.Found = false
			s.dir.identity.Applied = startPos - 1
		}, session.RowBehind},
		{"this node cannot read", func(s *signedIn) {
			s.dir.err = errors.New("the replicated estate is not open")
		}, session.RowStalled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			tc.world(rig)
			rig.clock.advance(absolute + time.Minute)
			refused := rig.validate()
			if refused.Row != session.RowEnded || refused.Deadline != session.DeadlineAbsolute {
				t.Fatalf("validation landed on %q (%q), want the absolute deadline",
					refused.Row, refused.Deadline)
			}
			got := session.Standing(t.Context(), rig.dir, refused.Bearer)
			if got.Row != tc.want {
				t.Errorf("standing is %q, want %q (%s)", got.Row, tc.want, got.Detail)
			}
			if got.Reissue != "" || got.Deadline != "" {
				t.Errorf("standing re-issued %q or named deadline %q: it is "+
					"about the rows alone", got.Reissue, got.Deadline)
			}
		})
	}
}
