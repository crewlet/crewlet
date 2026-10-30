package engine_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/tracker"
)

// seamClass is what a field a SERVING data node reads is, with respect to the
// company's configuration: the rule a node behind that configuration answers
// it under follows from the class, and only from the class.
type seamClass string

const (
	// seamGrant is an AUTHORIZATION judged from the serving node's
	// configuration — who may do something. Being asked last cannot make
	// one sound: told to answer anyway, a node behind would judge it by the
	// chart the fleet replaced, which may be the revision that revoked it.
	// So a grant field is judged only where a node behind the
	// configuration REFUSES it rather than answers it last. SIX exist, the
	// chart a SEAT is judged by on the serving node: code.ReaderOptions.Roster,
	// a read of the rows; query.Options.Roster, a read by object id on a git
	// node; host.Options.Roster, an engine write a git node builds;
	// code.CommitterOptions.SeatRules, a seat's push on the committer that
	// decides it; code.ChangeWriterOptions.SeatRules, a seat's write of a
	// change's conversation, and every reviewer an ask names, on the change
	// writer beside it; and code.StoreOptions.SeatRules, a tool's lifecycle
	// request — a person's as well as a seat's, since a create's owner is
	// judged against the chart too — on the lifecycle store that decides it,
	// refused there by its own gate, code.StoreOptions.Current, and by the
	// estate's declaration on code.lifecycle. The git node's own gates,
	// query.Options.Current and host.Options.Current, refuse a seat's read
	// and a seat's write there themselves (unready, behind_config) rather
	// than judging them; the committer's and the change writer's,
	// code.CommitterOptions.Current and code.ChangeWriterOptions.Current,
	// refuse such a write there as a node that ran nothing, so the
	// repository's next committing node decides it, even when this node is
	// asked last; a data node refuses a seat's read of the rows by the
	// operation's own declaration (the estate's op.judgesGrant on code.read
	// and code.resolve_ref, held by
	// estate.TestEveryOperationThatJudgesAGrantIsDeclared), even asked last
	// (M3-O8's hard arm). The tracker's own permission checks read an
	// authority the ASKING node forms from its own.
	seamGrant seamClass = "grant"

	// seamValue is a value or a routing read from the serving node's
	// epoch — a person a people field names, a lead a wake falls back to,
	// a day cut on the company's clock, a unit a read renders, a search's
	// scope and the container it leaves out, the model a query is embedded
	// in, a size limit. A stale one is a state that arises anyway: a
	// handle removed just after the write, a wake to a previous lead, a
	// repository over a limit a revision lowered. So the whole answer is
	// that a node behind the configuration is asked LAST
	// ([estate.Backend.Current]) and answers when no current data node
	// does.
	seamValue seamClass = "value"

	// seamGate is the predicate itself: whether this node serves the
	// company's configuration.
	seamGate seamClass = "gate"

	// seamNone reads no configuration: the log, the store, the clock, the
	// node's identity, its metrics — or a half whose own reads are
	// classified on the options it was built from.
	seamNone seamClass = "none"
)

func (c seamClass) valid() bool {
	return slices.Contains([]seamClass{seamGrant, seamValue, seamGate, seamNone}, c)
}

