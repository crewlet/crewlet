package store_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// EVERY TABLE IN THIS FILE ANSWERS "WHO HAS TO AGREE ON IT?" IN WRITING.
//
// This is the rule with every recorded incident in the repository behind it,
// and it is not the one people quote. The quoted rule is that the stream is
// the write-ahead log and this estate is derived state; the rule that keeps
// breaking is its neighbour — anything the COMPANY has to agree on belongs in
// the coordination store, never in a file one process owns exclusively.
//
// Migrations 0010 through 0013 and 0028 are five post-mortems of it, each
// discovering that the last repair was incomplete — nine tables between them:
//
//   - 0010 moved `webhook_deliveries`, `rate_limits`, `config_activations`,
//     `config_apply_status` and `turn_completions`. A vendor retrying a
//     delivery reached whichever ingress node the load balancer picked and
//     woke the same seat twice; four nodes ran four rate valves, so a seat
//     capped at five a second emitted twenty; each node read its own apply
//     status and drew a fleet of one; a redelivery that landed on a peer
//     found no completion row and ran the turn a second time.
//   - 0011 took the token counter.
//   - 0012 took `a2a_channels`, and says in its own text that it "was the
//     last of the tables migration 0010 should have taken".
//   - 0013 said it again about the next one.
//   - 0028 said it a fourth time, about `chat_thread_follows`: an inbound
//     chat message is claimed by ONE node of a competing consumer group, so
//     a follow recorded on the node that took the mention was invisible to
//     the node that took the reply.
//
// Every one of those tables had a migration describing shared state and a
// placement that was per node. Nothing compared the two, because the rule was
// prose and prose is not compared to anything.
//
// # What this gate does, and what it deliberately cannot do
//
// It cannot tell that a table is in the wrong estate — that is a judgement
// about what the table MEANS, and no walk over DDL reaches it. What it can do
// is make the judgement un-skippable: every table this estate declares must
// appear below with an answer and a reason, and the check is two-sided, so a
// table added without one fails and an entry for a table that no longer exists
// fails too. A new table's author writes the sentence, and a reviewer reads it
// in the diff next to the migration. That is the whole mechanism, and it is the
// one thing the four migrations above did not have.
//
// Nothing here binds the REPLICATED estate, which has its own gate one file
// over: [TestOnlyTheApplierWritesTheReplicatedEstate].
func TestEveryNodeTableSaysWhoHasToAgreeOnIt(t *testing.T) {
	t.Parallel()

	declared := tablesIn(t, store.EstateNode)
	if len(declared) == 0 {
		t.Fatal("no tables were derived from the node estate's schema; this " +
			"guard is watching an estate it cannot see and would pass " +
			"whatever the tree did")
	}

	placed := map[string]bool{}
	for _, p := range nodeEstatePlacements {
		if placed[p.Table] {
			t.Errorf("%s is declared twice, so one of the two reasons is "+
				"unread and neither is the answer", p.Table)
		}
		placed[p.Table] = true
		if strings.TrimSpace(p.Why) == "" {
			t.Errorf("%s has no reason — the point of the entry is the "+
				"sentence, not the row", p.Table)
		}
	}

	var undeclared []string
	for name := range declared {
		if !placed[name] {
			undeclared = append(undeclared, name)
		}
	}
	slices.Sort(undeclared)
	for _, name := range undeclared {
		t.Errorf("%s is a table in the node's own file and nothing says who "+
			"has to agree on it.\n"+
			"\tThis file is owned exclusively by one process, so a fact the "+
			"COMPANY has to agree on cannot live here: every node would read "+
			"its own copy and draw a fleet of one. That mistake has been made "+
			"five times and repaired in four migrations — read "+
			"internal/store/schema/node/0010 before answering. If the answer "+
			"is genuinely 'this node alone', add it to nodeEstatePlacements "+
			"with the reason; if it is 'the whole company', it belongs in "+
			"internal/coord instead.", name)
	}

	// AND THE OTHER DIRECTION, which is what stops this list becoming a
	// museum: an entry whose table left the estate is a reason nobody can
	// check, sitting where the next reader will take it for a live one.
	var stale []string
	for _, p := range nodeEstatePlacements {
		if !declared[p.Table] {
			stale = append(stale, p.Table)
		}
	}
	slices.Sort(stale)
	for _, name := range stale {
		t.Errorf("%s is declared here and is not a table of the node estate "+
			"— it moved, or it was dropped. Delete the entry, so the list "+
			"stays a description of this file rather than of one it used to be",
			name)
	}

	t.Logf("node estate: %d table(s), all placed", len(declared))
}

