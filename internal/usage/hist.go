package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"
)

// Hist is a turn-duration histogram: counts in geometric bins, 2^(1/6) wide,
// from one second to a day.
//
// Not a list of durations (a busy seat ends hundreds of turns a day, republished
// every flush) nor a stored p50 (a median of medians is not the fleet's): a
// record is a few dozen pairs and histograms merge by adding counts, so any
// window over any nodes is exact to the bin.
//
// 2^(1/6): a quantile read is the bin's geometric midpoint, within 2^(1/12) of
// every value in it, at most [QuantileResolution] (under 6%). Outside the range
// the error is unbounded: a turn under a second is counted in the first bin,
// and one past the last bin's upper edge (about 26 hours) in the last.
type Hist struct {
	// bins is sparse and sorted by index — the canonical form, so two
	// histograms holding the same counts encode to the same bytes.
	bins []histBin
}

type histBin struct {
	index int
	count int64
}

const (
	// HistBinsPerDoubling is the resolution: six bins every time a duration
	// doubles.
	HistBinsPerDoubling = 6

	// HistFloor is the lower edge of the first bin.
	HistFloor = time.Second

	// HistCeiling is the longest duration the range is stated for.
	HistCeiling = 24 * time.Hour
)

// HistBins is how many bins cover [HistFloor, HistCeiling]: the last one's
// lower edge is the last bin edge at or below the ceiling.
var HistBins = int(math.Floor(HistBinsPerDoubling*math.Log2(float64(HistCeiling)/float64(HistFloor)))) + 1

// QuantileResolution is the worst relative error of a quantile read inside the
// stated range: half a bin, geometrically.
var QuantileResolution = math.Pow(2, 1.0/(2*HistBinsPerDoubling)) - 1

// binOf is the bin a duration is counted in.
func binOf(d time.Duration) int {
	if d <= HistFloor {
		return 0
	}
	i := int(math.Floor(HistBinsPerDoubling * math.Log2(float64(d)/float64(HistFloor))))
	return min(max(i, 0), HistBins-1)
}

// midpoint is the value a bin reads back as: its geometric centre.
func midpoint(i int) time.Duration {
	return time.Duration(float64(HistFloor) * math.Pow(2, (float64(i)+0.5)/HistBinsPerDoubling))
}

// Add counts one duration.
func (h *Hist) Add(d time.Duration) { h.addN(binOf(d), 1) }

func (h *Hist) addN(index int, n int64) {
	if n == 0 {
		return
	}
	at, found := slices.BinarySearchFunc(h.bins, index, func(b histBin, i int) int {
		return b.index - i
	})
	if found {
		h.bins[at].count += n
		return
	}
	h.bins = slices.Insert(h.bins, at, histBin{index: index, count: n})
}

// Merge adds another histogram's counts into this one.
func (h *Hist) Merge(o Hist) {
	for _, b := range o.bins {
		h.addN(b.index, b.count)
	}
}

// Join is the least histogram holding, AT OR ABOVE every bin, as many
// durations as the more of h and o does there — the join a node's published
// day takes of what it derived and what the fleet holds (see join.go).
//
// AT OR ABOVE, never bin by bin: a turn whose end states no tally is measured
// over the segments its node holds, so as a later one arrives the turn moves UP
// a bin and its old bin's count falls. The count at or above any bin cannot
// fall that way — a turn added raises it, a turn moved up raises it or leaves
// it — so it is the figure that only grows, and the one a join may take the
// larger of. Its total is the larger of the two totals, as the turns it counts
// are. And where o is at or below h at every bin — an earlier measurement of
// the same turns — the join is h exactly.
func (h Hist) Join(o Hist) Hist {
	var (
		out            []histBin
		inH, inO, prev int64
	)
	i, j := len(h.bins)-1, len(o.bins)-1
	for i >= 0 || j >= 0 {
		index := -1
		if i >= 0 {
			index = h.bins[i].index
		}
		if j >= 0 && o.bins[j].index > index {
			index = o.bins[j].index
		}
		if i >= 0 && h.bins[i].index == index {
			inH += h.bins[i].count
			i--
		}
		if j >= 0 && o.bins[j].index == index {
			inO += o.bins[j].count
			j--
		}
		atOrAbove := max(inH, inO)
		if n := atOrAbove - prev; n > 0 {
			out = append(out, histBin{index: index, count: n})
		}
		prev = atOrAbove
	}
	slices.Reverse(out)
	return Hist{bins: out}
}

// Count is how many durations the histogram holds.
func (h Hist) Count() int64 {
	var n int64
	for _, b := range h.bins {
		n += b.count
	}
	return n
}

// Quantile is the q-th quantile (0 < q <= 1), read as the midpoint of the bin
// it falls in, and false for an empty histogram — an absent value rather than
// a zero duration, which would read as a seat whose turns take no time.
func (h Hist) Quantile(q float64) (time.Duration, bool) {
	total := h.Count()
	if total == 0 || q <= 0 || q > 1 {
		return 0, false
	}
	// THE NEAREST-RANK DEFINITION: the smallest value with at least q of
	// the counts at or below it, which is a value that actually occurred
	// rather than an interpolation between two bins.
	rank := int64(math.Ceil(q * float64(total)))
	var seen int64
	for _, b := range h.bins {
		seen += b.count
		if seen >= rank {
			return midpoint(b.index), true
		}
	}
	return midpoint(h.bins[len(h.bins)-1].index), true
}

// MarshalJSON writes the sparse pairs, `[[bin, count], …]`, in bin order.
func (h Hist) MarshalJSON() ([]byte, error) {
	pairs := make([][2]int64, 0, len(h.bins))
	for _, b := range h.bins {
		pairs = append(pairs, [2]int64{int64(b.index), b.count})
	}
	return json.Marshal(pairs)
}

// UnmarshalJSON reads the pairs back, refusing a bin outside the range, a
// count that is not positive, and a bin named twice — each is a histogram a
// build could not have written, and merging it would count durations nobody
// measured.
func (h *Hist) UnmarshalJSON(b []byte) error {
	var pairs [][2]int64
	if err := json.Unmarshal(b, &pairs); err != nil {
		return fmt.Errorf("usage: decode a duration histogram: %w", err)
	}
	out := Hist{}
	for _, p := range pairs {
		index, count := p[0], p[1]
		if index < 0 || index >= int64(HistBins) {
			return fmt.Errorf("usage: a duration histogram names bin %d, and "+
				"bins run 0..%d", index, HistBins-1)
		}
		if count <= 0 {
			return fmt.Errorf("usage: a duration histogram counts %d in bin %d "+
				"— an empty bin is absent, never zero", count, index)
		}
		if slices.ContainsFunc(out.bins, func(x histBin) bool { return x.index == int(index) }) {
			return fmt.Errorf("usage: a duration histogram names bin %d twice", index)
		}
		out.addN(int(index), count)
	}
	*h = out
	return nil
}