// servingSeams is every struct a serving data node's decide is handed what it
// reads through: the tracker writer's dependencies, the estate's backend and
// the serving node's own seams — and the options struct each half a
// [estate.Backend] field holds is BUILT from, where one is, since that is where
// what the half reads is decided: the code host's committer, change writer,
// lifecycle store and reader options, the knowledge searcher's, the page
// reader's and the page store's, the identity directory's reader's, the
// artifacts estate's and its datastore's readers' and writers', and the item
// search's ranker and the fan-out it and the knowledge searcher answer through.
// (The tracker's reader, the link index and the usage reader take what they
// read as arguments, which their Backend rows name.) A Backend row that only
// named its half would see nothing added to the options behind it: the
// knowledge searcher read the epoch's skills container and embedding model
// through options no row covered. And a GIT node's query server's options and
// its code host's: it serves a seat's read of a repository by object id, and
// builds a seat's engine write, from its own epoch's chart, which is the same
// question a serving decide's grant is.
var servingSeams = []reflect.Type{
	reflect.TypeFor[tracker.WriterDeps](),
	reflect.TypeFor[estate.Backend](),
	reflect.TypeFor[estate.ServerSeams](),
	reflect.TypeFor[code.CommitterOptions](),
	reflect.TypeFor[code.ChangeWriterOptions](),
	reflect.TypeFor[code.ChecksWriterOptions](),
	reflect.TypeFor[code.StoreOptions](),
	reflect.TypeFor[code.ReaderOptions](),
	reflect.TypeFor[query.Options](),
	reflect.TypeFor[host.Options](),
	reflect.TypeFor[pages.SearcherOptions](),
	reflect.TypeFor[pages.ReaderOptions](),
	reflect.TypeFor[pages.Options](),
	reflect.TypeFor[iamdomain.ReaderOptions](),
	reflect.TypeFor[artifactsdomain.ReaderOptions](),
	reflect.TypeFor[artifactsdomain.WriterDeps](),
	reflect.TypeFor[artifactdata.ReaderOptions](),
	reflect.TypeFor[artifactdata.WriterDeps](),
	reflect.TypeFor[engine.ItemRanker](),
	reflect.TypeFor[search.FanOut](),
}

