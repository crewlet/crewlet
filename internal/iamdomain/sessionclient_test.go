package iamdomain_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SESSION IS LISTED WITH THE BROWSER AND THE ADDRESS THAT OPENED IT, so
// "Sign out" can be aimed at a lost laptop. The sign-in names them on the
// start record and the listing reads them back; a User-Agent is kept to its
// bound, cut on a rune, since every data node keeps every session row.
//
// Mutation: drop the two fields from the start record or from the listing's
// read, and the session lists no device.
func TestASessionIsListedWithTheBrowserAndAddressThatOpenedIt(t *testing.T) {
	t.Parallel()
	const firefox = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14.6; rv:132.0) Gecko/20100101 Firefox/132.0"
	long := strings.Repeat("é", iamdomain.MaxSessionUserAgent) // two bytes a rune
	for _, tc := range []struct {
		name, ua, remote string
		wantUA           string
	}{
		{"a browser", firefox, "203.0.113.4", firefox},
		{"a header past the bound", long, "198.51.100.7", long[:iamdomain.MaxSessionUserAgent]},
		{"no browser (a token's exchange)", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newWriteRig(t)
			person := enrolForSessions(t, rig)
			lineage := uuid.Must(uuid.NewV7()).String()
			if err := rig.draining(func() error {
				_, err := rig.writer.OpenSession(t.Context(), iamdomain.SessionStart{
					Lineage: lineage, Person: person,
					AbsoluteExpiresAt: time.Now().UTC().Add(time.Hour),
					UserAgent:         tc.ua, Remote: tc.remote, OpID: "session:" + lineage,
				})
				return err
			}); err != nil {
				t.Fatalf("open: %v", err)
			}
			rig.drain()
			listed, err := rig.reader(t).Sessions(t.Context(), person)
			if err != nil {
				t.Fatalf("Sessions: %v", err)
			}
			if len(listed) != 1 {
				t.Fatalf("the listing holds %d sessions, want one", len(listed))
			}
			got := listed[0]
			if got.UserAgent != tc.wantUA || got.Remote != tc.remote {
				t.Errorf("the session lists browser %q (%d bytes) from %q, want %q (%d bytes) from %q",
					got.UserAgent, len(got.UserAgent), got.Remote, tc.wantUA, len(tc.wantUA), tc.remote)
			}
			if !utf8.ValidString(got.UserAgent) {
				t.Errorf("the kept browser %q was cut inside a character", got.UserAgent)
			}
		})
	}
}
