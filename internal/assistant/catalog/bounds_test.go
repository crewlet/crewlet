package catalog_test

import (
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
)

// bound is how one read's default answer is held to a size that does not
// grow with the company's data, so no default read is cut at what one step
// reads (ADR-0061).
type bound struct {
	kind string
	// why names the cap a route holds itself to, or what bounds a set the
	// company configures, where kind is boundCapped or boundConfig.
	why string
	// record is the argument that names the one record a boundRecord read
	// is of, which its path must carry as a segment of its own.
	record string
	// asks is what the read must send besides, where the route's own default
	// answer is past what one step reads and the read bounds it: a seat's
	// read asks for the seat without its call history.
	asks map[string]string
}

// The kinds of bound, each checked against the request the read sends.
const (
	// boundPage: the read asks for one page (`limit`, `max-keys`) and the
	// card says when more lie past it.
	boundPage = "page"
	// boundWindow: the read asks for a window of days.
	boundWindow = "window"
	// boundRecord: the read is of one record, named in its path.
	boundRecord = "record"
	// boundCapped: the route holds its own answer to a fixed size, named in
	// why.
	boundCapped = "cap"
	// boundConfig: one row per thing the company document configures (a
	// seat, a unit, a space, an integration, a node), which grows with the
	// configuration rather than with the work.
	boundConfig = "config"
)

// readBounds is every read tool's card variant and its bound. A NEW READ IS
// DECLARED HERE OR THE BUILD FAILS: a read that reads a route whole (the
// event log, a seat's transcripts, a growing list) was cut at the step's
// bound and gave the model nothing, and this is where that is decided once.
var readBounds = map[string]bound{
	"search_work": {kind: boundCapped,
		why: "a ranked search answers its best matches, at most 25, never a page of a list"},
	"list_tasks":             {kind: boundPage},
	"get_task":               {kind: boundRecord, record: "item"},
	"get_task_thread":        {kind: boundPage},
	"get_task_cost":          {kind: boundPage},
	"get_my_work/assigned":   {kind: boundCapped, why: "my-work answers seven lists of at most 20 each"},
	"get_my_work/priorities": {kind: boundCapped, why: "my-work answers seven lists of at most 20 each"},
	"get_my_work/asked":      {kind: boundCapped, why: "my-work answers seven lists of at most 20 each"},
	"get_my_work/inbox":      {kind: boundPage},
	"list_projects":          {kind: boundPage},
	"list_projects/one":      {kind: boundRecord, record: "key"},
	"get_workload":           {kind: boundConfig, why: "one row per seat"},
	"list_decisions":         {kind: boundCapped, why: "decisions answers at most 50, newest first, with its total"},
	"list_agents":            {kind: boundConfig, why: "one row per agent seat"},
	"get_agent/activity":     {kind: boundWindow},
	"get_agent/detail": {kind: boundRecord, record: "handle",
		asks: map[string]string{"history": "false"}},
	"get_org_chart/chart":        {kind: boundConfig, why: "the company's units and seats"},
	"get_org_chart/seats":        {kind: boundConfig, why: "one row per human seat"},
	"list_turns":                 {kind: boundPage},
	"list_executors":             {kind: boundConfig, why: "the runs active now, bounded by the sandboxes the company runs"},
	"get_spend/agent":            {kind: boundWindow},
	"get_spend/model":            {kind: boundWindow},
	"get_spend/phase":            {kind: boundWindow},
	"get_spend_series/by_group":  {kind: boundWindow},
	"get_spend_series/over_time": {kind: boundWindow},
	"list_budgets/agents":        {kind: boundConfig, why: "one row per seat and the company's windows"},
	"list_budgets/company":       {kind: boundConfig, why: "one row per seat and the company's windows"},
	"search_knowledge":           {kind: boundCapped, why: "the knowledge search answers its top hits, never the corpus"},
	"list_spaces":                {kind: boundConfig, why: "one row per space"},
	"get_page":                   {kind: boundRecord, record: "page"},
	"get_company_feed":           {kind: boundPage},
	"list_artifacts":             {kind: boundPage},
	"get_artifact":               {kind: boundRecord, record: "artifact"},
	"list_integrations":          {kind: boundConfig, why: "one row per integration the engine offers"},
	"get_fleet_health/backups":   {kind: boundCapped, why: "the backup history is one boundPage of 100"},
	"get_fleet_health/mcp_servers": {kind: boundConfig,
		why: "one row per MCP server the company configures"},
	"get_fleet_health/nodes": {kind: boundConfig, why: "one row per node of the fleet"},
	"list_config_changes": {kind: boundCapped,
		why: "config_audit answers the newest revisions, at most 50, as a bare list with no cursor"},
	"get_company_principles": {kind: boundConfig, why: "the company's one principles document"},
	"list_chat_rooms":        {kind: boundPage},
	"get_chat_room":          {kind: boundRecord, record: "channel"},
	"read_chat_messages":     {kind: boundPage},
	"get_chat_thread":        {kind: boundPage},
	"list_chat_mentions":     {kind: boundPage},
	"search_chat": {kind: boundCapped,
		why: "a ranked search answers its best matches, at most 50, never a page of a list"},
}