// classified is every field of [servingSeams], by `<package>.<Type>.<Field>`,
// with its class and why.
var classified = map[string]struct {
	class seamClass
	why   string
}{
	"tracker.WriterDeps.Publisher": {seamNone, "the tracker log's write authority"},
	"tracker.WriterDeps.DB":        {seamNone, "the replicated rows a write decides from"},
	"tracker.WriterDeps.Claims":    {seamNone, "the fleet's leases, for the key counters"},
	"tracker.WriterDeps.NodeID":    {seamNone, "the deciding node"},
	"tracker.WriterDeps.World": {seamValue, "a people field resolved to a handle, and a " +
		"project's default assignee checked, against the epoch's roster"},
	"tracker.WriterDeps.Metrics":   {seamNone, "the node's instruments"},
	"tracker.WriterDeps.Drain":     {seamNone, "the applier's measured rate"},
	"tracker.WriterDeps.Actor":     {seamNone, "who writes, which the asker's credential states"},
	"tracker.WriterDeps.ActorKind": {seamNone, "what kind of party writes"},
	"tracker.WriterDeps.Leads": {seamValue, "the leads a create's, a purge's and a " +
		"dependency's wakes fall back to"},
	"tracker.WriterDeps.Now":  {seamNone, "the clock"},
	"tracker.WriterDeps.Zone": {seamValue, "the company's clock a date given as an instant is cut to a day by"},

	"estate.Backend.Tracker": {seamNone, "the copy's reader, over its rows and its log's read " +
		"authority alone; the units it renders are ServerSeams.Units"},
	"estate.Backend.Writer": {seamNone, "a tracker writer per actor, built over tracker.WriterDeps"},
	"estate.Backend.WorkSearch": {seamNone, "the item search over the copy's rows, ranked by " +
		"engine.itemRanker through a search.FanOut, whose reads are classified there"},
	"estate.Backend.Pages":      {seamNone, "the copy's page reader, built over pages.ReaderOptions"},
	"estate.Backend.PageWriter": {seamNone, "the pages store, built over pages.Options"},
	"estate.Backend.Knowledge": {seamNone, "the knowledge searcher, built over " +
		"pages.SearcherOptions, whose reads are classified there; its scope is ServerSeams.Seat"},
	"estate.Backend.Backlinks": {seamNone, "the node's link index"},
	"estate.Backend.Usage":     {seamNone, "the usage rows"},
	"estate.Backend.Code": {seamNone, "the committer, the change writer, the checks writer, the reader " +
		"and the lifecycle store, built over code.CommitterOptions, code.ChangeWriterOptions, " +
		"code.ChecksWriterOptions, code.ReaderOptions and code.StoreOptions"},
	"estate.Backend.Git": {seamNone, "the git half a git operation is answered from; what it reads " +
		"is classified on query.Options, which it is built over"},
	"estate.Backend.Identity": {seamNone, "the identity directory's reader, built over " +
		"iamdomain.ReaderOptions: who holds a login and each human seat, and the row a machine " +
		"token is checked against, read from the copy's rows; the chart and the Tier A token " +
		"list an answer is judged against, and whether a token is good, are the ASKING node's"},
	"estate.Backend.Artifacts": {seamNone, "the artifacts reader and writer, built over " +
		"artifactsdomain.ReaderOptions and artifactsdomain.WriterDeps; a caller's role on an " +
		"artifact is decided and stamped on the asking side"},
	"estate.Backend.ArtifactData": {seamNone, "the datastore's reader and writer, built over " +
		"artifactdata.ReaderOptions and artifactdata.WriterDeps; a write's authority is stamped " +
		"on the asking side"},
	"estate.Backend.Reached": {seamNone, "the applier's position"},
	"estate.Backend.Answers": {seamNone, "the copy's distance from its log"},
	"estate.Backend.Admits":  {seamNone, "the copy's establishment, for a seat's admission"},
	"estate.Backend.Current": {seamGate, "whether this node serves the company's configuration"},
	"estate.Backend.verifiers": {seamNone, "the keys a machine token's narrowed answer is MAC'd " +
		"under, derived from the node's Tier A keyring, never from the company's configuration"},
	"estate.Backend.pageBytes": {seamNone, "how much of a paged read one answer carries"},

	"estate.ServerSeams.Units": {seamValue, "the unit a tracker read renders"},
	"estate.ServerSeams.Leads": {seamValue, "the leads a dependency's wakes fall back to"},
	"estate.ServerSeams.Seat": {seamValue, "a searching seat's role and the org-wide scope " +
		"its knowledge search covers"},

	"code.CommitterOptions.Publisher": {seamNone, "the code log's write authority"},
	"code.CommitterOptions.DB":        {seamNone, "the replicated rows a batch's answers are read from"},
	"code.CommitterOptions.NodeID":    {seamNone, "the deciding node"},
	"code.CommitterOptions.Limits": {seamValue, "the size limits a push is decided under and a seat's " +
		"day's quota, a count: a push decided under a bound a revision then lowered leaves the same rows"},
	"code.CommitterOptions.Retention": {seamValue, "the instant a repo_deleted refusal names"},
	"code.CommitterOptions.SeatRules": {seamGrant, "the chart a seat's push is judged by — its grant, " +
		"its share of a repository outside its unit — and the calendar its day is counted on"},
	"code.CommitterOptions.Current": {seamGate, "whether this node serves the company's configuration: " +
		"a seat's push behind it is handed to the repository's next committer, decided nowhere here"},
	"code.CommitterOptions.Now":     {seamNone, "the decide's clock"},
	"code.CommitterOptions.Events":  {seamNone, "the node's event stream an applied merge is announced on"},
	"code.CommitterOptions.Metrics": {seamNone, "the node's instruments"},
	"code.CommitterOptions.Logger":  {seamNone, "where a failed batch is reported"},

	"code.ChangeWriterOptions.Publisher": {seamNone, "the code log's write authority"},
	"code.ChangeWriterOptions.DB":        {seamNone, "the replicated rows a write's answer is read from"},
	"code.ChangeWriterOptions.NodeID":    {seamNone, "the deciding node"},
	"code.ChangeWriterOptions.Retention": {seamValue, "the instant a repo_deleted refusal names"},
	"code.ChangeWriterOptions.SeatRules": {seamGrant, "the chart a seat's write of a change's " +
		"conversation is judged by — whether it reads, reviews or maintains the repository — and every " +
		"reviewer an ask names, whoever asks: whether the seat may review it"},
	"code.ChangeWriterOptions.Current": {seamGate, "whether this node serves the company's configuration: " +
		"a write the chart judges behind it is handed to the repository's next committing node, decided " +
		"nowhere here"},
	"code.ChangeWriterOptions.Now":     {seamNone, "the decide's clock"},
	"code.ChangeWriterOptions.Metrics": {seamNone, "the node's instruments"},
	"code.ChangeWriterOptions.Logger":  {seamNone, "where a failed write is reported"},

	"code.ChecksWriterOptions.Publisher": {seamNone, "the code log's write authority"},
	"code.ChecksWriterOptions.DB":        {seamNone, "the replicated rows a write's answer is read from"},
	"code.ChecksWriterOptions.NodeID":    {seamNone, "the deciding node"},
	"code.ChecksWriterOptions.Retention": {seamValue, "the instant a repo_deleted refusal names"},
	"code.ChecksWriterOptions.Events":    {seamNone, "the node's event stream an applied ending is announced on"},
	"code.ChecksWriterOptions.Now":       {seamNone, "the decide's clock"},
	"code.ChecksWriterOptions.Metrics":   {seamNone, "the node's instruments"},
	"code.ChecksWriterOptions.Logger":    {seamNone, "where a failed write is reported"},

	"code.StoreOptions.Publisher": {seamNone, "the code log's write authority"},
	"code.StoreOptions.DB":        {seamNone, "the replicated rows a lifecycle write decides from"},
	"code.StoreOptions.NodeID":    {seamNone, "the deciding node"},
	"code.StoreOptions.Now":       {seamNone, "the clock"},
	"code.StoreOptions.SeatRules": {seamGrant, "the chart a tool's lifecycle request is judged by: who " +
		"may create, rename, change, archive, delete, restore or protect a repository, a create's owner, " +
		"and what a seat may hold"},
	"code.StoreOptions.Current": {seamGate, "whether this node serves the company's configuration: a " +
		"lifecycle request behind it is answered not serving, so the next data node decides it"},
	"code.StoreOptions.Policy": {seamValue, "the epoch's code.scratch and the repository bound a create " +
		"is held to: a create decided under a cap a revision then lowered leaves a repository the next " +
		"create counts"},

	"code.ReaderOptions.Log":    {seamNone, "the code log's read authority"},
	"code.ReaderOptions.Limits": {seamValue, "the limits a repository's detail reports"},
	"code.ReaderOptions.Roster": {seamGrant, "the chart a seat's read of a repository is " +
		"judged by: which repositories it lists, and whether it reads one"},
	"code.ReaderOptions.Now": {seamNone, "the clock a change's routing dates the notices still kept " +
		"by"},

	"query.Options.NodeID": {seamNone, "this node's name, which a refusal about the node names"},
	"query.Options.Cache": {seamNone, "the node's git cache, synced from the code log's rows like " +
		"any reader's"},
	"query.Options.Git":   {seamNone, "the node's git pool"},
	"query.Options.Share": {seamNone, "the node's seat git-work share of its read lane"},
	"query.Options.Resyncs": {seamNone, "the node's buckets of its seats' tools' re-syncs at the " +
		"code log's head, a fixed rate no revision moves: spent by a read, never configuration it reads"},
	"query.Options.Roster": {seamGrant, "the chart a seat's read of a repository by object id is " +
		"judged by on the git node that answers it"},
	"query.Options.Current": {seamGate, "whether this node serves the company's configuration: a " +
		"seat's read behind it is answered unready, behind_config, and judged nowhere here"},
	"query.Options.Fetched": {seamNone, "where a read the node answered of a visibility: unit " +
		"repository is recorded: the node's reads ledger, flushed into its own event log, which " +
		"no decide reads"},

	"host.Options.Cache": {seamNone, "the node's git cache, synced from the code log's rows like any " +
		"reader's"},
	"host.Options.Git":     {seamNone, "the node's git pool"},
	"host.Options.Estate":  {seamNone, "the code domain's rows and its committer, through the router"},
	"host.Options.Objects": {seamNone, "the object store a push's bundle is stored in"},
	"host.Options.Audit": {seamNone, "where a git push is recorded: the operator audit, which no " +
		"decide reads"},
	"host.Options.Fetched": {seamNone, "where a fetch of a visibility: unit repository is recorded: " +
		"the node's reads ledger"},
	"host.Options.Limits": {seamValue, "the size limits a push and an engine write are landed under: " +
		"a write landed under a bound a revision then lowered leaves the same rows"},
	"host.Options.DeletedRetention": {seamValue, "the instant a repo_deleted refusal names"},
	"host.Options.Roster": {seamGrant, "the chart a seat's engine write is judged by on the git " +
		"node that builds it, and a coding box's seat by the git node it dials"},
	"host.Options.Signer": {seamValue, "the epoch's names an engine-built commit carries: a seat's " +
		"role name and address and the identity domain — a revision renaming a seat just after " +
		"the write leaves the same commit — and the engine's committer a coding box's pack may " +
		"hold no new commit of, which a box judged against the domain a revision just replaced " +
		"shares with a box that pushed just before it"},
	"host.Options.Share": {seamNone, "the node's seat git-work share of its read lane"},
	"host.Options.Resyncs": {seamNone, "the node's buckets of its seats' tools' re-syncs at the " +
		"code log's head, the query server's own, a fixed rate no revision moves: spent by an engine " +
		"write's re-judgement, never configuration it reads"},
	"host.Options.Current": {seamGate, "whether this node serves the company's configuration: a " +
		"seat's write behind it is answered unready, behind_config, and built nowhere here, and a " +
		"coding box answered 503 behind_config"},
	"host.Options.Boxes": {seamNone, "a coding box's run tokens, the keyring's, and its run's rows, " +
		"coordination state; neither is an epoch's value — the box's grant reaches the epoch only " +
		"through Roster"},
	"host.Options.NodeID":  {seamNone, "this node's name, which a proposal and a refusal name"},
	"host.Options.Timing":  {seamNone, "the host's scaled waits"},
	"host.Options.Faults":  {seamNone, "the suite's fault script, nil in production"},
	"host.Options.Logger":  {seamNone, "where the host logs"},
	"host.Options.Metrics": {seamNone, "the node's instruments"},
	"host.Options.Now":     {seamNone, "the clock"},

	"pages.SearcherOptions.Index":  {seamNone, "the node's lexical index"},
	"pages.SearcherOptions.Node":   {seamNone, "this node's name in the fan-out's assignment table"},
	"pages.SearcherOptions.Peers":  {seamNone, "the fleet half of the fan-out"},
	"pages.SearcherOptions.Roster": {seamNone, "the live data nodes"},
	"pages.SearcherOptions.Report": {seamNone, "the node's instruments"},
	"pages.SearcherOptions.Enter":  {seamNone, "the count of scans in flight"},
	"pages.SearcherOptions.Vectors": {seamValue, "a query's embedding, by the embeddings " +
		"provider and model of the serving node's epoch"},
	"pages.SearcherOptions.SkillsContainer": {seamValue, "the tool-skills container every " +
		"result leaves out, read live from the epoch's knowledge.skills_container"},
	"pages.SearcherOptions.Copies": {seamNone, "this node's verdict on the copies it scans"},

	"pages.ReaderOptions.DB":  {seamNone, "the replicated rows a page read answers from"},
	"pages.ReaderOptions.Log": {seamNone, "the pages log's read authority"},

	"pages.Options.Publisher": {seamNone, "the pages log's write authority"},
	"pages.Options.DB":        {seamNone, "the replicated rows a page write decides from"},
	"pages.Options.Now":       {seamNone, "the clock an authored instant is stamped from"},
	"pages.Options.Reserved": {seamValue, "the containers an agent's write is held back " +
		"from and the spaces a page may be created in, read from the epoch's knowledge " +
		"settings and chart: a write a stale reading admits is one that, sent a moment " +
		"before the revision, would have landed anyway"},

	"iamdomain.ReaderOptions.DB":      {seamNone, "the replicated rows an identity read answers from"},
	"iamdomain.ReaderOptions.Log":     {seamNone, "the identity log's read authority"},
	"iamdomain.ReaderOptions.Reached": {seamNone, "the applier's position"},
	"iamdomain.ReaderOptions.Lag":     {seamNone, "the applier's distance from its log"},
	"iamdomain.ReaderOptions.Await":   {seamNone, "the applier's wait for a position"},

	"artifactsdomain.ReaderOptions.DB":  {seamNone, "the replicated rows an artifact read answers from"},
	"artifactsdomain.ReaderOptions.Log": {seamNone, "the artifacts log's read authority"},

	"artifactsdomain.WriterDeps.Publisher":  {seamNone, "the artifacts log's write authority"},
	"artifactsdomain.WriterDeps.DB":         {seamNone, "the replicated rows a write decides from"},
	"artifactsdomain.WriterDeps.Now":        {seamNone, "the clock"},
	"artifactsdomain.WriterDeps.Dependents": {seamNone, "the datastore a purge clears first"},
	"artifactsdomain.WriterDeps.SeatKind": {seamValue, "the kind a transfer records as its new " +
		"owner's, by the epoch's chart: a revision changing that seat's kind just after the " +
		"transfer leaves the same record"},

	"artifactdata.ReaderOptions.DB":  {seamNone, "the replicated rows a datastore read answers from"},
	"artifactdata.ReaderOptions.Log": {seamNone, "the datastore log's read authority"},

	"artifactdata.WriterDeps.Publisher": {seamNone, "the datastore log's write authority"},
	"artifactdata.WriterDeps.DB":        {seamNone, "the replicated rows a write's verdict is read from"},
	"artifactdata.WriterDeps.SeatExists": {seamValue, "whether a person column's value names a " +
		"seat of the epoch's chart: a seat removed just after the write leaves the same value"},
	"artifactdata.WriterDeps.Now":        {seamNone, "the clock"},
	"artifactdata.WriterDeps.Metrics":    {seamNone, "the node's instruments"},
	"artifactdata.WriterDeps.Objectives": {seamNone, "the node's objective meter"},

	"engine.itemRanker.fan":   {seamNone, "the item search's fan-out, classified on search.FanOut"},
	"engine.itemRanker.index": {seamNone, "the node's lexical index"},

	"search.FanOut.Self":   {seamNone, "this node's name in the assignment table"},
	"search.FanOut.Local":  {seamNone, "this node's scan of its own index and copies"},
	"search.FanOut.Peers":  {seamNone, "the fleet half of the fan-out"},
	"search.FanOut.Roster": {seamNone, "the live data nodes"},
	"search.FanOut.Corpus": {seamNone, "how many documents this node's index holds"},
	"search.FanOut.Vectors": {seamValue, "a query's embedding, by the embeddings provider " +
		"and model of the serving node's epoch"},
	"search.FanOut.Budget": {seamNone, "the wait for the peers' assignments"},
	"search.FanOut.Report": {seamNone, "the node's instruments"},
	"search.FanOut.Enter":  {seamNone, "the count of scans in flight"},
	"search.FanOut.passed": {seamNone, "the coordinator's memory of nodes that could not " +
		"cover a range"},
	"search.FanOut.now": {seamNone, "the clock that memory is timed by"},
}

