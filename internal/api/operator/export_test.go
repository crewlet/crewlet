package operator

import (
	"context"

	"github.com/crewlet/crewlet/internal/tools"
)

// Dispatch is the catalogue's own dispatch — the path every transport takes
// into a tool — so a test can hold a transport's answer against it.
func (s *Server) Dispatch(ctx context.Context, name string,
	args map[string]any) (tools.Result, bool, error) {

	return s.catalogue.call(ctx, name, args)
}
