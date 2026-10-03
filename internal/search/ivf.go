package search

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"math/bits"
	"math/rand/v2"
	"runtime"
	"slices"
	"sort"
	"sync"
)

// THE SEMANTIC FIRST STAGE AS AN INDEX — ADR-0028 — and this file is its
// arithmetic: an inverted file (IVF) over the 1-bit sign codes the full scan
// already reads, trained by k-means in HAMMING space with a majority-bit
// centroid update.
//
// # Why an index at all, when the package doc spent years saying there was none
//
// The full scan was the right answer while the corpus one node held fitted the
// interactive budget, and it no longer does. Its cost is N × c_row over the
// narrow table — 2.90 µs a source at p95 with one reader and 7.34 µs with
// eight (BenchmarkSemanticIVFUnderLoad's scan arm) — so one node holds
// ≈ 345 000 sources idle and ≈ 136 000 under load inside
// [SemanticScanBudget], and a company at a thousand seats measures ≈ 2.4 s
// under load in its first year. An inverted file makes the first stage read
// the lists nearest the query instead of every row: the cost becomes
// (probed / lists) × N plus a rerank that does not grow, and the exact f32
// rerank above it is UNCHANGED, so the index decides which documents are
// looked at and never how they are ordered.
//
// # Why IVF and not a graph, and why over the SIGN CODES
//
// No new dependency and no new file format: the lists are a column on the
// narrow table the scan already reads, the centroids are a record on the
// vector log every node already applies, and the whole index travels inside
// the snapshot that already carries the table. A graph index (HNSW, DiskANN)
// is a second structure with its own persistence, its own concurrent-update
// story and a build that is not a pure function of the records — every node
// would build a different one. And the training is over the codes rather than
// the f32 vectors because the codes are what every node already holds in the
// narrow table, 8× smaller, and because HAMMING arithmetic is integer: two
// CPUs agree on it to the bit, where a float k-means disagrees in the last
// place across architectures and two nodes would file one document in two
// lists. That is ADR-0027's objection to a float logarithm in placement, and
// it binds here for the same reason.
//
// # What it costs, and what it loses — measured, never assumed
//
// A document is found only if its list is probed, so recall now depends on
// the probe count as well as on the oversample. How much depends on the
// corpus, and the fixtures disagree with each other at every size:
//
//	sources   isotropic (no topics)        topical
//	 20 000   every list — declined        half
//	120 000   half (0.966)                 an eighth (0.985)
//	500 000   every list — declined        half (0.993)
//
// So the probe count is never a constant: every training MEASURES recall
// against the exact scan on held-out documents — sampled from the corpus,
// kept out of the k-means sample, and never counted as their own answer — and
// records the smallest probe count that meets the floor curve with no head
// miss in EVERY SHAPE a search is issued in ([QueryShape], [ChooseProbes]).
// An index that would have to read more than [IVFProbeCeiling] of its lists
// to do so is not installed at all, because it would read nearly the whole
// table through an index for the scan's own answer. The embedding duty
// re-measures an installed index every [IVFMeasureInterval], and `crewlet
// search eval` whenever an operator asks.
//
// # A narrowed search reads more lists, or scans
//
// A search narrowed to one source or to some containers keeps only the rows
// its filter matches, so the lists an unfiltered search would read hold too
// few of its answers: its true top 150 are the unfiltered top 150/s, for a
// filter keeping a share s of the corpus, and they are spread over lists no
// unfiltered probe reaches. Measured on the topical fixture at 20 000
// sources, an index trained on unfiltered queries alone installed at half its
// lists, and a search reading those same lists recalled 0.949 with three head
// misses when narrowed to the tenth of the corpus that is pages, and 0.68 to
// 0.74 when narrowed to one container (a sixty-fourth of it, clustered as a
// project is) — where the full scan it replaced recalled 1.000 for both. So a
// narrowed search reads lists, nearest first, until it
// has seen as many MATCHING rows as the unfiltered probe reads rows at all
// ([ProbeCount]) — the same neighbourhood, measured in the rows the answer can
// come from — and when that would take more than the ceiling it runs the full
// scan instead, which reads every matching row. The training measures every
// shape through that same rule, so the recall it records is the recall
// narrowed searches get.
//
// The no-head-miss half of that criterion is the binding one at scale. At
// 500 000 topical sources aggregate recall meets the 0.88 floor at 8 of
// 2 048 lists, and one head miss on twenty-five held-out queries persists to
// 512 — so the index installs at half, where it saves about a third of the
// scan rather than most of it. It is kept because it is the evaluation's own
// definition of passing ([EvalReport.Passed]): an index chosen by a weaker
// rule would be a first stage `crewlet search eval` reports as failing.
// What an installed index buys is therefore bounded below by the ceiling —
// at half the lists, 73 ms p95 against the scan's 116 at 40 000 sources with
// one reader and 219 against 294 with eight — and a corpus with real topics
// buys more.