// segments are a request path's segments, each unescaped.
func segments(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if s, err := url.PathUnescape(seg); err == nil && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// pages reports whether a card reads the answer's own word that rows or items
// lie past its page ([cards.Table.More], [cards.Items.More]).
func pages(spec cards.Spec) bool {
	switch {
	case spec.Table != nil:
		return spec.Table.More != ""
	case spec.Entity != nil && spec.Entity.Items != nil:
		return spec.Entity.Items.More != ""
	}
	return false
}

// variantArgs are the arguments that select one card variant of a read, with
// every required argument at its example and nothing else, so the request is
// the read's default.
func variantArgs(t *testing.T, tool catalog.Tool, variant string) catalog.Args {
	t.Helper()
	example := assistanttest.ExampleArgs(t, tool.Parameters)
	args := catalog.Args{}
	required, _ := tool.Parameters["required"].([]string)
	for _, name := range required {
		args[name] = example[name]
	}
	switch variant {
	case "":
	case "one":
		args["key"] = "ENG"
	case "detail":
		args["handle"] = "qa"
	default:
		props, _ := tool.Parameters["properties"].(map[string]any)
		for name, p := range props {
			if enum, _ := p.(map[string]any)["enum"].([]string); slices.Contains(enum, variant) {
				args[name] = variant
			}
		}
	}
	return args
}

// EVERY DEFAULT READ IS BOUNDED. Each read tool's every card variant declares
// how its default answer is held to a size that does not grow with the
// company's work, and the request it sends is held to that: a page asks for
// one, a window for its days, a record names it, and a route whose own
// default is past a step is asked for less. These are the assistant's reads:
// the routes' defaults are their own (ADR-0061). Mutation: drop a read's
// limit or a seat read's history=false, or add a read without declaring it,
// and this fails.
func TestEveryDefaultReadIsBounded(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindRead || tool.Infer {
			continue
		}
		variants := make([]string, 0, len(tool.Cards))
		for v := range tool.Cards {
			variants = append(variants, v)
		}
		sort.Strings(variants)
		for _, v := range variants {
			name := tool.Name
			if v != "" {
				name += "/" + v
			}
			seen[name] = true
			b, ok := readBounds[name]
			if !ok {
				t.Errorf("%s declares no bound: say in readBounds how its default answer is held "+
					"to a size that does not grow with the company's work", name)
				continue
			}
			args := variantArgs(t, tool, v)
			req, err := tool.Request(args)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			q := req.Query
			switch b.kind {
			case boundPage:
				if q.Get("limit") == "" && q.Get("max-keys") == "" {
					t.Errorf("%s is a page and asks %s?%s with no limit", name, req.Path, q.Encode())
				}
				// A PAGE SAYS IT IS ONE: the card reads the answer's own word
				// that rows lie past it, or the person and the model take
				// the first page for the whole.
				if !pages(tool.Cards[v]) {
					t.Errorf("%s is a page and its card names no More, so a full page "+
						"reads as the whole", name)
				}
			case boundWindow:
				if q.Get("days") == "" && q.Get("since") == "" {
					t.Errorf("%s is a window and asks %s?%s with no days", name, req.Path, q.Encode())
				}
			case boundRecord:
				// THE PATH NAMES THE RECORD THE CALL ASKED FOR, as a segment
				// of its own: a read that ignored its argument and asked a
				// list route would pass a count of slashes.
				want := args.String(b.record)
				if b.record == "" || want == "" || !slices.Contains(segments(req.Path), want) {
					t.Errorf("%s is one record, asked by %s=%q, and asks %s, which names "+
						"it in no segment", name, b.record, want, req.Path)
				}
			case boundCapped, boundConfig:
				if b.why == "" {
					t.Errorf("%s is bounded by %s and says not how", name, b.kind)
				}
			default:
				t.Errorf("%s declares the bound %q, which is none of the kinds", name, b.kind)
			}
			for param, want := range b.asks {
				if got := q.Get(param); got != want {
					t.Errorf("%s asks %s?%s, want %s=%s", name, req.Path, q.Encode(), param, want)
				}
			}
		}
	}
	for name := range readBounds {
		if !seen[name] {
			t.Errorf("readBounds names %s, which is no read's variant", name)
		}
	}
}
