package queries

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
)

// AdvisedOfAtlas is advisedOfAtlas for the external suite, which holds the
// atlas's harness: the `advisor` question admits only an administrator, who
// holds every app, so the reader without the atlas app is asked for here.
func (s Sources) AdvisedOfAtlas(ctx context.Context, answer advisor.Answer, now time.Time) (advisor.Answer, error) {
	return s.advisedOfAtlas(ctx, answer, now)
}