// placement is one node-estate table and the answer it gives.
type placement struct {
	// Table is the name the schema declares.
	Table string

	// Why answers "who has to agree on this, and what would a peer's copy
	// get wrong?" — in terms of the FACT rather than of the code. "The
	// learning subsystem writes it" is not an answer; "a seat's own
	// observation, carried between nodes by a compacted changelog rather
	// than agreed on" is.
	Why string
}

// nodeEstatePlacements is every table in the node's own database, with the
// case for it being there rather than in coordination.
//
// ORDERED BY WHAT THEY ARE, not alphabetically, because the groups are the
// argument: four kinds of thing end up in a file one process owns, and a new
// table almost always belongs to one of them.
var nodeEstatePlacements = []placement{
	// -----------------------------------------------------------------
	// This node's own record of what it did. Nobody else's copy is even
	// meaningful: two nodes ran different turns.
	// -----------------------------------------------------------------
	{
		Table: "crewlet_events",
		Why: "The audit log of what THIS node did. A publish listener writes " +
			"it inline on the publishing node, so there is no consumer group " +
			"and no two nodes can write one row. A peer's copy is a different " +
			"node's history, not a stale version of this one.",
	},
	{
		Table: "crewlet_event_parties",
		Why:   "The party index over the rows above, and it goes where they go.",
	},
	{
		Table: "conversation_sessions",
		Why: "What a seat already said in one thread, recorded by the node " +
			"that ran the turn. Read back only by that seat's next turn on " +
			"whichever node holds it, deduped on the work key, so a missing " +
			"row costs a repeated summary rather than a wrong answer.",
	},
	{
		Table: "scheduled_runs",
		Why: "This node's audit row for the fires it ran. The half the FLEET " +
			"has to agree on — 'may I start this fire' — is in coordination, " +
			"and the split is argued at internal/schedule/sharedclaim.go: it " +
			"used to be here alone, and a duty moving between nodes gave the " +
			"new holder an empty ledger, so every company got two standups.",
	},

	// -----------------------------------------------------------------
	// What a seat remembers. Best effort by design, and carried between
	// nodes by a compacted changelog rather than agreed on — see
	// internal/learning/memsync, which is why replication is not the
	// answer here and a peer's copy converges rather than arbitrates.
	// -----------------------------------------------------------------
	{
		Table: "agent_diary",
		Why: "A seat's private observations, written by whichever node ran " +
			"its turn and hydrated onto a node before that seat's mailbox " +
			"attaches. Nothing reads another seat's diary, so there is no " +
			"company-wide question for a peer's copy to answer differently.",
	},
	{
		Table: "episodes",
		Why: "One row per completed turn for that seat's own recall. Same " +
			"lifecycle as the diary.",
	},
	{
		Table: "synthesized_skills",
		Why: "Skills a seat drafted for itself. Loaded on demand by that seat " +
			"alone; cross-agent promotion goes through the knowledge base, " +
			"which is replicated, rather than through this table.",
	},
	{
		Table: "synthesized_skill_versions",
		Why: "The refinement history behind the row above, and it goes where " +
			"that row goes.",
	},
	{
		Table: "counterparty_profiles",
		Why: "What one seat has observed about one other party. Keyed on the " +
			"OBSERVER, so it is that seat's opinion rather than a company " +
			"fact — two seats holding different impressions of the same " +
			"person is the correct state, not a divergence.",
	},
	{
		Table: "agent_onboarding_markers",
		Why: "Whether a seat has read its unit's onboarding pages, keyed on " +
			"the derived agent id. Re-onboarding is cheap and the marker " +
			"re-derives from the org chain, so a node that has not seen one " +
			"runs the pass again rather than being wrong.",
	},

	// -----------------------------------------------------------------
	// A local cache of something coordination already decides. The
	// authority is elsewhere; the row here is for history and for reading
	// without a round trip.
	// -----------------------------------------------------------------
	{
		Table: "company_config",
		Why: "The revision payloads this node has seen, kept for its history, " +
			"its diffs and its revert targets. WHICH revision is current is " +
			"the activation pointer in coordination — that half moved out in " +
			"migration 0010 precisely because each node was reading its own " +
			"row and drawing a fleet of one. The local write is best effort.",
	},
	{
		Table: "secret_values",
		Why: "The BOOTSTRAP half of the secret store only: values written " +
			"while the engine was stopped, which internal/fleetsecrets " +
			"migrates onto the coordination store at the next start. The " +
			"company's live credentials are not here.",
	},

	// -----------------------------------------------------------------
	// A per-node OBSERVATION about shared infrastructure. Two nodes
	// legitimately hold different answers, which is what makes a shared
	// copy wrong rather than merely unnecessary.
	//
	// `chat_thread_follows` was in this group and did not belong: two
	// nodes holding different follows is not a legitimate difference, it
	// is the bug. It is in coordination now — see ADR-0003 and node
	// migration 0028.
	// -----------------------------------------------------------------
	{
		Table: "stream_identity",
		Why: "What this node last saw of each stream's identity, which is how " +
			"it detects a recreated stream. Two nodes can legitimately have " +
			"seen different generations, so a shared value would destroy the " +
			"comparison this exists to make.",
	},
	{
		Table: "statelog_adoption",
		Why: "This node's own progress through adopting a replicated estate " +
			"file. It describes a local file operation and means nothing on " +
			"a peer.",
	},

	// -----------------------------------------------------------------
	// An index over rows that live in the other estate. Derived, droppable
	// and rebuilt locally, so every node maintains its own.
	// -----------------------------------------------------------------
	{
		Table: "kb_docs",
		Why: "The lexical search index's document side, built asynchronously " +
			"behind replicated rows and droppable wholesale when the analyzer " +
			"changes. Rebuilding it is a local walk, so a peer's copy buys " +
			"nothing and a stale one would be worse than none.",
	},
	{
		Table: "kb_postings",
		Why: "The inverted list behind the documents above. Same lifecycle: " +
			"derived from replicated rows, rebuilt locally, never agreed on.",
	},
}

