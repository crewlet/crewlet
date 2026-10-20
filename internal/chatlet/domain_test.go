package chatlet_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE STREAM ARBITRATES EXACTLY THE KINDS THE ENUM SAYS IT DOES.
//
// Two halves of the framework read this and neither can see the other: the
// publisher asks [chatlet.ObjectKind.Arbitrated] whether to form an expectation,
// and the applier is handed [statelog.StreamSpec.ArbitratedKinds] and writes
// an anchor row for what it names. The declaration is a list of STRINGS and
// the enum is typed constants, so nothing in Go holds them together.
//
// A kind arbitrated by the writer and absent from the stream is an expectation
// the applier never advances, which wedges that subject the first time a gate
// drops a record on it. A kind in the stream that nobody publishes on is an
// anchor row nothing ever writes and nothing ever reads.
func TestTheStreamArbitratesExactlyTheKindsTheEnumDoes(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{}
	for _, k := range (chatlet.Domain{}).Stream().ArbitratedKinds {
		declared[k] = true
	}
	for _, kind := range chatlet.ObjectKinds {
		if kind.Arbitrated() != declared[string(kind)] {
			t.Errorf("%s reports Arbitrated=%v and the stream declares %v — the "+
				"publisher reads the first and the applier is handed the "+
				"second, and a disagreement wedges that subject the first time "+
				"a gate drops a record", kind, kind.Arbitrated(),
				declared[string(kind)])
		}
	}
	for kind := range declared {
		if !slices.Contains(chatlet.ObjectKinds, chatlet.ObjectKind(kind)) {
			t.Errorf("the stream arbitrates %q and the domain declares no such "+
				"kind — the anchor for it is a row nothing writes", kind)
		}
	}
	// AND THE ARBITRATED HALF IS EXACTLY THE CHANNEL'S OWN STATE. The
	// count is here rather than left implicit because the failure it
	// guards is the one that reads as a working system: this is the only
	// log in the engine where a kind is deliberately SEQUENCED, and
	// arbitrating the messages would serialise every room behind itself
	// and turn a lost race into a message somebody typed and lost.
	if len(declared) != 2 {
		t.Errorf("the stream arbitrates %d kinds and this domain has two — an "+
			"address and a room's own state. A message is sequenced, and a "+
			"barrier, an eviction and a generation pay nothing for an "+
			"expectation they never form", len(declared))
	}
}

// EVERY RECORD THIS DOMAIN PUBLISHES LANDS INSIDE ITS OWN STREAM.
//
// The subject a record is published to is built by [chatlet.Subject.Wire] through
// the topics grammar; the subject space the stream is CREATED with, and the
// prefix the framework appends a record's own path to, are declared here. A
// disagreement is silent in the worst way: the record publishes to a real
// subject that no consumer covers, so the write succeeds, the applier never
// sees it, and the row simply never appears on any node.
func TestEveryRecordThisDomainPublishesLandsInsideItsOwnStream(t *testing.T) {
	t.Parallel()
	spec := (chatlet.Domain{}).Stream()
	if len(spec.Subjects) != 1 {
		t.Fatalf("the stream is created with %v — one wildcard is what the "+
			"prefix below is the stem of", spec.Subjects)
	}
	wildcard, ok := strings.CutSuffix(spec.Subjects[0], ">")
	if !ok || wildcard != spec.SubjectPrefix+"." {
		t.Fatalf("the stream covers %q and records are published under %q — "+
			"the prefix is not the stem of the subject space, so every record "+
			"this domain writes lands outside its own stream",
			spec.Subjects[0], spec.SubjectPrefix)
	}
	for _, subject := range []chatlet.Subject{
		chatlet.ChannelNameSubject("launch"),
		chatlet.ChannelSubject("room-a"),
		chatlet.MessageSubject("room-a"),
		chatlet.EvictionSubject("node-a"),
		chatlet.GenerationSubject(3),
		chatlet.BarrierSubject(),
	} {
		wire := subject.Wire()
		if !strings.HasPrefix(wire, wildcard) {
			t.Errorf("a %s record publishes to %q, which the stream's own "+
				"subject space %q does not cover", subject.Kind, wire,
				spec.Subjects[0])
		}
	}
}