const (
	// IVFMinListRows is the list size below which a centroid is noise.
	//
	// SIXTY-FOUR: a centroid is a per-bit MAJORITY of its members, and with
	// fewer members than that a coordinate's majority is decided by a
	// handful of documents rather than by the topic they share. It caps the
	// list count below FAISS's rule for small corpora ([IVFLists]).
	IVFMinListRows = 64

	// IVFMinLists is the fewest lists an index is trained with: SIXTEEN,
	// because an index of fewer reads a sixteenth of the corpus at best and
	// is not worth a column on every row.
	IVFMinLists = 16

	// IVFMaxLists is the most: 2 048, the most whose TRAINING costs the
	// node holding the duty minutes of its cores rather than a quarter of an
	// hour at the partition sizes an index serves.
	//
	// The k-means costs sample × lists × rounds Hamming distances and its
	// sample is [IVFTrainPointsPerList] a list, so it grows with the SQUARE
	// of the list count. Measured at 500 000 topical sources on four shared
	// cores: 33 s of k-means and 19 s filing every code at 2 048 lists,
	// against 126 s and 39 s at 4 096 — which FAISS's rule ([IVFLists])
	// would have chosen from about 524 000 sources. At 2 048 the same
	// training is about two minutes on every core, a little over three on
	// the half of them a training runs on ([ivfCoreShare]), and on a node
	// allowed one core, shared with two searchers, already 237–282 s of
	// k-means and 139–172 s of filing (BenchmarkIVFTrainingShare -cpu 1); at
	// 4 096 that k-means alone would be four times as long. NOTHING ELSE BOUNDS IT: the
	// tick's budget measures progress, and arithmetic that cannot wedge runs
	// exempt from it ([Budget]), so the training's own size is what decides
	// how long the duty's node gives its cores to it — and this cap is that
	// size.
	// The rule reaches 2 048 at about 131 000 sources, and at ≈ 545 000 — the
	// most one node searches inside [SemanticScanBudget] through an index at
	// its probe ceiling — the mean list still holds ≈ 266 rows, four times
	// the [IVFMinListRows] a centroid needs, so the cap costs resolution only
	// on partitions past what a node serves anyway.
	IVFMaxLists = 2048

	// IVFMinCorpus is the smallest partition that gets an index: sixteen
	// lists of sixty-four rows. Below it the full scan reads fewer than a
	// thousand narrow rows, which costs about a millisecond — there is
	// nothing for an index to save.
	IVFMinCorpus = IVFMinLists * IVFMinListRows

	// IVFTrainPointsPerList is how many sampled rows each centroid is
	// trained from.
	//
	// FORTY, which is FAISS's own floor — it warns below 39 points per
	// centroid — rounded. Measured at 120 000 fixture sources and 1 024
	// lists, recall at a quarter of the lists was 0.9435 trained from 40 a
	// list, 0.9448 from 64 and 0.9469 from every row: inside one standard
	// error of each other, for a third of the training time. The sample is
	// the one term of training cost that grows with the corpus, so it is
	// the smallest one that measured no loss.
	IVFTrainPointsPerList = 40

	// IVFTrainIterations caps the k-means rounds.
	//
	// TEN, measured on the topical fixture at 120 000 sources and 1 024
	// lists: recall at an eighth of the lists was 0.9675 after three rounds,
	// 0.9760 after ten and 0.9752 after twenty. Ten captures the gain and
	// twenty doubles the training for none; on the isotropic fixture every
	// count from three to twenty measured within noise.
	IVFTrainIterations = 10

	// IVFProbeCeiling is the share of its lists an index may probe and
	// still be worth having, as a divisor: an index whose recall needs more
	// than 1/IVFProbeCeiling of its lists is NOT INSTALLED.
	//
	// TWO, measured at the ceiling itself: at 40 000 topical sources an index
	// probing half its lists answered at 73 ms p95 against the full scan's
	// 116 with one reader, and 219 against 294 with eight
	// (BenchmarkSemanticIVFUnderLoad). A row read through the covering index
	// costs about 1.3× a row of the scan's sequential read, so at half the
	// lists the index has already spent two thirds of what the scan costs
	// idle and three quarters under load — and a stale rollout (see
	// [Stage1]) adds a materialised union on top. Past half it would be
	// bookkeeping on every embed and every node for a first stage that is no
	// faster; the scan is chosen instead.
	IVFProbeCeiling = 2

	// IVFRetrainFactor is how far the corpus may move before the index is
	// retrained: when it has doubled or halved since training.
	//
	// The list count follows √N, so a corpus that doubled wants √2 as many
	// lists, and the probe count was measured at the old size — a recall
	// measured at N says nothing certain at 2N. Halving is the same rule the
	// other way, and below [IVFMinCorpus] it retires the index.
	IVFRetrainFactor = 2

	// IVFImbalance is how lopsided the lists may get before a retrain: the
	// largest more than four times the mean. A list that large is a probe
	// that reads four lists' worth of rows whenever it is chosen, which is
	// the cost model's whole premise failing.
	IVFImbalance = 4

	// IVFReassignBatch is how many rows one reassign record re-files.
	//
	// [ScanBatch]'s size, for its reason: a thousand rows is a handful of
	// statements, and a reassign apply costs at most IVFReassignBatch ×
	// lists Hamming distances — about 135 ms at 2 048 lists — so no apply
	// transaction holds this store's writer for a whole partition.
	IVFReassignBatch = ScanBatch
)

// IVFLists is the list count for a corpus of n sources.
//
// FAISS's rule of thumb, stated as such — the power of two nearest 4·√n —
// capped so the mean list holds at least [IVFMinListRows], and clamped to
// [IVFMinLists, IVFMaxLists]. The row cap binds below 65 536 sources, where
// 4·√n lists would average fewer than sixty-four rows; FAISS's rule binds from
// there to about 131 000, and [IVFMaxLists] above. At [IVFMinCorpus] it is
// sixteen lists of sixty-four, which is what that constant is.
func IVFLists(n int) int {
	if n <= 0 {
		return IVFMinLists
	}
	faiss := 1 << int(math.Round(math.Log2(4*math.Sqrt(float64(n)))))
	lists := faiss
	if cap := n / IVFMinListRows; cap < lists {
		// THE LARGEST POWER OF TWO NOT ABOVE THE CAP, so the mean list
		// never falls below the size a centroid needs.
		lists = 1
		for lists*2 <= cap {
			lists *= 2
		}
	}
	return min(max(lists, IVFMinLists), IVFMaxLists)
}

// CodeWords is how many 64-bit words an n-dimensional sign code occupies —
// [Quantize]'s own output length.
func CodeWords(dimensions int) int { return (dimensions + 63) / 64 }