// THE NODE ESTATE'S ANSWERS AND THE PUBLISHED PAGE ARE THE SAME LIST.
//
// docs/concepts/architecture.md § "Where state lives" is what an operator
// reads to find out where a fact of theirs is kept, and its first table is a
// hand-maintained enumeration of this estate — which goes stale on the first
// table somebody adds, silently, in the direction that matters least to the
// person who added it and most to the person reading it. This is the guard, in
// the idiom internal/events/categorydoc_test.go established for the event
// categories: a narrative page bound to a declaration, failing both ways.
//
// # The node estate only, and that is deliberate
//
// The page enumerates this estate table by table because a reader is looking
// for one of sixteen named things. It describes the REPLICATED estate by
// family — "the tracker's own tables", "the knowledge base's" — because sixty
// rows would be a schema dump rather than a map, and because a table there is
// reached through a domain rather than named by an operator. Demanding every
// one would make the page worse to read in exchange for a guarantee nobody
// asked for.
//
// What that costs is stated rather than glossed: a new replicated table can
// arrive without the page mentioning it. It is covered by the gate that
// matters for it instead — a write to it fails
// [TestOnlyTheApplierWritesTheReplicatedEstate] unless its author says why.
func TestTheArchitecturePageNamesEveryNodeTable(t *testing.T) {
	t.Parallel()

	page := estatePage(t)
	node, replicated := tablesIn(t, store.EstateNode), tablesIn(t, store.EstateReplicated)

	names := make([]string, 0, len(node))
	for name := range node {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !strings.Contains(page, "`"+name+"`") {
			t.Errorf("%s is a table in this node's own file and %s does not "+
				"name it — an operator looking for where their data lives has "+
				"no way to learn it exists, and nothing else in the tree tells "+
				"them", name, estateDoc)
		}
	}

	// The other direction, over the names the section carries that look
	// like one of ours. A name the page keeps after its table is dropped is
	// a row pointing at nothing, which reads exactly like a live one.
	for _, name := range backtickedTableNames(page) {
		if node[name] || replicated[name] {
			continue
		}
		t.Errorf("%s names `%s` as a table this node holds, and neither "+
			"estate declares it any more — the row points at nothing",
			estateDoc, name)
	}
}

