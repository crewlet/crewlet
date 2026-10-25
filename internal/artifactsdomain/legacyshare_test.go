package artifactsdomain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SHARE STORED BEFORE THE GRANT RULE STAYS THE OWNER'S TO ACT ON. A build
// before [artifacts.Audience.ValidateGrant] wrote `seat:token:maya`, a login
// named as a seat, which reaches nobody. The owner still sees it among the
// artifact's shares, may change its role, and withdraws it by its text form; it
// is never counted in a listing it is then withheld from; and a NEW share to a
// login is refused. A record the engine could neither honour nor remove was a
// dead end on every deployment that held one.
func TestAShareStoredBeforeTheGrantRuleIsListedAndWithdrawn(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	bob := person("bob", artifacts.RoleOwner)
	id := n.create(bob, "Shared with a login", upload(t, "index.html", "<p>1</p>")).Artifact
	legacy := artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "token:maya"}
	if _, err := artifactsdomain.ShareBeforeTheGrantRule(t.Context(), n.writer, bob, id,
		artifacts.Share{Audience: legacy, Role: artifacts.RoleViewer}); err != nil {
		t.Fatalf("plant the legacy share: %v", err)
	}
	shares := func() []artifacts.Share {
		t.Helper()
		head, _, err := n.reader.Artifact(t.Context(), id, statelog.Freshness{Level: statelog.ReadSession})
		if err != nil {
			t.Fatalf("read the artifact: %v", err)
		}
		return head.Shares
	}
	held := func(role artifacts.Role) bool {
		return slices.ContainsFunc(shares(), func(s artifacts.Share) bool {
			return s.Audience == legacy && s.Role == role
		})
	}
	if !held(artifacts.RoleViewer) {
		t.Fatalf("the legacy share is not among the artifact's shares: %+v", shares())
	}

	// Never counted where it is withheld: the login's own Shared listing.
	page, err := n.reader.List(t.Context(), artifactsdomain.Viewer{Record: "token:maya"},
		artifactsdomain.ListQuery{Scope: artifactsdomain.ScopeShared})
	if err != nil || page.Total != 0 || len(page.Contents) != 0 {
		t.Errorf("the login's Shared listing counts %d and lists %d (%v)", page.Total, len(page.Contents), err)
	}

	// A new share to a login is refused; the stored one may change its role.
	other := artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "token:sam"}
	if _, err := n.writer.Share(t.Context(), bob, id,
		artifacts.Share{Audience: other, Role: artifacts.RoleViewer}); !errors.Is(err, artifactsdomain.ErrInvalid) {
		t.Errorf("a new share to a login answered %v, want ErrInvalid", err)
	}
	if _, err := n.writer.Share(t.Context(), bob, id,
		artifacts.Share{Audience: legacy, Role: artifacts.RoleCommenter}); err != nil || !held(artifacts.RoleCommenter) {
		t.Errorf("changing the stored share's role: %v, shares %+v", err, shares())
	}

	// Withdrawn by its text form, as the dialog's Remove sends it.
	parsed, err := artifacts.ParseAudience(legacy.String())
	if err != nil {
		t.Fatalf("the stored audience no longer parses: %v", err)
	}
	if _, err := n.writer.Unshare(t.Context(), bob, id, parsed); err != nil {
		t.Fatalf("withdraw the legacy share: %v", err)
	}
	if got := shares(); len(got) != 0 {
		t.Errorf("after the withdrawal the artifact still shares with %+v", got)
	}
}