// Codes is a matrix of sign codes, one row per document, held FLAT.
//
// One allocation rather than a slice per row: at the corpus sizes an index is
// trained over, a slice header per row is a third of the memory again and a
// pointer the collector walks per document.
type Codes struct {
	// Words is every row's width, [CodeWords] of the embedding width.
	Words int
	// Data is the rows end to end.
	Data []uint64
}

// NewCodes is an empty matrix with room for capacity rows.
func NewCodes(words, capacity int) Codes {
	return Codes{Words: words, Data: make([]uint64, 0, words*capacity)}
}

// Append adds one row. A row of another width is a caller error, for
// [Hamming]'s reason: two widths are two embedding spaces.
func (c *Codes) Append(code []uint64) {
	if len(code) != c.Words {
		panic(fmt.Sprintf("search: a %d-word code appended to a %d-word matrix",
			len(code), c.Words))
	}
	c.Data = append(c.Data, code...)
}

// Len is how many rows the matrix holds.
func (c Codes) Len() int {
	if c.Words == 0 {
		return 0
	}
	return len(c.Data) / c.Words
}

// At is row i.
func (c Codes) At(i int) []uint64 { return c.Data[i*c.Words : (i+1)*c.Words] }

// IVF is one trained index: its centroids, each a sign code.
//
// A VALUE, like everything else in this file's arithmetic, so the probe the
// query runs and the assignment every applier runs are functions a unit test
// calls with no database.
type IVF struct {
	words     int
	centroids []uint64
}

// NewIVF wraps centroids of the given width.
func NewIVF(words int, centroids []uint64) (IVF, error) {
	if words <= 0 || len(centroids) == 0 || len(centroids)%words != 0 {
		return IVF{}, fmt.Errorf("search: %d centroid words are not a whole "+
			"number of %d-word codes", len(centroids), words)
	}
	return IVF{words: words, centroids: centroids}, nil
}

// Lists is how many lists the index has.
func (x IVF) Lists() int {
	if x.words == 0 {
		return 0
	}
	return len(x.centroids) / x.words
}

// Words is the width of every centroid.
func (x IVF) Words() int { return x.words }

// Centroid is list j's centroid.
func (x IVF) Centroid(j int) []uint64 { return x.centroids[j*x.words : (j+1)*x.words] }

// Nearest is the list a code is filed in: the centroid at the smallest Hamming
// distance, THE LOWEST LIST WINNING A TIE.
//
// The tie break is not cosmetic, for the reason it is not in [CandidatePool]:
// Hamming distance over a 3 072-bit code takes at most 3 073 values, so a code
// equidistant from two centroids is an ordinary event, and a node that broke
// the tie the other way would file the document in a list nobody else probes
// for it.
//
// INTEGER ARITHMETIC ONLY. A float distance here — the cosine of the f32
// vector against a ±1 centroid, say — ranks nearly the same and is not
// bit-reproducible across architectures, so two holders of one partition
// would disagree about where a document lives: see ADR-0028.
func (x IVF) Nearest(code []uint64) int {
	best, list := math.MaxInt, 0
	for j := range x.Lists() {
		if d := Hamming(code, x.Centroid(j)); d < best {
			best, list = d, j
		}
	}
	return list
}

// ProbeOrder is every list, nearest the query first: by Hamming distance to
// its centroid, then by list number.
//
// The same metric the assignment files documents by, which is what makes the
// nearest lists the ones a query's neighbours were filed in.
func (x IVF) ProbeOrder(code []uint64) []int {
	distance := make([]int, x.Lists())
	order := make([]int, x.Lists())
	for j := range order {
		order[j] = j
		distance[j] = Hamming(code, x.Centroid(j))
	}
	sort.Slice(order, func(a, b int) bool {
		if distance[order[a]] != distance[order[b]] {
			return distance[order[a]] < distance[order[b]]
		}
		return order[a] < order[b]
	})
	return order
}

// Assign files every row of codes in its [IVF.Nearest] list.
//
// PARALLEL AND STILL DETERMINISTIC: each row's list is a function of that row
// and the centroids alone, and every worker writes a disjoint range of the
// answer, so the result does not depend on how many workers ran or in what
// order they finished.
//
// It stops within a stride ([ivfStride]) of ctx ending and answers ctx's
// error, never a partial filing: at the largest partition an index serves the
// filing is the longest step a training takes without returning — tens of
// seconds on its share of the cores — and a duty tick cut off, by a lease it
// could not renew or an engine stopping, must not go on spending those cores
// on a result it will discard.
func (x IVF) Assign(ctx context.Context, codes Codes) ([]int32, error) {
	return x.assign(ctx, codes, ivfWorkers())
}

// assign is [IVF.Assign] on the given number of workers.
func (x IVF) assign(ctx context.Context, codes Codes, workers int) ([]int32, error) {
	out := make([]int32, codes.Len())
	if err := parallelRanges(ctx, workers, codes.Len(), func(from, to int) {
		for i := from; i < to; i++ {
			out[i] = int32(x.Nearest(codes.At(i)))
		}
	}); err != nil {
		return nil, fmt.Errorf("search: filing %d codes in %d lists stopped: %w",
			codes.Len(), x.Lists(), err)
	}
	return out, nil
}

// Bytes is the index as it is stored and carried: every centroid's words,
// little-endian, list by list.
func (x IVF) Bytes() []byte {
	out := make([]byte, 8*len(x.centroids))
	for i, w := range x.centroids {
		binary.LittleEndian.PutUint64(out[8*i:], w)
	}
	return out
}

// DecodeIVF reads [IVF.Bytes] back, refusing a blob that is not lists whole
// codes of the width dimensions implies.
func DecodeIVF(dimensions, lists int, b []byte) (IVF, error) {
	words := CodeWords(dimensions)
	if dimensions <= 0 || lists <= 0 || len(b) != 8*words*lists {
		return IVF{}, fmt.Errorf("search: an index of %d lists at %d dimensions "+
			"is %d bytes, and this one is %d", lists, dimensions,
			8*words*max(lists, 0), len(b))
	}
	centroids := make([]uint64, words*lists)
	for i := range centroids {
		centroids[i] = binary.LittleEndian.Uint64(b[8*i:])
	}
	return NewIVF(words, centroids)
}

