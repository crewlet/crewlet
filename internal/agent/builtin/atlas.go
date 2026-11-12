package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// AtlasSearchTool reads the company's atlas: its entries, or those a query or a
// kind names. Reading it takes the atlas app (ADR-0127).
const AtlasSearchTool = string(authz.ActionAtlasSearch)

// The listing's page: fifty entries by default, a screen of them, and at most
// five hundred, the most a person reads through before narrowing by kind. Each
// page is cut further, whole entries, wherever it outgrows one tool answer.
const (
	defaultAtlasList = 50
	maxAtlasList     = 500
)

// AtlasSettings is the settings an atlas read reads: the document this node runs
// (read only through its redacting copy), its revision, empty where this node
// cannot tell, and the addresses the deployment answers at.
type AtlasSettings struct {
	Company    *config.Company
	Revision   string
	Deployment derive.Deployment
}

// AtlasDeps are what the atlas tools and questions read the atlas through.
// The zero value serves none of them.
type AtlasDeps struct {
	// Settings is the company settings this node runs, read per call, and
	// false where it runs none yet. Nil omits every atlas tool.
	Settings func(ctx context.Context) (AtlasSettings, bool)
	// Records is every record source of the engine's this node reaches, read
	// per call, Gitlet aside: each caller adds Gitlet as itself. A source this
	// node does not reach is nil, which the read reports as unread.
	Records func(ctx context.Context) gather.Sources
}

// AtlasRead is one read of the atlas for one caller: Gitlet read as asker
// where reads is set, every record this node reaches, and the settings it
// runs. False where it runs no company yet.
func (d AtlasDeps) AtlasRead(ctx context.Context, reads gather.GitletReads, asker gitlet.Principal,
	fresh statelog.Freshness, now time.Time) (gather.Answer, bool) {
	if d.Settings == nil {
		return gather.Answer{}, false
	}
	settings, running := d.Settings(ctx)
	if !running {
		return gather.Answer{}, false
	}
	var sources gather.Sources
	if d.Records != nil {
		sources = d.Records(ctx)
	}
	sources.Gitlet = reads
	return gather.Read(ctx, sources, gather.Request{
		Company: settings.Company, Revision: settings.Revision, Deployment: settings.Deployment,
		Asker: asker, Fresh: fresh, Now: now,
	}), true
}

// atlasSearch is atlas_search: the atlas as its caller may read it. Gitlet is
// read as the caller and only where they may read code (ADR-0110), since the
// atlas never opens another app's records (ADR-0127).
type atlasSearch struct {
	deps      AtlasDeps
	code      CodeDeps
	authorize Authorizer
}

var _ tools.SeatCallable = (*atlasSearch)(nil)

func (t *atlasSearch) Name() string { return AtlasSearchTool }

func (t *atlasSearch) Description() string {
	return "Search the company's atlas: the map of everything it runs on (vendor accounts, domains, " +
		"hostnames, servers, data stores, repositories, monitoring, tool servers and models). Lists the " +
		"entries a `query` or a `kind` names, each with its facts, where each fact came from, and what it " +
		"depends on. Repositories are listed only for a caller who uses Gitlet."
}

func (t *atlasSearch) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Part of an entry's name or key, or its id. Omit for every entry.",
			},
			"kind": map[string]any{
				"type":        "string",
				"enum":        atlasKinds(),
				"description": "Only entries of this kind.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many entries to return, 1..%d (default %d).",
					maxAtlasList, defaultAtlasList),
			},
		},
	}
}

func (t *atlasSearch) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// atlasList is atlas_search's answer: the entries, how many matched, which
// sources were read in full, and how many values were left out for being
// shaped like a credential, counted and never shown.
type atlasList struct {
	Entries  []atlas.Entry     `json:"entries"`
	Total    int               `json:"total"`
	Coverage []gather.Coverage `json:"coverage"`
	Withheld int               `json:"withheld,omitempty"`
}

