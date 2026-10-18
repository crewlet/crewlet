package artifactsapi

import (
	"context"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// WHAT A READER IS SHOWN, by their role (03 section 9): the owner and editors
// see the audience list, the elevated reads and the share changes in full; a
// viewer and a commenter see the owner and how many shares there are, and the
// history of what was published, without who it was shared with. ONE SHAPING
// for every surface that answers an artifact's head or history, the REST
// routes and the socket's questions alike.

// Artifact is one artifact's head as a reader is shown it.
type Artifact struct {
	artifactsdomain.Head

	// Role is the reader's own role on the artifact.
	Role artifacts.Role `json:"role"`

	// ShareCount is how many shares the artifact carries, which a reader
	// below editor is shown in place of the list.
	ShareCount int `json:"share_count"`

	// Elevated reports a read only the admin path admitted, which the
	// dashboard says is recorded: the reader holds no relation to it.
	Elevated bool `json:"elevated,omitempty"`
}

// sharesShown reports whether a reader at role sees an artifact's audience.
func sharesShown(role artifacts.Role) bool { return role.Includes(artifacts.RoleEditor) }

// shapeHead is head as a reader at role is shown it.
func shapeHead(head artifactsdomain.Head, role artifacts.Role, elevated bool) Artifact {
	out := Artifact{Head: head, Role: role, ShareCount: len(head.Shares), Elevated: elevated}
	if !sharesShown(role) {
		out.Shares = nil
	}
	return out
}

// Detail is one artifact as its own page reads it: the head, and what the
// reader may do there.
type Detail struct {
	Artifact

	// Writer reports whether the reader may publish a new version, which
	// asks the artifacts app beside the role.
	Writer bool `json:"writer"`
}

// detail is the reader's view of the artifact a admitted.
func (g Gate) detail(ctx context.Context, a Admission) (Detail, error) {
	d := authz.Decide(ctx, a.Principal, authz.ActionArtifactPublish, g.object(a.Head), g.Chart, g.now())
	if d.Unknown() {
		return Detail{}, d.Err
	}
	return Detail{Artifact: shapeHead(a.Head, a.Role(), a.Elevated()), Writer: d.Allowed}, nil
}

// shapeHistory is a history page as a reader at role is shown it: below
// editor, without the elevated reads and without the audience of a share
// change.
func shapeHistory(page artifactsdomain.HistoryPage, role artifacts.Role) artifactsdomain.HistoryPage {
	if sharesShown(role) {
		return page
	}
	out := artifactsdomain.HistoryPage{After: page.After,
		Entries: make([]artifactsdomain.HistoryEntry, 0, len(page.Entries))}
	for _, e := range page.Entries {
		switch artifactsdomain.OpKind(e.Op) {
		case artifactsdomain.OpReadElevated:
			continue
		case artifactsdomain.OpShare, artifactsdomain.OpUnshare:
			e.Summary.Audience, e.Summary.Role = "", ""
		}
		out.Entries = append(out.Entries, e)
	}
	return out
}

// seenByShown reports whether a reader at role is shown who has opened an
// artifact under the company's settings (D21): the owner and editors, and only
// while the company's `artifacts.seen_by` is editors.
func seenByShown(settings config.CompanyArtifacts, role artifacts.Role) bool {
	return settings.SeenByOrDefault() == config.SeenByEditors && role.Includes(artifacts.RoleEditor)
}

// SeenBy is who has opened an artifact, as a reader is shown it: the list, or
// Shown false and nothing for a reader the company's setting withholds it
// from, which is an answer rather than a refusal (the dashboard leaves the
// section out).
type SeenBy struct {
	Shown bool                   `json:"shown"`
	Reads []artifactsdomain.Read `json:"reads"`
}

// SeenByReads is who has opened an artifact.
type SeenByReads interface {
	SeenBy(ctx context.Context, id artifacts.ID) ([]artifactsdomain.Read, error)
}

// SeenBy answers who has opened artifact id, as p is shown it.
func (g Gate) SeenBy(ctx context.Context, reads SeenByReads, p iam.Principal, id artifacts.ID) (SeenBy, error) {
	a, err := g.Read(ctx, p, id, 0)
	if err != nil {
		return SeenBy{}, err
	}
	if !seenByShown(g.settings(), a.Role()) {
		return SeenBy{Reads: []artifactsdomain.Read{}}, nil
	}
	list, err := reads.SeenBy(ctx, id)
	if err != nil {
		return SeenBy{}, err
	}
	if list == nil {
		list = []artifactsdomain.Read{}
	}
	return SeenBy{Shown: true, Reads: list}, nil
}

// Detail answers artifact id's head as p is shown it: [Gate.Read] and the
// shaping.
func (g Gate) Detail(ctx context.Context, p iam.Principal, id artifacts.ID) (Detail, error) {
	a, err := g.Read(ctx, p, id, 0)
	if err != nil {
		return Detail{}, err
	}
	return g.detail(ctx, a)
}

// History shapes a history page for the reader a admitted.
func (a Admission) History(page artifactsdomain.HistoryPage) artifactsdomain.HistoryPage {
	return shapeHistory(page, a.Role())
}
