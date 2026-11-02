package search_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
)

// copiesOf is a node's verdict on its copies: every source's rows in service
// unless named in out, and the vectors unless vectorsOut.
type copiesOf struct {
	out        map[search.Source]bool
	vectorsOut bool
}

func (c copiesOf) Serves(_ context.Context, src search.Source) bool { return !c.out[src] }
func (c copiesOf) VectorsServe(context.Context) bool                { return !c.vectorsOut }

// A PARTICIPANT WHOSE COPY IS OUT OF SERVICE COVERS NOTHING — ADR-0072.
//
// A copy out of service is wrong rather than behind, and the index over it is
// over rows that stopped being the fleet's. Scanned anyway, the participant
// answered its range from them and the coordinator merged that answer under
// complete coverage: the quietly short answer the fan-out exists to make
// visible. PER SOURCE, as building is: a query that reads only a corpus whose
// copy serves is answered normally.
func TestAParticipantWhoseCopyIsOutOfServiceCoversNothing(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	page(t, db, "p.one", "ENG", "Rollback", "roll back a deploy by release", 1)
	x := search.NewIndexerOver(db, db.Replicated().Reader(),
		[]search.LexicalSource{search.PageSource{}, search.TaskSource{}})
	indexAll(t, x)
	scanner := search.NodeScanner{Index: x,
		Copies: copiesOf{out: map[search.Source]bool{search.SourcePage: true}}}
	lexical := []search.Method{search.MethodLexical}

	for name, sources := range map[string][]string{
		"every source":   nil,
		"the pages only": {string(search.SourcePage)},
	} {
		got, err := scanner.Scan(t.Context(), search.FanQuery{Text: "rollback",
			Sources: sources, Methods: lexical}, search.Everything())
		if err != nil {
			t.Fatalf("%s: scan: %v", name, err)
		}
		if !got.OutOfService || len(got.Lexical) != 0 {
			t.Fatalf("%s: a scan over a pages copy out of service answered %+v — "+
				"want nothing covered, said so", name, got)
		}
	}
	got, err := scanner.Scan(t.Context(), search.FanQuery{Text: "rollback",
		Sources: []string{string(search.SourceTask)}, Methods: lexical}, search.Everything())
	if err != nil {
		t.Fatalf("a task-only scan: %v", err)
	}
	if got.OutOfService {
		t.Fatal("a scan of the tasks alone was refused over the pages' copy")
	}

	// AND A SCANNER THAT JUDGES NO COPY scans every one it holds.
	got, err = search.NodeScanner{Index: x}.Scan(t.Context(), search.FanQuery{Text: "rollback",
		Methods: lexical}, search.Everything())
	if err != nil || got.OutOfService || len(got.Lexical) != 1 {
		t.Fatalf("a scanner with no verdict = (%+v, %v), want the page found", got, err)
	}
}

