package session_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam/session"
)

// restrictedCookie mints the rig's session again as one that may only enrol a
// second factor, as a password sign-in does where a second factor is
// required and the person holds none.
func (s *signedIn) restrictedCookie() string {
	s.t.Helper()
	cookie, err := s.signer.Mint(session.Mint{
		Lineage: s.lineage, Person: personID, Epoch: 3, Generation: 1,
		StartPosition: startPos, AbsoluteExpiresAt: s.clock.now().Add(absolute),
		EnrolmentOnly: true,
	})
	if err != nil {
		s.t.Fatalf("mint a restricted bearer: %v", err)
	}
	return cookie
}

// A SESSION THAT MAY ONLY ENROL SAYS SO IN ITS OWN BEARER, WHEREVER IT IS READ.
//
// The restriction lived on the session's ROW alone, and a node that has not
// applied the row serves reads on the bearer alone — every node, for the apply
// latency after every sign-in, since a sign-in answers before any node
// applies it. There the row is the zero row, so the session read as whole. It
// is the bearer's own signed fact now, so a node with no row still reads it,
// and every re-issue carries it on.
//
// The CONTROL is a whole session on the same lagging node, which reads as
// whole — so the restriction is the bearer's and not the lag's. Mutation:
// drop the scope from the payload, or read [session.Validation.EnrolmentOnly]
// off the row alone, and the restricted rows read as whole.
func TestASessionThatMayOnlyEnrolSaysSoInItsOwnBearer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		restricted bool
	}{
		{"a session that may only enrol", true},
		{"a whole session (the control)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			cookie := rig.cookie
			if tc.restricted {
				cookie = rig.restrictedCookie()
			}
			// A NODE THAT HAS NOT APPLIED THE SESSION'S START: no row,
			// and its position below the bearer's.
			rig.dir.identity.Session = session.LineageRow{}
			rig.dir.identity.Applied = startPos - 1
			rig.clock.advance(session.ReissueAfter + time.Minute)

			got := rig.signer.Validate(t.Context(), rig.dir, cookie)
			if got.Row != session.RowBehind ||
				got.Answer(session.NeedRead) != session.AnswerServe {
				t.Fatalf("landed on %q (%s); this case is about a read the "+
					"lagging node serves", got.Row, got.Detail)
			}
			if got.EnrolmentOnly() != tc.restricted {
				t.Errorf("a node with no row for the session read it as "+
					"enrolment-only %v, want %v", got.EnrolmentOnly(), tc.restricted)
			}
			if got.Reissue == "" {
				t.Fatal("no re-issue past the throttle; this case needs one to " +
					"read back")
			}
			again := rig.signer.Validate(t.Context(), rig.dir, got.Reissue)
			if again.EnrolmentOnly() != tc.restricted {
				t.Errorf("the re-issued bearer reads as enrolment-only %v, want "+
					"%v — a re-issue that dropped it would widen the session on "+
					"its first request after five minutes", again.EnrolmentOnly(),
					tc.restricted)
			}
		})
	}
}

// A WHOLE SESSION'S BEARER IS THE FORMAT EVERY BUILD READS, AND A SCOPED ONE IS
// A FORMAT NO OLDER BUILD ACCEPTS.
//
// Two builds share bearers during a rolling upgrade, so the scope is OPTIONAL:
// an ordinary session's bearer is byte for byte the nine fields every build
// parses, and only a restricted one grows the tenth — which a build knowing no
// scope refuses as malformed rather than serving whole. The same refusal meets
// a scope THIS build cannot name (a newer build narrowing a session some other
// way) and a scope cut off to widen the session, which the signature covers.
//
// Mutation: always append a scope and the whole row has ten fields; accept an
// unknown scope as whole and the forged row is served; leave the scope outside
// the signed payload and the stripped row validates as a whole session.
func TestAScopedBearerIsAFormatNoOlderBuildServesWhole(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	restricted := rig.restrictedCookie()

	if n := len(strings.Split(rig.cookie, ".")); n != 9 {
		t.Errorf("a whole session's bearer has %d fields, want the nine every "+
			"build reads", n)
	}
	if n := len(strings.Split(restricted, ".")); n != 10 {
		t.Fatalf("a restricted session's bearer has %d fields, want ten", n)
	}

	// THE FORGE IS SOUND: the restricted bearer re-signed as it stands
	// still validates, so a refusal below is the scope's and not the
	// helper's.
	resigned := resignForDomain(t, restricted, session.SigningDomain, "k2",
		"the-active-key-material")
	if got := rig.signer.Validate(t.Context(), rig.dir, resigned); got.Row !=
		session.RowValid || !got.EnrolmentOnly() {
		t.Fatalf("the re-signed restricted bearer landed on %q (enrolment-only "+
			"%v): %s", got.Row, got.EnrolmentOnly(), got.Detail)
	}

	parts := strings.Split(restricted, ".")
	withScope := func(scope string) string {
		forged := append(append(parts[:8:8], scope), parts[9])
		return resignForDomain(t, strings.Join(forged, "."),
			session.SigningDomain, "k2", "the-active-key-material")
	}
	for name, cookie := range map[string]string{
		// NOT re-signed: whoever cuts the scope off holds no key.
		"the scope cut off":                strings.Join(append(parts[:8:8], parts[9]), "."),
		"a scope this build cannot name":   withScope("admin"),
		"an empty scope in the tenth slot": withScope(""),
	} {
		got := rig.signer.Validate(t.Context(), rig.dir, cookie)
		if got.Row != session.RowMalformed {
			t.Errorf("%s: landed on %q (enrolment-only %v), want %q", name,
				got.Row, got.EnrolmentOnly(), session.RowMalformed)
		}
	}
}
