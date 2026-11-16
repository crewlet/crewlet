package artifactrender

import (
	"context"
	"sync"

	"github.com/crewlet/crewlet/internal/iam"
)

// lineages is the render handler's credential resolver: each live render's
// lineage, standing for the principal it renders as for as long as the render
// runs, and nothing else. A ticket minted through any other credential, or
// through a render that has ended, stands for nobody here; and the public
// origin's resolver (internal/api/auth's Lineages) knows no render lineage,
// so a render's ticket opens nothing there.
type lineages struct {
	mu   sync.Mutex
	live map[string]grant
}

// grant is one live render's standing.
type grant struct {
	principal iam.Principal
	admit     func(ctx context.Context) iam.Resolution
}

func newLineages() *lineages { return &lineages{live: map[string]grant{}} }

func (l *lineages) add(lineage string, p iam.Principal, admit func(context.Context) iam.Resolution) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live[lineage] = grant{principal: p, admit: admit}
}

func (l *lineages) remove(lineage string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.live, lineage)
}

// Principal answers who a render's lineage stands for, decided again as its
// tool decided it on every document request.
func (l *lineages) Principal(ctx context.Context, lineage string) (iam.Principal, iam.Resolution) {
	l.mu.Lock()
	g, ok := l.live[lineage]
	l.mu.Unlock()
	if !ok {
		return iam.Principal{}, iam.Anonymous
	}
	switch how := g.admit(ctx); how {
	case iam.Resolved:
		return g.principal, iam.Resolved
	case iam.Unknown:
		return iam.Principal{}, iam.Unknown
	default:
		return iam.Principal{}, iam.Anonymous
	}
}