func (t *atlasSearch) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	kind := atlas.Kind(strings.TrimSpace(argString(args, "kind")))
	if kind != "" && !kind.Valid() {
		return failed(fmt.Sprintf("%s: `kind` is %s, not %q.", t.Name(), words.Or(atlas.Kinds),
			clip(string(kind)))), nil
	}
	limit := argInt(args, "limit", defaultAtlasList)
	if limit < 1 || limit > maxAtlasList {
		return failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), maxAtlasList)), nil
	}
	settings, running := t.deps.Settings(ctx)
	if !running {
		return failed(fmt.Sprintf("%s: this node runs no company yet, so there is no atlas to read. "+
			"Try again once its settings are applied.", t.Name())), nil
	}
	sources, asker, why := t.gitlet(ctx, turn)
	answer := gather.Read(ctx, sources, gather.Request{
		Company: settings.Company, Revision: settings.Revision, Deployment: settings.Deployment,
		Asker: asker, Fresh: seatRead, Now: time.Now().UTC(),
	})
	coverage := gitletSaid(answer.Coverage, why)
	matched := atlasMatches(answer.View.Entries, strings.TrimSpace(argString(args, "query")), kind)
	shown := matched[:min(limit, len(matched))]
	rendered, err := fitPages(len(shown), func(k int) (json.RawMessage, error) {
		return json.Marshal(atlasList{Entries: shown[:k], Total: len(matched),
			Coverage: coverage, Withheld: len(answer.Withheld)})
	})
	if err != nil {
		return labelFault(ctx, t.Name(), err), nil
	}
	return compactAnswer(rendered, "Ask for fewer with `limit`, or narrow by `kind` or `query`.")
}

// gitlet is the Gitlet source this call reads and who it reads as: none where
// this surface serves no code or the caller (or whoever a seat's turn works
// for) may not read it, and then why says which, for the answer's coverage.
func (t *atlasSearch) gitlet(ctx context.Context, turn *turnctx.Turn) (
	src gather.Sources, asker gitlet.Principal, why string) {

	if !t.code.serves() || t.authorize == nil {
		return gather.Sources{}, gitlet.Principal{}, ""
	}
	switch err := t.authorize(ctx, authz.ActionCodeRead, authz.Object{}); {
	case errors.Is(err, ErrUndecidable):
		return gather.Sources{}, gitlet.Principal{}, "whether you may read the company's code could not be " +
			"decided right now, so no repository was read; ask again shortly"
	case err != nil:
		return gather.Sources{}, gitlet.Principal{}, "you do not use Gitlet, so no repository was read"
	}
	caller, refusal := t.code.callerOn(ctx, turn, t.Name(), authz.ActionCodeRead)
	if refusal != nil {
		return gather.Sources{}, gitlet.Principal{}, "whoever this turn works for may not read the company's " +
			"code now, so no repository was read"
	}
	return gather.Sources{Gitlet: atlasGitlet{code: t.code, caller: caller}}, caller.asker, ""
}

// gitletSaid puts why on the Gitlet source's coverage, where the call did not
// read it for a reason of its own rather than the read's.
func gitletSaid(coverage []gather.Coverage, why string) []gather.Coverage {
	if why == "" {
		return coverage
	}
	out := slices.Clone(coverage)
	for i := range out {
		if out[i].Source == gather.SourceGitlet {
			out[i].Complete, out[i].Reason = false, why
		}
	}
	return out
}

// atlasGitlet is Gitlet as one call reads it: every query asked as the
// call's caller, with its ceiling, through [CodeDeps.read].
type atlasGitlet struct {
	code   CodeDeps
	caller codeCaller
}

func (g atlasGitlet) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return g.code.read(ctx, g.caller, q)
}

// atlasMatches is the entries of kind, or of every kind, whose name or key
// holds query, case folded, or whose id is query; every entry where query is
// empty. In the view's order.
func atlasMatches(entries []atlas.Entry, query string, kind atlas.Kind) []atlas.Entry {
	folded := strings.ToLower(query)
	return slices.DeleteFunc(slices.Clone(entries), func(e atlas.Entry) bool {
		if kind != "" && e.Ref.Kind != kind {
			return true
		}
		if folded == "" || e.ID.String() == folded {
			return false
		}
		return !strings.Contains(strings.ToLower(e.Name), folded) &&
			!strings.Contains(strings.ToLower(e.Ref.Key), folded)
	})
}

// atlasKinds is every kind of entry, as the tool's schema names them.
func atlasKinds() []string {
	out := make([]string, len(atlas.Kinds))
	for i, k := range atlas.Kinds {
		out[i] = string(k)
	}
	return out
}
