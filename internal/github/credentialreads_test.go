package github_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/org"
)

// THE READS ARE THE SCAN'S. CredentialReads names what CredentialOf reads on
// the reconcile's seats — each spelling it tries, in the one block it tries —
// and nothing else: not the block's other keys, not a tool server whose name
// only starts with the vendor's, and nothing of a person's seat. A running
// node is asked for exactly these, each a reveal logged by name.
func TestCredentialReadsAreWhatTheScanReads(t *testing.T) {
	t.Parallel()
	writer := &org.Role{Name: "Writer", DeclaredHandle: "writer", MCPEnv: map[string]map[string]string{
		github.SeatEnv:               {"GITHUB_TOKEN": "${TOKEN_WRITER}", "GITHUB_HOST": "${GITHUB_HOST}"},
		github.SeatEnv + ".internal": {"GITHUB_TOKEN": "${GHE_TOKEN}"},
	}}
	reviewer := &org.Role{Name: "Reviewer", DeclaredHandle: "reviewer", MCPEnv: map[string]map[string]string{
		github.SeatEnv: {"Authorization": "Bearer ${TOKEN_REVIEWER}"},
	}}
	founder := &org.Role{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
		MCPEnv: map[string]map[string]string{github.SeatEnv: {"GITHUB_TOKEN": "${FOUNDER_TOKEN}"}}}
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{writer, founder},
		Units: []*org.Unit{{Name: "Platform", Roles: []*org.Role{reviewer}}}}

	want := []string{"TOKEN_REVIEWER", "TOKEN_WRITER"}
	if got := github.CredentialReads(o); !slices.Equal(got, want) {
		t.Errorf("CredentialReads = %v, want %v", got, want)
	}
	// Whatever the values resolve to, every name the scan reads is one of
	// them: an empty answer walks every spelling, a held one stops at it.
	for _, answer := range []string{"", "ghp_held"} {
		for seat := range o.AllRoles() {
			github.CredentialOf(seat, func(value string) string {
				for _, name := range envref.Names(value) {
					if !slices.Contains(want, name) {
						t.Errorf("the scan read ${%s} on %s, which CredentialReads omits",
							name, seat.Handle())
					}
				}
				if value == "" {
					return ""
				}
				return answer
			})
		}
	}
	if got := github.CredentialReads(nil); got != nil {
		t.Errorf("CredentialReads(nil) = %v, want none", got)
	}
}