// codeFromBits turns the driver's `vector1bit` encoding into the words
// [Quantize] produces.
//
// The packed bits come first, LSB-first — bit i at position i%8 of byte i/8,
// which [Quantize]'s own doc measured against the driver — and a trailer
// follows ([CodeBytes]). Little-endian words over the packed bytes are
// therefore bit i at position i%64 of word i/64: Quantize's layout exactly,
// which TestTheStoredCodeIsTheQuantizedCode pins against the driver itself.
func codeFromBits(b []byte, dimensions int) ([]uint64, error) {
	if len(b) != CodeBytes(dimensions) {
		return nil, fmt.Errorf("search: a %d-dimensional sign code is %d bytes "+
			"and this one is %d", dimensions, CodeBytes(dimensions), len(b))
	}
	packed := b[:(dimensions+7)/8]
	code := make([]uint64, CodeWords(dimensions))
	for i, octet := range packed {
		code[i/8] |= uint64(octet) << (8 * uint(i%8))
	}
	return code, nil
}

// IVFSeed is the training's random seed, DERIVED rather than drawn.
//
// From the log the index is for and the index it replaces (zero for the
// first), so two runs of one training over one corpus — a retry after an
// unresolved publish, a lease that moved mid-training, two holders checking
// each other — draw the same sample and reach the same centroids. A seed from
// the clock would make "every holder builds the same index" true only of the
// holders that were handed the record, and false of every test that re-trains
// to check it.
func IVFSeed(log string, basis int64) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(log))
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(basis))
	_, _ = h.Write(b[:])
	return h.Sum64()
}

// ivfStream is the PCG stream the training draws on, fixed so the seed alone
// decides the sample.
const ivfStream = 0x1CEB00DA

// TrainIVF runs k-means over codes in Hamming space and returns lists
// centroids.
//
// # Deterministic, on every node, to the bit
//
// The sample is a seeded partial shuffle (PCG, integer), the assignment is
// [IVF.Nearest] (integer, lowest list on a tie), the update is a per-bit
// MAJORITY of each list's members with a tie keeping the centroid's previous
// bit, and a list left empty is re-seeded with the sampled row farthest from
// its own centroid (lowest row on a tie). Nothing in it is a float and nothing
// depends on how many workers ran, so the same codes and the same seed give
// the same centroids everywhere — TestEveryHolderBuildsTheSameIndex.
//
// # Why the majority and not a mean
//
// A mean of sign codes is a vector of fractions, and ranking a query code
// against fractions is float arithmetic again. The majority is the Hamming
// median — the code minimising the summed Hamming distance to the list's
// members — which is exactly the quantity k-means in this metric minimises.
//
// # Held-out rows are never trained on
//
// heldOut are the rows the training will measure recall FROM, ascending, and
// no centroid is drawn from them: a query document that shaped the centroid of
// the list it is filed in is a query the index was fitted to answer, and the
// recall it measured would describe that fit rather than a search.
//
// It stops within a stride ([ivfStride]) of ctx ending — a training runs
// inside a duty tick whose lease is bounded, and one that would outlive it
// must publish nothing ([Embedder.Tick]), so the cores it holds are released
// as soon as that is known rather than at the end of the round. The caller
// chooses lists ([IVFLists]); codes must hold at least that many rows outside
// heldOut. It runs on [ivfWorkers] of the cores.
func TrainIVF(ctx context.Context, codes Codes, lists int, seed uint64, heldOut []int) (IVF, error) {
	return trainIVF(ctx, codes, lists, seed, heldOut, ivfWorkers())
}

// trainIVF is [TrainIVF] on the given number of workers, which changes how
// long it takes and never what it answers.
func trainIVF(ctx context.Context, codes Codes, lists int, seed uint64, heldOut []int,
	workers int) (IVF, error) {

	n := codes.Len()
	if lists <= 0 || n-len(heldOut) < lists {
		return IVF{}, fmt.Errorf("search: train %d lists over %d codes, %d of "+
			"them held out", lists, n, len(heldOut))
	}
	sample := sampleRows(n, IVFTrainPointsPerList*lists, seed, heldOut)
	points := NewCodes(codes.Words, len(sample))
	for _, i := range sample {
		points.Append(codes.At(i))
	}

	// THE FIRST lists SAMPLED ROWS are the starting centroids: the sample is
	// a uniform shuffle, so they are a uniform draw of distinct rows.
	centroids := make([]uint64, lists*codes.Words)
	copy(centroids, points.Data[:lists*codes.Words])
	x := IVF{words: codes.Words, centroids: centroids}

	assignment := make([]int32, points.Len())
	distance := make([]int32, points.Len())
	for i := range assignment {
		assignment[i] = -1
	}
	for round := range IVFTrainIterations {
		changed, err := x.assignInto(ctx, points, assignment, distance, workers)
		if err == nil && round > 0 && changed == 0 {
			// CONVERGED: the update below would rebuild these same
			// centroids, since every tie keeps the bit it has.
			break
		}
		if err == nil {
			err = x.update(ctx, points, assignment, distance, workers)
		}
		if err != nil {
			return IVF{}, fmt.Errorf("search: the index's training stopped "+
				"in round %d of %d: %w", round+1, IVFTrainIterations, err)
		}
	}
	return x, nil
}