// EVERY SEAM A SERVING DECIDE READS IS CLASSIFIED.
//
// A data node the control plane has confirmed behind the company's
// configuration goes on serving the estate: every operation is asked of it
// last ([estate.Backend.Current]), because what it supplies from its own epoch
// is the configuration the fleet replaced — a VALUE whose stale reading arises
// anyway, and which a current data node answers first. That is sound only for
// as long as every seam a serving decide reads is such a value: one that judged
// a GRANT from the chart would be decided, by the last resort, against the
// revision that revoked it. Which a seam is was an audit of every field these
// structs carry; this test is that audit kept current. A field added to any of
// them without a row fails here, and so does a row naming a field that is gone,
// so the classification cannot drift from the code.
//
// A seam reached through another one of these structs — an embedded one — is
// classified by that struct's own rows rather than twice.
func TestEveryEpochSeamAServingDecideReadsIsClassified(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, typ := range servingSeams {
		for i := range typ.NumField() {
			field := typ.Field(i)
			if field.Anonymous && slices.Contains(servingSeams, field.Type) {
				continue
			}
			name := typ.String() + "." + field.Name
			seen[name] = true
			row, ok := classified[name]
			switch {
			case !ok:
				t.Errorf("%s is not classified: say whether a serving decide reads a grant, "+
					"a value or no configuration through it — a node behind the company's "+
					"configuration answers a value last and must refuse a grant", name)
			case !row.class.valid():
				t.Errorf("%s is classified %q, which is no class", name, row.class)
			case row.why == "":
				t.Errorf("%s is classified %s with no reason", name, row.class)
			}
		}
	}
	for name := range classified {
		if !seen[name] {
			t.Errorf("%s is classified and no serving seam has it — drop the row", name)
		}
	}
}
