package queries

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// FileReader is the read side of a project's files this surface calls.
type FileReader interface {
	Files(ctx context.Context, q tracker.FileQuery) (tracker.FileListing, error)
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (tracker.FileDetail, error)
}

// workFiles answers a page of one project's files.
func (s Sources) workFiles(ctx context.Context, p Params) (any, error) {
	project := strings.TrimSpace(p.String("project"))
	if project == "" {
		return nil, badParams("project", "", []string{"<KEY>"})
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	listing, err := s.Files.Files(ctx, tracker.FileQuery{
		Project: project, Folder: p.String("folder"), After: p.String("after"),
		Limit: p.Int("limit", 0), Removed: p.Bool("removed", false), Freshness: fresh,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"project": tracker.ProjectKey(project), "files": listing.Files,
		"read_level": listing.Level, "log_seq": listing.LogSeq,
		"applied_through": listing.AppliedThrough, "complete": listing.Complete,
	}
	if listing.Next != "" {
		out["next"] = listing.Next
	}
	if listing.LogLag != nil {
		out["log_lag"] = *listing.LogLag
	}
	if listing.Incomplete != nil {
		out["incomplete"] = listing.Incomplete
	}
	return out, nil
}