// assignInto files every point in its nearest list, recording its distance,
// and reports how many moved — or ctx's error, having stopped within a stride
// of it ending.
func (x IVF) assignInto(ctx context.Context, points Codes, assignment, distance []int32,
	workers int) (int, error) {

	var mu sync.Mutex
	changed := 0
	err := parallelRanges(ctx, workers, points.Len(), func(from, to int) {
		moved := 0
		for i := from; i < to; i++ {
			code := points.At(i)
			best, list := math.MaxInt, 0
			for j := range x.Lists() {
				if d := Hamming(code, x.Centroid(j)); d < best {
					best, list = d, j
				}
			}
			if assignment[i] != int32(list) {
				moved++
			}
			assignment[i], distance[i] = int32(list), int32(best)
		}
		mu.Lock()
		changed += moved
		mu.Unlock()
	})
	return changed, err
}

// update moves every centroid to its list's per-bit majority, and re-seeds the
// lists that emptied — or answers ctx's error, having stopped within a stride
// of it ending and left the centroids for the caller to discard.
func (x IVF) update(ctx context.Context, points Codes, assignment, distance []int32,
	workers int) error {

	lists := x.Lists()
	// THE MEMBERS OF EACH LIST, by a counting sort, so the per-list pass
	// below reads its own rows and needs one bit counter rather than one per
	// list — lists × 3 072 counters is 25 MB at 2 048 lists.
	start := make([]int, lists+1)
	for _, a := range assignment {
		start[a+1]++
	}
	for j := range lists {
		start[j+1] += start[j]
	}
	members := make([]int, len(assignment))
	next := append([]int(nil), start[:lists]...)
	for i, a := range assignment {
		members[next[a]] = i
		next[a]++
	}

	bitsPerCode := 64 * x.words
	if err := parallelRanges(ctx, workers, lists, func(from, to int) {
		counter := make([]int32, bitsPerCode)
		for j := from; j < to; j++ {
			own := members[start[j]:start[j+1]]
			if len(own) == 0 {
				continue
			}
			clear(counter)
			for _, i := range own {
				for w, word := range points.At(i) {
					for word != 0 {
						counter[64*w+bits.TrailingZeros64(word)]++
						word &= word - 1
					}
				}
			}
			centroid := x.Centroid(j)
			for b, count := range counter {
				switch twice := 2 * int(count); {
				case twice > len(own):
					centroid[b/64] |= 1 << uint(b%64)
				case twice < len(own):
					centroid[b/64] &^= 1 << uint(b%64)
				}
				// A TIE KEEPS THE BIT the centroid already has, which
				// is what makes an unchanged assignment an unchanged
				// centroid and so lets the loop stop.
			}
		}
	}); err != nil {
		return err
	}

	// AN EMPTY LIST IS RE-SEEDED, never left: a centroid nothing is nearest
	// to is a list every document skips, and the count the corpus was sized
	// for silently shrinks. It takes the point farthest from its own
	// centroid — the one the current partition fits worst.
	var empty []int
	for j := range lists {
		if start[j] == start[j+1] {
			empty = append(empty, j)
		}
	}
	if len(empty) == 0 {
		return nil
	}
	far := make([]int, len(assignment))
	for i := range far {
		far[i] = i
	}
	sort.SliceStable(far, func(a, b int) bool {
		return distance[far[a]] > distance[far[b]]
	})
	for k, j := range empty {
		if k >= len(far) {
			break
		}
		copy(x.Centroid(j), points.At(far[k]))
	}
	return nil
}

