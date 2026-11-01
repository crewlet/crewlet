package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/worklet"
)

// searchCopies is this node's per-domain verdict as a search participant asks
// it (ADR-0072): the rows a source is indexed from are its own domain's — a
// page's the knowledge base's, a task's the tracker's — and the vectors are
// theirs.
//
// A SEARCH SLICE IS NOT AN ESTATE OPERATION, so the router's own gate never saw
// it: a data node whose tracker or knowledge-base copy was wrong answered its
// bucket range from an index over rows it had stopped serving, and the
// coordinator merged that answer under complete coverage.
type searchCopies struct{ local *localEstate }

// sourceDomains is the domain each search source's rows are applied by.
var sourceDomains = map[search.Source]string{
	search.SourcePage: doclet.Domain{}.Name(),
	search.SourceTask: worklet.Domain{}.Name(),
}

// Serves reports whether this node's copy of the domain src is indexed from is
// in service. A source this build maps to no domain is no copy it can vouch
// for.
func (c searchCopies) Serves(ctx context.Context, src search.Source) bool {
	domain, known := sourceDomains[src]
	return known && c.local.serves(ctx, domain)
}

// VectorsServe reports whether this node's copy of the vectors is in service.
func (c searchCopies) VectorsServe(ctx context.Context) bool {
	return c.local.serves(ctx, search.Domain{}.Name())
}

// copies is this node's verdict as a search participant reads it — nil, never
// a typed nil, on a node that judges no copy of its own.
func (e *Engine) copies() search.Copies {
	if e.local == nil {
		return nil
	}
	return searchCopies{local: e.local}
}
