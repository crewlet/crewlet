package googlegrowth

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// A WRONG ROLE IS SAID ENTRY BY ENTRY: a seat with Editor on one property
// and Viewer on another reads as holding Viewer where it falls short, never
// as holding both there.
//
// Mutation: gather every entry's roles into one list, and the sentence says
// it holds Editor and Viewer on the property it holds only Viewer on.
func TestAWrongRoleIsSaidEntryByEntry(t *testing.T) {
	t.Parallel()
	id := &Identity{Entry: "property", Entries: "properties", Roles: Roles{Ladder: []RoleName{
		{ID: "viewer", Name: "Viewer"}, {ID: "editor", Name: "Editor"}}}}
	v := SeatAccess{Name: "Google Analytics", Identity: func() *Identity { return id }}
	got := v.verdict(seataccess.IdentityCheck{Needs: "Editor"}, "Analyst", "a@id.example", Need{Roles: []string{"editor"}},
		[]Reached{{Entry: "1", OK: true, Read: true, Roles: []string{"editor"}},
			{Entry: "2", OK: true, Read: true, Roles: []string{"viewer"}}})
	if got.Status != seataccess.IdentityWrongRole || got.Role != "Viewer" ||
		!strings.Contains(got.Said, "holds Viewer on 2;") || strings.Contains(got.Said, "Editor on 2") {
		t.Errorf("verdict %+v", got)
	}
}