// estateDoc is the operator-facing map of where each fact is kept.
const estateDoc = "docs/concepts/architecture.md"

// The section, and the two subsections inside it that are about the STORE.
const (
	estateHeading  = "## 5. Where state lives"
	storeSubsector = "**This node alone — the store.**"
	kvSubsector    = "**The whole company — coordination KV.**"
)

// estatePage reads the part of that section that describes the two database
// files, and stops where the coordination estate begins.
//
// NOT THE WHOLE FILE, and not even the whole section. The rest of the page
// names tables in prose for other reasons, so a whole-file read would accept a
// table mentioned anywhere as a table placed somewhere. And the section's own
// last two subsections are about the coordination KV and the streams, whose
// BUCKETS are named `crewlet_leases`, `crewlet_epochs`, `crewlet_ledger` and so
// on — names that look exactly like this engine's tables and are not tables at
// all. Reading past that boundary made the reverse direction report twenty
// buckets as dropped tables.
func estatePage(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(estateDoc)))
	if err != nil {
		t.Fatalf("read %s: %v", estateDoc, err)
	}
	page := string(body)
	start := strings.Index(page, estateHeading)
	if start < 0 {
		t.Fatalf("%s no longer has a %q section, so this guard is reading "+
			"nothing and would pass whatever the page said", estateDoc, estateHeading)
	}
	rest := page[start+len(estateHeading):]
	from := strings.Index(rest, storeSubsector)
	if from < 0 {
		t.Fatalf("%s's %q section no longer opens the store's own tables with "+
			"%q, so this guard cannot find them", estateDoc, estateHeading, storeSubsector)
	}
	rest = rest[from:]
	to := strings.Index(rest, kvSubsector)
	if to < 0 {
		t.Fatalf("%s's %q section no longer reaches %q, so this guard would "+
			"read the coordination buckets as tables", estateDoc, estateHeading, kvSubsector)
	}
	return rest[:to]
}

// backtickedTableNames is every `snake_case` name the section carries that
// could be one of this engine's tables.
//
// Splitting on the backtick puts the SPANS BETWEEN a pair at odd indices, so
// those are the code spans and everything else is prose.
func backtickedTableNames(page string) []string {
	var out []string
	spans := strings.Split(page, "`")
	for i := 1; i < len(spans); i += 2 {
		name := strings.TrimSpace(spans[i])
		if !isTableish(name) || slices.Contains(out, name) {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// isTableish reports whether a backticked token could be one of this engine's
// table names: lower-case, underscore-separated, and carrying one of the
// prefixes the two schemas actually use.
//
// PREFIXES RATHER THAN A SHAPE TEST, because `store_dir`, `node_id` and
// `search_shard` are all lower-case snake_case and none of them is a table.
// Keyed on the prefix, the false-positive set is empty and the false-negative
// set is a table whose name starts with something new — which the FIRST
// direction of this test catches, since that table is in the schema.
func isTableish(s string) bool {
	for _, prefix := range []string{
		"crewlet_", "agent_", "tracker_", "pages_", "kb_", "statelog_",
		"synthesized_", "counterparty_", "conversation_", "scheduled_",
		"chat_", "company_", "secret_", "stream_", "episodes",
	} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