// THE FRAMEWORK'S VIEW OF A RECORD CARRIES BOTH HALVES OF THE GATE QUESTION.
//
// [chatlet.RecordEnvelope.InstallsGate] is the rule and it has TWO conditions —
// an eviction by its KIND, an erase and a prune by their OP on an ordinary
// channel subject — but the framework never sees a chat envelope. It sees the
// [statelog.Envelope] this domain translated, and asks the domain back. So the
// gate rests on that translation carrying the op as well as the kind, and a
// translation that dropped one would be silent: an unreadable erase would be
// DEFERRED rather than stopping the applier, this node would go on serving
// what every other node destroyed, and there is no inverse that repairs it.
//
// It runs over records as they are actually published — encoded, then read
// back through [chatlet.Domain.Envelope] — because a hand-built envelope would
// assert the translation against itself.
func TestTheFrameworksViewOfARecordCarriesBothHalvesOfTheGateQuestion(t *testing.T) {
	t.Parallel()
	d := chatlet.Domain{}
	const room = "room-a"
	// TWO QUESTIONS, and the erase and the prune are where they part. Both
	// install an apply gate — a deferred deletion is a node still serving
	// what every other node destroyed — and neither is a NODE gate: a write
	// flagged one is excused the passed generation, a peer's truncated rows
	// and the gate reserve, so a prune that answered true would spend the
	// room kept for the eviction that unpins a full log. Only a node's own
	// eviction and readmission are both.
	for name, tc := range map[string]struct {
		subject  chatlet.Subject
		op       chatlet.OpKind
		gate     bool
		nodeGate bool
	}{
		"an eviction, by its kind": {
			chatlet.EvictionSubject("node-a"), chatlet.OpEviction, true, true},
		"an erase, by its op on an ordinary room": {
			chatlet.ChannelSubject(room), chatlet.OpErase, true, false},
		"a retention prune, by its op": {
			chatlet.ChannelSubject(room), chatlet.OpPrune, true, false},
		"an ordinary post": {
			chatlet.MessageSubject(room), chatlet.OpPost, false, false},
		"a tombstoning delete, which keeps the row": {
			chatlet.MessageSubject(room), chatlet.OpDelete, false, false},
		"a settings patch": {
			chatlet.ChannelSubject(room), chatlet.OpPatch, false, false},
		"a reanchor's generation": {
			chatlet.GenerationSubject(2), chatlet.OpGeneration, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			payload, err := chatlet.Encode(chatlet.MutationRecord{
				RecordEnvelope: chatlet.RecordEnvelope{
					V: chatlet.RecordVersion, OpID: "op-" + string(tc.op),
					Subject: tc.subject, Op: tc.op, Gen: 4,
					Writer: "node-a", Scope: chatlet.ScopeSet{Subject: true},
				},
			})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			env, err := d.Envelope(payload)
			if err != nil {
				t.Fatalf("the domain could not read back what it published: %v", err)
			}
			if env.Kind != string(tc.subject.Kind) || env.Op != string(tc.op) {
				t.Fatalf("the framework sees kind %q op %q, and the record is "+
					"kind %q op %q — both halves of the gate question are read "+
					"from this envelope", env.Kind, env.Op, tc.subject.Kind, tc.op)
			}
			if env.Subject.ID != tc.subject.ID {
				t.Errorf("the framework sees subject id %q, want %q — it is "+
					"what the deferral is filed under", env.Subject.ID,
					tc.subject.ID)
			}
			if env.Scope.Empty() {
				t.Error("the envelope declares no scope — an empty scope says " +
					"the record makes nothing stale, which is the one claim a " +
					"record no build may be able to read cannot make")
			}
			if env.Gen != 4 || env.Writer != "node-a" ||
				env.OpID != "op-"+string(tc.op) || env.V != chatlet.RecordVersion {
				t.Errorf("the envelope carries v=%d gen=%d writer=%q op_id=%q — "+
					"the eviction gate reads the writer, the version decides "+
					"whether there is a second pass, and an ambiguous publish "+
					"is resolved by the op id", env.V, env.Gen, env.Writer,
					env.OpID)
			}
			if got := d.InstallsGate(env); got != tc.gate {
				t.Fatalf("InstallsGate = %v, want %v", got, tc.gate)
			}
			if got := d.NodeGate(env); got != tc.nodeGate {
				t.Fatalf("NodeGate = %v, want %v — only a node's own "+
					"eviction may pass the fences and spend the reserve a "+
					"node gate is excused", got, tc.nodeGate)
			}
		})
	}
}