// sampleRows is up to k distinct rows of [0, n) outside skip (ascending), in
// the seeded order a partial Fisher–Yates shuffle draws them.
func sampleRows(n, k int, seed uint64, skip []int) []int {
	rng := rand.New(rand.NewPCG(seed, ivfStream))
	rows := make([]int, 0, n-len(skip))
	next := 0
	for i := range n {
		if next < len(skip) && skip[next] == i {
			next++
			continue
		}
		rows = append(rows, i)
	}
	k = min(k, len(rows))
	for i := range k {
		j := i + rng.IntN(len(rows)-i)
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows[:k]
}

// ivfCoreShare is the share of this process's cores the index's CPU-bound
// steps — the k-means and filing every code — run on, as a divisor: HALF, and
// never fewer than [ivfMinWorkers] workers where the node has that many cores.
//
// # Why not every core
//
// The training runs on whichever node holds the embedding duty, and that node
// is also running seats and answering searches — the first stage of every
// semantic search is itself a CPU-bound scan. On every core, a training at the
// largest partition an index serves took the whole node for over a minute.
// Measured at 500 000 topical sources and 2 048 lists on this repository's
// four-core container, shared with two other test runs, with two searchers
// scanning 40 000 codes throughout (BenchmarkIVFTrainingShare): on every core
// the k-means took 48 s and the filing 31 s while the searchers' p95 went from
// 7.1 ms idle to 26.4 ms; on half, 72 s and 42 s, and 13.9 ms. Without the
// searchers the same pair was 30 s and 17 s against 42 s and 27 s. So half
// the cores costs the training about 1.45 times as long and halves what it
// adds to every search's latency meanwhile.
//
// # How long it holds the node
//
// The training runs inside the duty's tick ([Embedder.Tick]), with the lease
// renewed every [EmbedInterval] by a goroutine the training does not starve —
// it leaves the other half of the cores — and exempt from the tick's budget,
// which measures progress and which arithmetic that cannot wedge would only
// ever have cut off unfinished ([Budget]). At the ≈ 545 000 sources a node
// searches through an index, the reading at about 120 µs a source is ≈ 65 s,
// and the k-means and filing on half of four busy cores ≈ 125 s: a little
// over three minutes in the worst case measured. That holds on every node the
// share gives two workers or more, which is every node with two cores or more
// ([ivfMinWorkers] says why a two- or three-core node is not halved), and the
// k-means stops growing once the list count reaches [IVFMaxLists] — its sample
// is a fixed number of rows a list — so past that the filing and the reading
// are the only terms a larger partition lengthens.
//
// A node allowed ONE core trains on it, as every build has, and no share can
// leave its searches anything. With the same two searchers on that one core
// the training measured 237 s of k-means and 139 s of filing at 500 000
// sources (BenchmarkIVFTrainingShare -cpu 1), and 282 s and 172 s again
// pinned to one CPU of a host whose other three were busy; its reading
// measured 394 µs a source (BenchmarkIndexTraining -cpu 1, pinned the same
// way), ≈ 215 s at the largest partition. That is about eleven minutes in all,
// which a single core with two searches always in flight — already past the
// load its corpus table is measured at — spends ONCE, because the tick is
// bounded by its progress rather than by its length.
//
// # Why a constant
//
// It scales with the cores already — [ivfWorkers] reads GOMAXPROCS, which
// follows a container's CPU limit — and there is no deployment for which a
// different share is the honest answer: a node with cores to spare finishes a
// rare training sooner, and one without them needs the other half for its
// seats. An operator who wants the whole engine on fewer cores sets GOMAXPROCS.
const ivfCoreShare = 2

// ivfMinWorkers is the fewest workers a training runs on wherever the node has
// that many cores: TWO, so the share halves only a node with four or more.
//
// Half of two cores is one worker, and on two cores that bought the searches
// nothing while it nearly doubled the training. Measured at GOMAXPROCS 2 over
// the same fixture with the same two searchers (BenchmarkIVFTrainingShare
// -cpu 2), once on an otherwise idle host and once on this container shared
// with other runs: one worker took 149 s of k-means and 90 s of filing (135 s
// and 81 s shared), two took 78 s and 52 s (77 s and 47 s), and the searchers'
// p95 was 23.0 ms against 24.8 ms (22.9 against 24.7), from 4.4 ms with no
// training. Two searches always in flight on two cores already queue for each
// other's time slices, so one training worker more or less moves their tail
// by a scheduler quantum rather than by a core — while one worker makes the
// training half as long again: at the ≈ 545 000 sources an index serves it
// projects to 290–310 s with the reading, against about 195 s on two. A three-core node takes two workers for the same reason,
// where half would have been one.
const ivfMinWorkers = 2

// ivfWorkers is how many goroutines a training runs its k-means and its filing
// on: [ivfCoreShare] of the cores this process may use, never fewer than
// [ivfMinWorkers] of them — nor more than it has.
func ivfWorkers() int { return workersFor(runtime.GOMAXPROCS(0)) }

// workersFor is [ivfWorkers] for a process allowed procs cores.
func workersFor(procs int) int {
	return max(procs/ivfCoreShare, min(procs, ivfMinWorkers), 1)
}

// ivfStride is how many indices a training worker covers between two readings
// of whether its tick has ended: ONE THOUSAND AND TWENTY-FOUR.
//
// The costliest index is a row being filed, which ranks it against every
// list — at [IVFMaxLists] measured at about 110 µs idle and 170 µs on a busy
// node (BenchmarkIVFTrainingShare's filing, per row per worker) — so a stride
// is a sixth of a second of one worker's time at most, and a training cut off
// hands its cores back within that. A reading of the context costs a lock and
// a load, under a millionth of the stride it guards, so nothing is saved by a longer
// one; a shorter one buys a release no reader of a tick's log could tell from
// this one.
const ivfStride = 1024

// parallelRanges splits [0, n) into at most workers contiguous ranges and runs
// fn over each in strides of [ivfStride], returning when all have — or ctx's
// error, once every worker has finished the stride it was in when ctx ended.
//
// Every caller writes a disjoint range, which is what keeps the answer
// independent of the number of workers — so a training's cost can be bounded
// ([ivfWorkers]) without its result depending on the bound, or on the cores
// of whichever node holds the duty. A caller handed an error discards what
// the ranges wrote: a stride finished is not a range finished.
func parallelRanges(ctx context.Context, workers, n int, fn func(from, to int)) error {
	workers = min(workers, max(n, 1))
	if workers <= 1 {
		walkStrides(ctx, 0, n, fn)
		return ctx.Err()
	}
	var wg sync.WaitGroup
	step := (n + workers - 1) / workers
	for from := 0; from < n; from += step {
		wg.Add(1)
		go func(from, to int) {
			defer wg.Done()
			walkStrides(ctx, from, to, fn)
		}(from, min(from+step, n))
	}
	wg.Wait()
	return ctx.Err()
}

// walkStrides runs fn over [from, to) in strides of [ivfStride], stopping at
// the first stride boundary after ctx ends.
//
// A FUNCTION OF ITS OWN rather than a closure, so a worker doing a range's
// work is one a stack names: a goroutine that has signalled its WaitGroup is
// no longer in here although the runtime still lists it, and counting those
// read a worker on its way out beside the next round's as one worker too
// many (TestATrainingRunsOnItsShareOfTheCores).
func walkStrides(ctx context.Context, from, to int, fn func(from, to int)) {
	for at := from; at < to; at += ivfStride {
		if ctx.Err() != nil {
			return
		}
		fn(at, min(at+ivfStride, to))
	}
}

// IVFCandidates is the first stage over an index, as a pure function: the
// depth nearest codes by Hamming distance among the rows keep accepts in the
// given lists, in ascending distance with the row breaking ties. A nil keep
// accepts every row.
//
// THE SAME ORDER THE SQL PROBE PRODUCES — `ORDER BY vector_distance_cos(bits,
// vector1bit(?)), source, source_id` over rows loaded in key order — which is
// what lets a training measure, with no database, the recall the installed
// index will then serve. byList is [GroupByList]'s; every list of it is the
// full scan.
func IVFCandidates(codes Codes, byList [][]int32, lists []int, query []uint64, depth int, keep func(row int) bool) []int {
	if depth <= 0 {
		return nil
	}
	depth = min(depth, BinaryCandidateCeiling)
	top := newTopK(depth)
	for _, list := range lists {
		for _, row := range byList[list] {
			if keep != nil && !keep(int(row)) {
				continue
			}
			top.offer(int(row), float64(-Hamming(codes.At(int(row)), query)))
		}
	}
	return top.ordered()
}

// GroupByList inverts an assignment: the rows of each list, ascending.
func GroupByList(assignment []int32, lists int) [][]int32 {
	out := make([][]int32, lists)
	for row, list := range assignment {
		out[list] = append(out[list], int32(row))
	}
	return out
}

// LargestList is how many rows the fullest list of an assignment holds.
func LargestList(byList [][]int32) int {
	largest := 0
	for _, rows := range byList {
		largest = max(largest, len(rows))
	}
	return largest
}

// ProbeCount is how many lists, in probe order, a search reads: probes when it
// is not narrowed, and when it is, as many as it takes for the lists read to
// hold as many rows its filter matches as the first probes lists hold rows at
// all — reading at least probes lists and at most ceiling (or probes, when that
// is more). False means no count within that meets it, and the search runs
// the full scan instead.
//
// # Why "as many matching rows as the unfiltered probe reads rows"
//
// A filter keeping a share s of the corpus makes a query's true top 150 the
// unfiltered top 150/s — a neighbourhood 1/s times the size, so 1/s times the
// rows to hold it, of which the filter keeps s: the same number of MATCHING
// rows as the unfiltered probe reads rows. A filter that matches everything
// meets it at probes exactly, so an unfiltered search and one narrowed to
// every source read the same lists; a filter whose matches cluster near the
// query meets it early; one that matches few rows anywhere never does, and is
// the full scan's to answer — the scan reads every matching row, which is
// what such a search needed.
//
// total and matching count list's filed rows, of every source and of the
// filter's; nil matching is an unnarrowed search. matching may fail — the SQL
// path counts some lists as it goes — and the error is returned.
func ProbeCount(order []int, probes, ceiling int, total func(list int) int, matching func(list int) (int, error)) (int, bool, error) {
	probes = min(probes, len(order))
	if matching == nil {
		return probes, true, nil
	}
	ceiling = min(max(ceiling, probes), len(order))
	need := 0
	for _, list := range order[:probes] {
		need += total(list)
	}
	read := 0
	for k, list := range order {
		if k >= probes && read >= need {
			return k, true, nil
		}
		if k >= ceiling {
			return 0, false, nil
		}
		n, err := matching(list)
		if err != nil {
			return 0, false, err
		}
		read += n
	}
	if read >= need {
		return len(order), true, nil
	}
	return 0, false, nil
}

// QueryShape is one of the forms a semantic search is issued in, which a
// training and an evaluation each measure the index in.
//
// # Why an index is measured in more than one shape
//
// Every search the engine issues is NARROWED — a knowledge search to pages and
// often to the containers the company's knowledge scope names, a work search to
// tasks — and a narrowed search reads a different set of lists from an
// unfiltered one ([ProbeCount]). A training that measured only the unfiltered
// shape certified a first stage no caller runs, and installed indexes that
// lost a third of a narrowed search's answer.
type QueryShape string

const (
	// ShapeAll is a search over the whole partition.
	ShapeAll QueryShape = "all"

	// ShapeSource is a search narrowed to one source — every search the
	// engine's callers issue is at least this narrow.
	ShapeSource QueryShape = "source"

	// ShapeContainer is a search narrowed to the query document's own
	// source and container: a search scoped to one project or one space.
	ShapeContainer QueryShape = "container"
)

// QueryShapes is every shape, for the enum's validation.
var QueryShapes = []QueryShape{ShapeAll, ShapeSource, ShapeContainer}

// Valid reports whether a shape off the wire is one this build measures.
func (q QueryShape) Valid() bool { return slices.Contains(QueryShapes, q) }

// TrainingCorpus is what a training measures on: the partition's codes in key
// order, and what each row's shape filters match on.
type TrainingCorpus struct {
	Codes Codes

	// Sources is each row's source, and Containers each row's container as
	// an index into its own numbering (any consistent one).
	Sources    []Source
	Containers []int32
}

// ShapeFilter is one shape a trial is measured in: the shape, and the source
// and container it narrows to when it does.
type ShapeFilter struct {
	Shape     QueryShape
	Source    Source
	Container int32
}

// keeps reports whether a row of c is one the filter's search can return.
func (f ShapeFilter) keeps(c *TrainingCorpus, row int) bool {
	switch f.Shape {
	case ShapeSource:
		return c.Sources[row] == f.Source
	case ShapeContainer:
		return c.Sources[row] == f.Source && c.Containers[row] == f.Container
	}
	return true
}

// IVFTrial is one held-out query a training measures recall on, in one shape.
type IVFTrial struct {
	// Query is its sign code.
	Query []uint64

	// Filter is the shape it is measured in.
	Filter ShapeFilter

	// Self is the query document's own row, or -1 for a query the corpus
	// does not hold. The document is never a candidate: a search for
	// anything but itself is not answered by it, and at distance zero it
	// would take a slot of the pool every first stage fills.
	Self int

	// Want is the rows of that shape's EXACT top [ReturnDepth], best first
	// — never the query document's own row, which every first stage finds
	// and which a search for anything but itself would not be answered by.
	Want []int

	// Floor is [FloorAt] of how many rows the shape searches: a narrowed
	// search's corpus is the rows its filter keeps.
	Floor float64
}

// ProbeChoice is what a training concluded about how many lists to probe.
type ProbeChoice struct {
	// Probes is the smallest probe count that met every shape's floor with
	// no head miss, or every list when none did.
	Probes int

	// Measurement is what was measured at Probes — the worst shape's recall
	// and floor, and every shape's head misses.
	Measurement Measurement

	// Shapes is every shape's own result at Probes, for the log line an
	// operator reads when an index is not installed.
	Shapes []ShapeResult
}

// Passed reports that Probes met every shape's floor with no head miss.
// False means even every list did not — the corpus's first stage, the scan
// included, is below the floor, which is the `ivf_recall_below_floor` alarm.
func (c ProbeChoice) Passed() bool { return c.Measurement.Passed() }

// Worthwhile reports whether an index needing c.Probes of lists lists is worth
// installing: one that passed within [IVFProbeCeiling].
func (c ProbeChoice) Worthwhile(lists int) bool {
	return c.Passed() && c.Probes*IVFProbeCeiling <= lists
}

// ShapeResult is one shape's measurement at one probe count.
type ShapeResult struct {
	Shape  QueryShape
	Source Source

	// Trials is how many trials measured it, and Scanned how many of those
	// ran the full scan because their filter matched too few rows for any
	// probe count within the ceiling ([ProbeCount]).
	Trials, Scanned int

	// Recall is the mean over its trials, Floor the mean of their floors,
	// and HeadMisses the documents of an exact top ten they lost.
	Recall, Floor float64
	HeadMisses    int
}

// ChooseProbes measures recall at 1, 2, 4, … lists and returns the smallest
// probe count at which every shape meets its floor with no document missing
// from any trial's top ten — the same judgement [EvalReport.Passed] makes, so
// an index installed by this is one an evaluation passes.
//
// Each trial runs as the SQL first stage would run it: its lists chosen by
// [ProbeCount], or the full scan over its filter when that says so.
//
// # Why the recall is read off the candidate pool
//
// The rerank above the first stage is EXACT, so a document of the true top K
// that reaches the pool is in the answer: fewer than K documents in the whole
// corpus beat it, so fewer than K in the pool can. The recall of the answer is
// therefore the share of the true top K the pool holds, and no rerank is run
// to measure it.
//
// It stops between probe counts when ctx ends, for [TrainIVF]'s reason.
func ChooseProbes(ctx context.Context, c *TrainingCorpus, byList [][]int32, x IVF, trials []IVFTrial) (ProbeChoice, error) {
	lists := x.Lists()
	orders := make([][]int, len(trials))
	for t, trial := range trials {
		orders[t] = x.ProbeOrder(trial.Query)
	}
	total := func(list int) int { return len(byList[list]) }

	// EACH NARROWED FILTER'S ROWS PER LIST, counted once for every trial
	// that shares it — the shape's [ProbeCount] input — and each trial's
	// full-scan pool, which does not depend on the probe count.
	matching := map[ShapeFilter][]int{}
	for _, trial := range trials {
		if trial.Filter.Shape == ShapeAll || matching[trial.Filter] != nil {
			continue
		}
		counts := make([]int, lists)
		for list, rows := range byList {
			for _, row := range rows {
				if trial.Filter.keeps(c, int(row)) {
					counts[list]++
				}
			}
		}
		matching[trial.Filter] = counts
	}
	every := make([]int, lists)
	for j := range every {
		every[j] = j
	}
	scans := make([][]int, len(trials))

	ceiling := lists / IVFProbeCeiling
	for probes := 1; ; probes = min(2*probes, lists) {
		if err := ctx.Err(); err != nil {
			return ProbeChoice{}, fmt.Errorf("search: the index's measurement "+
				"stopped at %d probes: %w", probes, err)
		}
		results := map[ShapeFilter]*ShapeResult{}
		var order []ShapeFilter
		for t, trial := range trials {
			if len(trial.Want) == 0 {
				continue
			}
			filter, self := trial.Filter, trial.Self
			keep := func(row int) bool { return row != self && filter.keeps(c, row) }
			var counted func(int) (int, error)
			if filter.Shape != ShapeAll {
				counts := matching[filter]
				counted = func(list int) (int, error) { return counts[list], nil }
			} else if self < 0 {
				keep = nil
			}
			k, indexed, _ := ProbeCount(orders[t], probes, ceiling, total, counted)
			var pool []int
			if indexed {
				pool = IVFCandidates(c.Codes, byList, orders[t][:k], trial.Query,
					Stage1Depth, keep)
			} else {
				if scans[t] == nil {
					scans[t] = IVFCandidates(c.Codes, byList, every, trial.Query,
						Stage1Depth, keep)
				}
				pool = scans[t]
			}
			found := make(map[int]bool, len(pool))
			for _, row := range pool {
				found[row] = true
			}
			hits, head := 0, 0
			for rank, row := range trial.Want {
				if found[row] {
					hits++
				} else if rank < 10 {
					head++
				}
			}
			key := ShapeFilter{Shape: filter.Shape, Source: filter.Source}
			r := results[key]
			if r == nil {
				r = &ShapeResult{Shape: filter.Shape, Source: filter.Source}
				results[key] = r
				order = append(order, key)
			}
			r.Trials++
			if !indexed {
				r.Scanned++
			}
			r.Recall += float64(hits) / float64(len(trial.Want))
			r.Floor += trial.Floor
			r.HeadMisses += head
		}
		choice := ProbeChoice{Probes: probes,
			Measurement: Measurement{Sources: c.Codes.Len(), Recall: 0, Floor: 0}}
		worst := math.Inf(1)
		for _, key := range order {
			r := results[key]
			r.Recall /= float64(r.Trials)
			r.Floor /= float64(r.Trials)
			choice.Shapes = append(choice.Shapes, *r)
			choice.Measurement.HeadMisses += r.HeadMisses
			if margin := r.Recall - r.Floor; margin < worst {
				worst = margin
				choice.Measurement.Recall, choice.Measurement.Floor = r.Recall, r.Floor
				choice.Measurement.Shape, choice.Measurement.ShapeSource = r.Shape, r.Source
			}
		}
		if len(order) == 0 {
			// NOTHING WAS MEASURED, which no floor can be met by: a
			// training with no trial installs nothing.
			choice.Measurement.Shape = ShapeAll
			return choice, nil
		}
		if choice.Passed() || probes == lists {
			return choice, nil
		}
	}
}