// AN OUT-OF-SERVICE SLICE IS COUNTED MISSING, its hits dropped and its node
// named with the reason it gave — a range cannot be both scanned and
// unscanned, and merged under "complete" it is the answer that is quietly
// short.
func TestAnOutOfServiceSliceIsCountedMissing(t *testing.T) {
	t.Parallel()
	fan := &search.FanOut{
		Self:  "n1",
		Local: fixed{search.Slice{Node: "n1", Lexical: []search.Scored{{Key: "page:A", Score: 1}}}},
		Peers: peersFunc(func(context.Context, search.FanQuery, []search.Assigned) ([]search.Slice, error) {
			return []search.Slice{{Node: "n2", OutOfService: true,
				Lexical: []search.Scored{{Key: "page:FROZEN", Score: 9}}}}, nil
		}),
		Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2"}, nil },
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
	}
	answer, err := fan.Search(t.Context(), search.FanQuery{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case !answer.Partial():
		t.Fatal("a peer whose copy is out of service was counted as covering its range")
	case !slices.Equal(answer.Absent, []string{"n2"}):
		t.Fatalf("absent = %v, want the out-of-service peer named", answer.Absent)
	case !slices.Equal(answer.Hits, []string{"page:A"}):
		t.Fatalf("hits = %v — a frozen copy's hits were merged", answer.Hits)
	case answer.BucketsAnswered+answer.BucketsMissing != search.SearchShards:
		t.Fatalf("answered %d + missing %d does not partition %d buckets",
			answer.BucketsAnswered, answer.BucketsMissing, search.SearchShards)
	}
	for _, node := range answer.Nodes {
		if node.ID == "n2" && !strings.Contains(node.Error, "out of service") {
			t.Fatalf("n2's coverage says %q, want the reason its reply gave", node.Error)
		}
	}
}

// A SEMANTIC HALF OVER AN OUT-OF-SERVICE VECTORS COPY IS SKIPPED, and says so:
// the half is best effort, so a wrong vectors copy degrades the answer the way
// a missing provider does, while the lexical half still covers the range from
// copies that serve.
func TestASemanticHalfOverAnOutOfServiceCopyIsSkipped(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	page(t, db, "p.one", "ENG", "Rollback", "roll back a deploy by release", 1)
	x := search.NewIndexerOver(db, db.Replicated().Reader(), []search.LexicalSource{search.PageSource{}})
	indexAll(t, x)
	scanner := search.NodeScanner{Index: x, Copies: copiesOf{vectorsOut: true}}
	got, err := scanner.Scan(t.Context(), search.FanQuery{Text: "rollback",
		Vector: make([]byte, 16), Model: "m", Dim: 4,
		Methods: []search.Method{search.MethodLexical, search.MethodSemantic}}, search.Everything())
	switch {
	case err != nil:
		t.Fatalf("a scan with its vectors copy out of service failed: %v", err)
	case !got.SemanticSkipped || !got.VectorsOutOfService:
		t.Fatal("a semantic half over a vectors copy out of service was not reported " +
			"skipped for that reason")
	case got.OutOfService || len(got.Lexical) != 1:
		t.Fatalf("the lexical half did not cover the range: %+v", got)
	}
}

// A PEER THAT COVERED NOTHING IS GIVEN NO RANGE FOR A WHILE, so the next search
// is complete — and it is asked again once the window lapses.
//
// Every node holds the whole corpus, so a peer whose copy of what a search
// reads is out of service, or whose index is still on its first lap, is a
// share the rest can scan. Handed that share on every search regardless,
// every search in the company was answered over part of the corpus for as
// long as the state held, and paid a round trip to a node that scanned
// nothing. PER SHAPE: the sources a query names, since both states are per
// source; a peer whose VECTORS copy is out of service is passed over only for
// the queries that rank by meaning.
func TestAPeerThatCoveredNothingIsGivenNoRangeForAWhile(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reply    search.Slice
		methods  []search.Method
		semantic bool
	}{
		"out of service":    {reply: search.Slice{OutOfService: true}},
		"still building":    {reply: search.Slice{Building: true}},
		"vectors gone, too": {reply: search.Slice{SemanticSkipped: true, VectorsOutOfService: true}, semantic: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var tables [][]search.Assigned
			fan := &search.FanOut{
				Self:  "n1",
				Local: fixed{search.Slice{Node: "n1"}},
				Peers: peersFunc(func(_ context.Context, _ search.FanQuery,
					table []search.Assigned) ([]search.Slice, error) {
					mu.Lock()
					tables = append(tables, table)
					mu.Unlock()
					var out []search.Slice
					for _, a := range table {
						if a.Node == "n1" {
							continue
						}
						reply := search.Slice{Node: a.Node}
						if a.Node == "n2" {
							reply = tc.reply
							reply.Node = a.Node
						}
						out = append(out, reply)
					}
					return out, nil
				}),
				Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2", "n3"}, nil },
				Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
			}
			now := time.Unix(1_700_000_000, 0)
			search.SetClock(fan, func() time.Time { return now })
			query := search.FanQuery{Text: "rollback", Sources: []string{"task"},
				Methods: []search.Method{search.MethodLexical}}
			if tc.semantic {
				query.Vector, query.Model, query.Dim = make([]byte, 16), "m", 4
				query.Mode = knowledge.ModeHybrid
			}
			askedN2 := func() bool {
				mu.Lock()
				defer mu.Unlock()
				for _, a := range tables[len(tables)-1] {
					if a.Node == "n2" {
						return true
					}
				}
				return false
			}
			first, err := fan.Search(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			if !askedN2() {
				t.Fatal("the premise: n2 was not asked the first time")
			}
			if !tc.semantic && first.BucketsMissing == 0 {
				t.Fatal("the premise: the first search lost n2's range")
			}

			next, err := fan.Search(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			if askedN2() {
				t.Fatal("n2 was given a range again inside the window")
			}
			if next.BucketsMissing != 0 || next.SemanticSkipped {
				t.Fatalf("the next search was answered %+v — the rest did not take n2's range",
					next.Coverage())
			}
			for _, node := range next.Nodes {
				if !node.Answered {
					t.Fatalf("the next search names %s absent: %+v", node.ID, next.Nodes)
				}
			}

			// ANOTHER SHAPE IS NOT PASSED OVER: n2 is asked for the pages
			// — or, for its vectors, for a keyword search.
			other := query
			if tc.semantic {
				other.Vector, other.Model, other.Dim, other.Mode = nil, "", 0, knowledge.ModeKeyword
			} else {
				other.Sources = []string{"page"}
			}
			if _, err := fan.Search(t.Context(), other); err != nil {
				t.Fatal(err)
			}
			if !askedN2() {
				t.Fatal("n2 was passed over for a query of another shape")
			}

			// AND THE WINDOW LAPSES: n2 is asked again.
			now = now.Add(search.PassOverFor + time.Second)
			if _, err := fan.Search(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			if !askedN2() {
				t.Fatal("n2 was still passed over after the window lapsed")
			}
		})
	}
}

// countingScan is a coordinator's own scan that answers one slice and counts
// how often it was asked, and over which range.
type countingScan struct {
	mu     sync.Mutex
	slice  search.Slice
	calls  int
	shards []search.Assignment
}

func (c *countingScan) Scan(_ context.Context, _ search.FanQuery, shards search.Assignment) (search.Slice, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.shards = append(c.shards, shards)
	return c.slice, nil
}

func (c *countingScan) asked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// A COORDINATOR THAT CANNOT COVER A SHAPE GIVES ITS RANGE TO ITS PEERS — the
// pass-over applies to this node as it does to every other.
//
// A coordinator whose own index is still on its first lap (every freshly
// joined data node, for minutes), or whose vectors copy is out of service,
// covers nothing of its own range — and the router sends it searches all the
// same, since nothing routes on the index or the vectors. Kept as a
// participant regardless, its range was missing from EVERY search however
// many peers could have scanned it, and a passed-over peer's share that landed
// on it was missing too. Passed over like a peer, it scans nothing for that
// shape and its peers split every bucket.
func TestACoordinatorThatCannotCoverAShapeGivesItsRangeToItsPeers(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		local    search.Slice
		semantic bool
	}{
		"still building": {local: search.Slice{Building: true}},
		"out of service": {local: search.Slice{OutOfService: true}},
		"vectors out of service": {local: search.Slice{SemanticSkipped: true,
			VectorsOutOfService: true}, semantic: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local := &countingScan{slice: tc.local}
			var mu sync.Mutex
			var tables [][]search.Assigned
			fan := &search.FanOut{
				Self:  "n1",
				Local: local,
				Peers: peersFunc(func(_ context.Context, _ search.FanQuery,
					table []search.Assigned) ([]search.Slice, error) {
					mu.Lock()
					tables = append(tables, table)
					mu.Unlock()
					out := make([]search.Slice, 0, len(table))
					for _, a := range table {
						out = append(out, search.Slice{Node: a.Node, Shards: a.Shards})
					}
					return out, nil
				}),
				Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2", "n3"}, nil },
				Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
			}
			now := time.Unix(1_700_000_000, 0)
			search.SetClock(fan, func() time.Time { return now })
			query := search.FanQuery{Text: "rollback", Sources: []string{"task"},
				Methods: []search.Method{search.MethodLexical}}
			if tc.semantic {
				query.Vector, query.Model, query.Dim = make([]byte, 16), "m", 4
				query.Mode = knowledge.ModeHybrid
			}
			scattered := func() (named []string, width int) {
				mu.Lock()
				defer mu.Unlock()
				for _, a := range tables[len(tables)-1] {
					named = append(named, a.Node)
					width += a.Shards.Width()
				}
				return named, width
			}

			first, err := fan.Search(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			if local.asked() != 1 {
				t.Fatal("the premise: the coordinator did not scan its own range the first time")
			}
			if tc.semantic && !first.SemanticSkipped || !tc.semantic && first.BucketsMissing == 0 {
				t.Fatalf("the premise: the first search lost nothing of the coordinator's range: %+v",
					first.Coverage())
			}

			next, err := fan.Search(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			if got := local.asked(); got != 1 {
				t.Fatalf("the coordinator scanned its own range again inside the window "+
					"(%d scans) — over a shape its last scan said it cannot cover", got)
			}
			named, width := scattered()
			if slices.Contains(named, "n1") || width != search.SearchShards {
				t.Fatalf("the next search scattered %v over %d buckets, want n2 and n3 "+
					"over all %d", named, width, search.SearchShards)
			}
			if next.BucketsMissing != 0 || next.SemanticSkipped {
				t.Fatalf("the next search was answered %+v — the peers did not take the "+
					"coordinator's range", next.Coverage())
			}
			for _, node := range next.Nodes {
				if node.ID == "n1" || !node.Answered {
					t.Fatalf("the next search's coverage names %+v", next.Nodes)
				}
			}

			// ANOTHER SHAPE IS NOT PASSED OVER: the coordinator scans its
			// range for the pages — or, for its vectors, a keyword search.
			other := query
			if tc.semantic {
				other.Vector, other.Model, other.Dim, other.Mode = nil, "", 0, knowledge.ModeKeyword
			} else {
				other.Sources = []string{"page"}
			}
			if _, err := fan.Search(t.Context(), other); err != nil {
				t.Fatal(err)
			}
			if local.asked() != 2 {
				t.Fatal("the coordinator was passed over for a query of another shape")
			}

			// AND THE WINDOW LAPSES: it scans its own range again.
			now = now.Add(search.PassOverFor + time.Second)
			if _, err := fan.Search(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			if local.asked() != 3 {
				t.Fatal("the coordinator was still passed over after the window lapsed")
			}
			local.mu.Lock()
			defer local.mu.Unlock()
			for _, shards := range local.shards {
				if !shards.Covers() {
					t.Fatalf("the coordinator scanned %+v, want a bounded range of a "+
						"divided table", shards)
				}
			}
		})
	}
}

// NOBODY KNOWN TO COVER IS ASKED AGAIN. When every node a search could divide
// its buckets among has said it cannot cover that shape, passing all of them
// over leaves nobody — and solo would ask only the coordinator, the one node
// known not to. So every node is asked again, and each reply refreshes what
// is known: a node that recovered is found by the next search rather than
// after the window.
func TestNobodyKnownToCoverIsAskedAgain(t *testing.T) {
	t.Parallel()
	local := &countingScan{slice: search.Slice{Building: true}}
	var mu sync.Mutex
	var tables [][]search.Assigned
	n2Building := true
	fan := &search.FanOut{
		Self:  "n1",
		Local: local,
		Peers: peersFunc(func(_ context.Context, _ search.FanQuery,
			table []search.Assigned) ([]search.Slice, error) {
			mu.Lock()
			defer mu.Unlock()
			tables = append(tables, table)
			out := make([]search.Slice, 0, len(table))
			for _, a := range table {
				out = append(out, search.Slice{Node: a.Node, Shards: a.Shards, Building: n2Building})
			}
			return out, nil
		}),
		Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2"}, nil },
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
	}
	now := time.Unix(1_700_000_000, 0)
	search.SetClock(fan, func() time.Time { return now })
	query := search.FanQuery{Text: "rollback", Methods: []search.Method{search.MethodLexical}}

	first, err := fan.Search(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if first.BucketsAnswered != 0 {
		t.Fatalf("the premise: a search over two building nodes answered %+v", first.Coverage())
	}

	mu.Lock()
	n2Building = false
	mu.Unlock()
	next, err := fan.Search(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if local.asked() != 2 {
		t.Fatalf("the coordinator scanned %d time(s), want its range again — nobody "+
			"was known to cover, so every node is asked", local.asked())
	}
	mu.Lock()
	asked := len(tables)
	last := tables[len(tables)-1]
	mu.Unlock()
	if asked != 2 || len(last) != 1 || last[0].Node != "n2" {
		t.Fatalf("the second search scattered %d time(s), last %+v — want n2 asked again", asked, last)
	}
	answered := false
	for _, node := range next.Nodes {
		if node.ID == "n2" && node.Answered {
			answered = true
		}
	}
	if !answered || next.BucketsAnswered != last[0].Shards.Width() {
		t.Fatalf("n2 recovered and answered its range, and the search counted %+v", next.Coverage())
	}
}

// A COORDINATOR GIVEN NO RANGE STANDS IN FOR WHAT ITS PEERS LEFT UNCOVERED.
//
// Passed over for its own shape, a coordinator scans nothing of it — and it
// still holds the whole corpus. So when the broker cannot be asked, or every
// peer stays silent, or one does, the ranges nobody covered are scanned here
// before the answer is fused. Passed over for its VECTORS, that is its keyword
// half: the search is answered over every bucket, said as a semantic skip, and
// the silent peers are named as not having answered. Given no range and
// nothing in its place, a broker that failed inside the half minute emptied
// every search the coordinator could have answered by its words alone. Passed
// over for a copy out of service or an index still building, its own scan
// still covers nothing — as it would have — and the answer says that too.
func TestACoordinatorGivenNoRangeStandsInForWhatItsPeersLeftUncovered(t *testing.T) {
	t.Parallel()
	type peerFault int
	const (
		brokerFails peerFault = iota
		allSilent
		oneSilent
	)
	for name, tc := range map[string]struct {
		local  search.Slice
		fault  peerFault
		covers bool
		gap    search.Assignment
	}{
		"vectors out of service, the broker fails": {
			local: search.Slice{SemanticSkipped: true, VectorsOutOfService: true,
				Lexical: []search.Scored{{Key: "task:t1", Score: 2}}},
			fault: brokerFails, covers: true, gap: search.Assignment{From: 0, To: 64},
		},
		"vectors out of service, every peer silent": {
			local: search.Slice{SemanticSkipped: true, VectorsOutOfService: true,
				Lexical: []search.Scored{{Key: "task:t1", Score: 2}}},
			fault: allSilent, covers: true, gap: search.Assignment{From: 0, To: 64},
		},
		"vectors out of service, one peer silent": {
			local: search.Slice{SemanticSkipped: true, VectorsOutOfService: true,
				Lexical: []search.Scored{{Key: "task:t1", Score: 2}}},
			fault: oneSilent, covers: true, gap: search.Assignment{From: 0, To: 32},
		},
		"still building, the broker fails": {
			local: search.Slice{Building: true},
			fault: brokerFails, gap: search.Assignment{From: 0, To: 64},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local := &countingScan{slice: tc.local}
			var mu sync.Mutex
			fault := false
			fan := &search.FanOut{
				Self:  "n1",
				Local: local,
				Peers: peersFunc(func(_ context.Context, _ search.FanQuery,
					table []search.Assigned) ([]search.Slice, error) {
					mu.Lock()
					defer mu.Unlock()
					if fault && tc.fault == brokerFails {
						return nil, errors.New("nats: no responders")
					}
					var out []search.Slice
					for _, a := range table {
						switch {
						case fault && tc.fault == allSilent:
						case fault && tc.fault == oneSilent && a.Node == "n2":
						default:
							out = append(out, search.Slice{Node: a.Node, Shards: a.Shards,
								Semantic: []search.Scored{{Key: "page:" + a.Node, Score: -1}}})
						}
					}
					return out, nil
				}),
				Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2", "n3"}, nil },
				Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
			}
			now := time.Unix(1_700_000_000, 0)
			search.SetClock(fan, func() time.Time { return now })
			query := search.FanQuery{Text: "rollback", Sources: []string{"task"},
				Vector: make([]byte, 16), Model: "m", Dim: 4, Mode: knowledge.ModeHybrid}
			if _, err := fan.Search(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			if local.asked() != 1 {
				t.Fatal("the premise: the coordinator did not scan its own range the first time")
			}

			mu.Lock()
			fault = true
			mu.Unlock()
			answer, err := fan.Search(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			local.mu.Lock()
			scans := slices.Clone(local.shards)
			local.mu.Unlock()
			if len(scans) != 2 || scans[1] != tc.gap {
				t.Fatalf("the coordinator scanned %+v, want its first range and then the "+
					"uncovered %+v in its peers' place", scans, tc.gap)
			}
			if !tc.covers {
				if answer.BucketsAnswered != 0 || answer.BucketsMissing != search.SearchShards ||
					!slices.Equal(answer.Absent, []string{"n2", "n3"}) {
					t.Fatalf("a coordinator that covers nothing claimed %+v", answer.Coverage())
				}
				for _, node := range answer.Nodes {
					if !strings.Contains(node.Error, "n1 could not scan it in its place") {
						t.Fatalf("%s's absence does not say the coordinator could not stand "+
							"in: %q", node.ID, node.Error)
					}
				}
				return
			}
			if answer.BucketsMissing != 0 || answer.BucketsAnswered != search.SearchShards ||
				len(answer.Absent) != 0 {
				t.Fatalf("the search lost what the coordinator could scan: %+v", answer.Coverage())
			}
			if !answer.SemanticSkipped || answer.Degraded != knowledge.DegradedSemanticPartial {
				t.Fatalf("the keyword half the coordinator stood in with was not said: "+
					"skipped %v, degraded %q", answer.SemanticSkipped, answer.Degraded)
			}
			if !slices.Contains(answer.Hits, "task:t1") {
				t.Fatalf("the coordinator's stand-in hits were not merged: %v", answer.Hits)
			}
			silent := map[string]bool{"n2": true, "n3": tc.fault != oneSilent}
			for _, node := range answer.Nodes {
				switch {
				case node.ID == "n1":
					if !node.Answered {
						t.Fatalf("the coordinator scanned in its peers' place and is named "+
							"as not answering: %+v", answer.Nodes)
					}
				case silent[node.ID]:
					if node.Answered || !strings.Contains(node.Error, "n1 scanned its range in its place") {
						t.Fatalf("%s did not answer and is named %+v", node.ID, node)
					}
				case !node.Answered:
					t.Fatalf("%s answered and is named %+v", node.ID, node)
				}
			}
		})
	}
}