// THE LOG ITSELF SAYS WHO WAS EVICTED, both ways.
//
// A node the fleet re-anchored past never applies an eviction written after
// its applier stopped, so the engine reads a node's gates straight off the
// log ([statelog.EvictionProbe]) — on the subject the domain names, decoded by
// the domain. A probe that read a readmission as an eviction would strand a
// node the operator had taken back, and one that read neither would leave a
// gone node counted on this log for ever.
func TestTheEvictionProbeReadsBothHalvesOfTheGate(t *testing.T) {
	t.Parallel()
	d := chatlet.Domain{}
	var probe statelog.EvictionProbe = d
	subject := probe.EvictionSubject("node-b")
	if want := chatlet.EvictionSubject("node-b"); subject.Kind != string(want.Kind) ||
		subject.ID != want.ID {
		t.Fatalf("the probe looks for node-b's gates on %v and the writer "+
			"publishes them on %v", subject, want)
	}
	for _, readmit := range []bool{false, true} {
		payload, err := chatlet.Encode(chatlet.MutationRecord{
			RecordEnvelope: chatlet.RecordEnvelope{
				OpID: "op-gate", Subject: chatlet.EvictionSubject("node-b"),
				Op: chatlet.OpEviction, Gen: 1, Writer: "node-a",
				Scope: chatlet.ScopeSet{Subject: true},
			},
			Mutation: mustJSON(t, chatlet.Eviction{
				V: chatlet.GateRecordVersion, NodeID: "node-b",
				EvictedBy: "op", Readmitted: readmit,
			}),
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		evicts, err := probe.Evicts(payload)
		if err != nil {
			t.Fatalf("the probe could not read a gate its own writer encodes: %v", err)
		}
		if evicts == readmit {
			t.Errorf("a record with readmitted=%v reads as evicts=%v", readmit, evicts)
		}
	}
	// AND A RECORD THAT IS NOT A GATE IS REFUSED rather than read as
	// either answer: whatever sits on that subject is a gate or a fault.
	other, err := chatlet.Encode(chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			OpID: "op-patch", Subject: chatlet.ChannelSubject("room-a"),
			Op: chatlet.OpPatch, Gen: 1, Writer: "node-a",
			Scope: chatlet.ScopeSet{Subject: true},
		},
		Mutation: mustJSON(t, chatlet.ChannelPatch{V: chatlet.DocumentVersion}),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := probe.Evicts(other); err == nil {
		t.Error("the probe read a room's patch as a gate")
	}
}

// THE WAKE FEED THIS LOG'S TRIM WAITS FOR IS THE ONE THE FEED OPENS.
//
// Two parties name one consumer and neither can ask the other: the change feed
// that advances it and the trim's `feed_ack_floor` term that waits for it. An
// empty answer would trim records nobody has been woken for yet — a message
// whose recipients are never told — and another domain's group would block
// this log's trim on a consumer nobody opens here.
func TestTheTrimWaitsForTheFeedThisLogActuallyRuns(t *testing.T) {
	t.Parallel()
	opened := new(chatlet.Translator).Source().Group
	if got := (chatlet.Domain{}).FeedGroup(); got == "" || got != opened {
		t.Fatalf("the domain declares feed group %q and the feed opens %q",
			got, opened)
	}
}

// THE FRAMEWORK AND THE DOMAIN AGREE ABOUT A BARRIER, INCLUDING ITS NAME.
//
// A barrier is the one record the FRAMEWORK decides to write and the DOMAIN
// has to shape. The framework builds the subject it subscribes and appends on
// out of its OWN constant — [statelog.BarrierKind] under the domain's subject
// prefix — while this domain parses the kind out of that subject with its own
// enum. The two are separate string constants in separate packages, and a
// disagreement is silent in the direction that matters: the append lands, the
// domain reads it as a kind it has never heard of, and the position a
// linearizable read was waiting on belongs to a record the applier files as a
// deferral.
func TestTheFrameworkAndTheDomainAgreeAboutABarrier(t *testing.T) {
	t.Parallel()
	if string(chatlet.KindBarrier) != statelog.BarrierKind {
		t.Fatalf("this domain calls the barrier kind %q and the framework "+
			"publishes on %q — the subject is built from the framework's "+
			"constant and read with this one", chatlet.KindBarrier,
			statelog.BarrierKind)
	}
	payload, err := chatlet.EncodeBarrier(statelogBarrier(""))
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	env, err := (chatlet.Domain{}).Envelope(payload)
	if err != nil {
		t.Fatalf("the domain could not read back the barrier it rendered: %v", err)
	}
	if env.Kind != statelog.BarrierKind || env.Op != string(chatlet.OpBarrier) {
		t.Errorf("the framework sees kind %q op %q for its own barrier",
			env.Kind, env.Op)
	}
	if env.OpID != "" {
		t.Errorf("the barrier carries op id %q — an op id becomes a message "+
			"id, and a duplicate ack is served out of the dedupe window with "+
			"no quorum round trip at all, which is the one claim a barrier "+
			"must never fake", env.OpID)
	}
	if len(env.Scope.Paths) != 1 || env.Scope.Paths[0] != statelog.BarrierScope {
		t.Errorf("a barrier's scope reads back as %v rather than the "+
			"framework's own %q — anything else serialises every linearizable "+
			"read behind every other", env.Scope.Paths, statelog.BarrierScope)
	}
	if (chatlet.Domain{}).InstallsGate(env) {
		t.Error("a barrier installs a gate — it writes no row on any node, so " +
			"stopping the applier for one would halt the domain for a record " +
			"with no effect")
	}
}

// EVERY TABLE THE APPLIER WRITES IS CLASSIFIED, AND ONLY THE LEDGER DIVERGES.
//
// A snapshot's scrub list, the identity claim's membership and the local sweep
// are all derived from one map, which is why a table missing from it is three
// lists that are silently short rather than one error.
func TestEveryTableTheApplierWritesIsClassifiedAndOnlyTheLedgerDiverges(t *testing.T) {
	t.Parallel()
	d := chatlet.Domain{}
	tables := d.Tables()
	for _, name := range chatlet.ReproducibleTables {
		if class, held := tables[name]; !held || class != statelog.Replicated {
			t.Errorf("%s is a reproducible table and the domain says %s — a "+
				"replicated table is what the identity claim compares and what "+
				"a snapshot carries whole", name, classOf(tables, name))
		}
	}
	for _, name := range chatlet.MachineryTables {
		want := statelog.Local
		if name == d.OpsTable() {
			// THE LEDGER TRAVELS, so an adopter can answer a retry of
			// anything its donor applied.
			want = statelog.Divergent
		}
		if class, held := tables[name]; !held || class != want {
			t.Errorf("%s is the log's own machinery and the domain says %s, "+
				"want %s — a deferral is this node's own verdict and is "+
				"scrubbed, while the ledger travels so a retry of anything the "+
				"donor applied is answered rather than decided afresh",
				name, classOf(tables, name), want)
		}
	}
	// THE LEDGER IS THE ONE DIVERGENT TABLE, and that is a claim about the
	// APPLIER rather than a gap in the map: it reads no epoch key and no
	// node identity, so every node writes identical rows and the identity
	// claim covers the whole of the conversation. The ledger diverges only
	// in `applied_at`, each node's own clock. Any other Divergent table
	// means some value a node decided for itself reached a replicated row,
	// and the claim quietly stops covering it.
	for name, class := range tables {
		if class == statelog.Divergent && name != d.OpsTable() {
			t.Errorf("%s is classed divergent — this domain's applier is a "+
				"function of the record and the rows in its own transaction, "+
				"so the only value two nodes may legitimately disagree about "+
				"is the instant each applied an operation", name)
		}
	}
	// THE MAP IS DERIVED FROM THE INVENTORIES AND FROM NOTHING ELSE. An
	// entry added straight to the map is invisible to the completeness
	// walk below and to the replay it stands for.
	if len(tables) != len(chatlet.ReproducibleTables)+len(chatlet.MachineryTables) {
		t.Errorf("the domain classifies %d tables and its two inventories name "+
			"%d — the scrub list, the claim and the sweep are derived once, so "+
			"a table that is in the map and in neither list is one no walk can "+
			"see", len(tables),
			len(chatlet.ReproducibleTables)+len(chatlet.MachineryTables))
	}
	// AND THE MACHINERY THE FRAMEWORK ITSELF WRITES IS NAMED, because
	// those statements are the framework's and the schema is the
	// domain's.
	for _, named := range []struct {
		field, table string
		want         statelog.TableClass
	}{
		{"DeferredTable", d.DeferredTable(), statelog.Local},
		{"ScopeIndex", d.ScopeIndex(), statelog.Local},
		{"OpsTable", d.OpsTable(), statelog.Divergent},
	} {
		if class, held := tables[named.table]; !held || class != named.want {
			t.Errorf("%s names %q and the domain's own map says %s, want %s — "+
				"the framework writes that table and derives what a snapshot "+
				"does with it from this class", named.field, named.table,
				classOf(tables, named.table), named.want)
		}
	}
}

// classOf is how the domain classes a table, IN WORDS, including the case
// where it does not class it at all.
//
// The zero [statelog.TableClass] is Replicated, so a bare map lookup reports a
// table nobody declared as the one class the identity claim covers — which is
// the exact failure these walks exist to report, printed as its opposite.
func classOf(tables map[string]statelog.TableClass, name string) string {
	class, held := tables[name]
	if !held {
		return "it is not declared at all"
	}
	return "it is " + class.String()
}

// theWrittenTables is every table each kind's apply may write.
//
// WRITTEN OUT rather than derived, because a list derived from the thing it
// checks agrees with itself whatever that thing says. It is the union that
// carries the weight: a replicated table no kind writes is a table a replay
// from zero can never fill, which is precisely the failure the completeness
// walk exists for.
var theWrittenTables = map[chatlet.ObjectKind][]string{
	// A create is ONE record: the claim, the room and its founding
	// membership, in one transaction.
	chatlet.KindChannelName: {
		"chat_channel_names", "chat_channels", "chat_members",
	},
	// A room's own state, and the two ops that REMOVE what was said in
	// it: an operator's erase, which leaves the marker, and the retention
	// prune, which takes a room's messages and everything hung off them.
	chatlet.KindChannel: {
		"chat_channels", "chat_members", "chat_messages", "chat_mentions",
		"chat_reactions", "chat_thread_participants", "chat_follows",
		"chat_deletions",
	},
	// A post, an edit, a tombstone and a reaction. It touches the channel
	// row because the per-channel sequence is minted from its counter,
	// and it writes the derived child rows — who was named, who has
	// spoken in the thread, who now follows it — and, for every one of
	// them, what the applier decided about it.
	chatlet.KindMessage: {
		"chat_messages", "chat_channels", "chat_mentions",
		"chat_thread_participants", "chat_follows", "chat_reactions",
		"chat_verdicts",
	},
	chatlet.KindEviction:   {"chat_evictions"},
	chatlet.KindGeneration: {"chat_log_generations"},
	chatlet.KindBarrier:    barrierWrites(),
}

// barrierWrites is [chatlet.BarrierTables] as this walk reads it.
//
// THE BARRIER'S ROW IN THE TABLE ABOVE COMES FROM THE PACKAGE'S OWN
// DECLARATION rather than from an empty literal here, which is the whole
// reason that declaration exists: "this kind writes nothing" and "nobody
// classified this kind" are the same observation from the walk's side, and
// only the package can tell them apart.
func barrierWrites() []string {
	out := make([]string, 0, len(chatlet.BarrierTables))
	for name := range chatlet.BarrierTables {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// EVERY KIND DECLARES THE TABLES IT WRITES, AND EVERY REPLICATED TABLE HAS A
// KIND THAT WRITES IT.
//
// The walk runs BOTH WAYS, and each direction is a different failure. A kind
// with no entry is a record whose effect nobody accounted for — it publishes,
// it is delivered, and what it writes is outside every list derived from the
// table map. A replicated table no kind names is worse and quieter: it is a
// table a replay from zero leaves empty for ever, which looks exactly like a
// company that never used the feature.
func TestEveryKindDeclaresTheTablesItWrites(t *testing.T) {
	t.Parallel()
	tables := (chatlet.Domain{}).Tables()
	written := map[string]bool{}
	for _, kind := range chatlet.ObjectKinds {
		names, classified := theWrittenTables[kind]
		if !classified {
			t.Fatalf("kind %q is declared and this walk does not say what its "+
				"apply writes — a kind nobody accounted for writes rows that "+
				"no scrub list, no identity claim and no sweep knows about", kind)
		}
		for _, name := range names {
			if class, held := tables[name]; !held || class != statelog.Replicated {
				t.Errorf("a %s record writes %s and the domain says %s — a "+
					"table an applier writes and the domain does not declare "+
					"is scrubbed from no snapshot and compared by no claim",
					kind, name, classOf(tables, name))
			}
			written[name] = true
		}
	}
	for kind := range theWrittenTables {
		if !slices.Contains(chatlet.ObjectKinds, kind) {
			t.Errorf("this walk accounts for %q and the domain declares no such "+
				"kind — a table reachable only from a record nothing publishes "+
				"is a table a replay never fills", kind)
		}
	}
	for _, name := range chatlet.ReproducibleTables {
		if !written[name] {
			t.Errorf("%s is a reproducible table and no kind writes it — a "+
				"replay from zero would end with it empty, which is "+
				"indistinguishable from a company that never used it", name)
		}
	}
	// AND THE BARRIER IS THE DECLARED EMPTY SET. It is the one kind that
	// writes no row on any node, and it may not reach that answer by
	// being forgotten: a barrier that wrote a row would put an upsert per
	// linearizable read inside the transaction holding this store's only
	// writer.
	if len(chatlet.BarrierTables) != 0 {
		t.Errorf("the barrier declares %v — it is the read index's "+
			"payload-free append and writes nothing anywhere",
			chatlet.BarrierTables)
	}
}
