package search

import (
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE INDEX'S OWN RECORDS on the vector log — ADR-0022 — and why they are
// records rather than something each node computes for itself.
//
// # Published once, applied everywhere, like the vectors themselves
//
// The centroids are the product of a training no node should repeat: a
// seeded k-means over every code in the partition, plus a recall measurement
// that scans the exact vectors. Trained by the embedding duty — the log's one
// writer — and PUBLISHED, they are applied into every holder's file by the
// same applier that writes the rows they index, so every holder holds the same
// index at the same position, and a node adopting a snapshot adopts the index
// inside it rather than rebuilding it.
//
// # The index's generation is the record's POSITION, never a counter
//
// A counter the duty minted — "the last generation plus one" — can be minted
// twice: a publish whose outcome was unknown, a lease that moved mid-training.
// Both records would sit on the one centroids subject, the compaction would
// keep the second, and a node replaying the log would hold the second's
// centroids under the number every live node filed its rows against the
// first's. A position is minted by the broker, once, identically on every
// node: two trainings are simply two generations, the later superseding the
// earlier, and a row filed under the earlier one is stale rather than wrong.
//
// # Why the re-filing is records too, and why its ranges are FIXED
//
// When a new index is installed every row is filed under the old one. A node
// could re-file them itself, but not identically: a live node would do it at
// one point in its log and a replaying node at another, and a snapshot taken
// between the two would carry half of each. So the duty publishes the re-filing
// as [OpReassign] records, each naming one batch of the index's ROLLOUT, and
// each holder applies them in log order — the same rows re-filed at the same
// position everywhere, and never a whole partition inside one apply
// transaction.
//
// The rollout's key ranges are cut ONCE, by the training, from the ids the
// corpus held then, and carried in the centroids record itself ([IndexRecord]
// Rollout). They were once cut afresh from whatever ids the duty's node held
// at each publication, on subjects numbered only by batch — and a
// re-publication after new ids had arrived shifted every boundary, so a
// re-publication that stopped partway, or whose publish outcome was unknown,
// left the compacted log holding a mix of two cuts that no longer tiled the
// key space. A node replaying it kept those rows unfiled for ever, and the
// duty — whose own rows the superseded batches HAD filed — never noticed. With
// the ranges a function of the generation alone, every publication of batch n
// says the same thing, so any mix of them tiles.
//
// And each batch's subject names its GENERATION as well as its number, so a
// batch of one index can never supersede a batch of another on the compacted
// log — not even one a second publisher wrote from an index it had not yet
// seen replaced. A retired generation's batches leave with the stream's own
// age bound ([VectorLogMaxAge]) like every other record; they cost a few
// hundred bytes each and apply as nothing meanwhile.
//
// # Everything about an index NESTS under its centroids
//
// A reassign batch and a measurement mean something only against the index
// they name, so their scope paths sit BENEATH the centroids record's
// ([IndexScopePath]). A node that must defer a centroids record — one a newer
// build reshaped at a version this one cannot read — then retains that index's
// batches and measurements behind it and applies them after it, rather than
// applying them first as no-ops against the index it still holds and never
// re-filing the rows once it can read the one they were for.

// IndexSource is the subject KIND every record about the index is filed under.
//
// NOT A SOURCE this build embeds — [Sources] does not list it — but the same
// field of the same subject on the wire, because the envelope's shape is fixed
// for every build ([RecordEnvelope]).
const IndexSource Source = "ivf"

// IndexCentroids is the one subject the installed index lives on, so the
// compacted log keeps exactly the current one.
var IndexCentroids = Subject{Source: IndexSource, ID: "centroids"}

// ReassignSubject is batch n of the rollout of the index at generation index.
//
// QUALIFIED BY THE GENERATION: see the file's doc. The compaction keeps one
// record per (index, batch), which is exactly one statement of one range.
func ReassignSubject(index int64, batch int) Subject {
	return Subject{Source: IndexSource,
		ID: "reassign." + strconv.FormatInt(index, 10) + "." + strconv.Itoa(batch)}
}

// MeasureSubject is where the measurements of the index at generation index
// are published: one subject per generation, so the compaction keeps the
// newest measurement of each index and none of one index can replace
// another's.
func MeasureSubject(index int64) Subject {
	return Subject{Source: IndexSource, ID: "measure." + strconv.FormatInt(index, 10)}
}

// IndexScopeRoot is the scope level every index record lives under.
//
// A ROOT OF ITS OWN, beside the documents' [ScopeRoot] rather than under it,
// because an index record writes no document: a deferred centroids record on
// an older build must not block — or be blocked by — a read or a write of any
// document, and a path under [ScopeRoot] would cover every one of them.
const IndexScopeRoot = "ivf"

// IndexScopePath is an index record's one scope path: the centroids record's
// own, and every other index record's BENEATH it — see the file's doc.
func IndexScopePath(subject Subject) string {
	centroids := IndexScopeRoot + statelog.ScopeSeparator + IndexCentroids.String()
	if subject == IndexCentroids {
		return centroids
	}
	return centroids + statelog.ScopeSeparator + subject.ID
}

// IndexRecord is the partition's semantic index as the duty trained it — or
// the verdict that it has none.
type IndexRecord struct {
	// Log is the vector log the index was trained for, and Basis the
	// generation it replaces — zero for the first. Seed is the training's
	// seed, [IVFSeed] of the two: carried so an operator, or a test, can
	// re-train it bit for bit. The log is also what a process holding many
	// partitions keys its decoded copy of each partition's index on.
	Log   string `json:"log"`
	Basis int64  `json:"basis"`
	Seed  uint64 `json:"seed"`

	// Lists is how many lists the index has. ZERO IS A VERDICT, not an
	// absence: the partition has no index and every search scans, because it
	// is below [IVFMinCorpus] or because no probe count within
	// [IVFProbeCeiling] met the floor (Why says which).
	Lists int `json:"lists"`

	// Probes is how many lists a search reads: the smallest count whose
	// measured recall met the floor with no head miss ([ChooseProbes]).
	Probes int `json:"probes,omitempty"`

	// TrainedOn is how many sources the partition held when it was
	// trained, which is what the retrain rule is read at.
	TrainedOn int `json:"trained_on"`

	// Largest is the most rows the training filed in any one list. The
	// imbalance rule judges a list's growth AGAINST THIS rather than against
	// the mean alone: a corpus with many identical codes — blank tasks, a
	// template copied a thousand times — piles into one list whatever the
	// seed, and an absolute rule retrained it every other tick for ever.
	Largest int `json:"largest,omitempty"`

	// Measurement is what the training measured against the exact scan —
	// at Probes, or at every list on a verdict that the index was not worth
	// installing. Nil when nothing was measured: a partition retired for its
	// size is one nobody measured.
	Measurement *Measurement `json:"measurement,omitempty"`

	// Why is the verdict's reason, empty on an installed index.
	Why IndexVerdict `json:"why,omitempty"`

	// Centroids are [IVF.Bytes]: Lists codes of the record's width.
	Centroids []byte `json:"centroids,omitempty"`

	// Rollout is the index's re-filing, cut once by the training: batch n
	// re-files Rollout[n]. Together the ranges tile every listed source's
	// whole key space — see [RolloutRanges].
	Rollout []RolloutRange `json:"rollout,omitempty"`
}

// RolloutRange is one batch of a rollout: the ids of one source in [From, To),
// From empty meaning the source's start and To empty its end.
//
// A RANGE OF ONE SOURCE'S IDS, so the apply is a seek on the narrow table's
// primary key, `(source, source_id)`, rather than a comparison across two
// columns no index serves.
type RolloutRange struct {
	Source Source `json:"source"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
}

// Holds reports whether the range covers one source id.
func (r RolloutRange) Holds(source Source, id string) bool {
	return r.Source == source && id >= r.From && (r.To == "" || id < r.To)
}

// Measurement is what one measurement of an index found against the exact
// scan, summarised over every query shape it measured ([QueryShape]).
//
// THE WORST SHAPE'S RECALL AND FLOOR, and every shape's head misses: the
// index passed only if every shape met its own floor with no head miss, and
// the shape closest to failing is the one an operator needs named.
type Measurement struct {
	// Sources is how many sources the partition held when it was measured.
	Sources int `json:"sources"`

	// Recall and Floor are the worst shape's: its mean recall, and the
	// floor [FloorAt] puts on the corpus that shape searches.
	Recall float64 `json:"recall"`
	Floor  float64 `json:"floor"`

	// Shape is which shape that was, and ShapeSource its source when the
	// shape names one.
	Shape       QueryShape `json:"shape"`
	ShapeSource Source     `json:"shape_source,omitempty"`

	// HeadMisses is how many documents of an exact top ten every shape's
	// queries lost, together.
	HeadMisses int `json:"head_misses,omitempty"`
}

// Passed reports whether every shape met its floor with no head miss.
//
// The worst shape's margin decides the first half and the total the second,
// which is the judgement [EvalReport.Passed] makes shape by shape.
func (m Measurement) Passed() bool { return m.Recall >= m.Floor && m.HeadMisses == 0 }

// validate refuses a measurement no measuring could have produced.
//
// STRUCTURE ONLY, for [IndexRecord.validate]'s reason: a shape this build has
// not heard of is a later build's report, and a report is not a rule.
func (m Measurement) validate() error {
	if m.Sources < 0 || m.HeadMisses < 0 {
		return fmt.Errorf("search: a measurement over %d sources with %d head "+
			"misses", m.Sources, m.HeadMisses)
	}
	for _, v := range []float64{m.Recall, m.Floor} {
		if math.IsNaN(v) || v < 0 || v > 1 {
			return fmt.Errorf("search: a measurement's recall %v or floor %v is "+
				"outside [0, 1]", m.Recall, m.Floor)
		}
	}
	if m.Shape == "" {
		return fmt.Errorf("search: a measurement names no shape")
	}
	return nil
}

// IndexVerdict is why a partition has no index.
type IndexVerdict string

const (
	// VerdictTooSmall is a partition below [IVFMinCorpus].
	VerdictTooSmall IndexVerdict = "below_min_corpus"

	// VerdictNotWorthwhile is an index whose recall needed more than
	// [IVFProbeCeiling] of its lists — or every list and still missed the
	// floor, which the recorded measurement says.
	VerdictNotWorthwhile IndexVerdict = "not_worthwhile"
)

// IndexVerdicts is every verdict this build writes.
var IndexVerdicts = []IndexVerdict{VerdictTooSmall, VerdictNotWorthwhile}

// Valid reports whether a verdict off the wire is one this build knows.
func (v IndexVerdict) Valid() bool { return slices.Contains(IndexVerdicts, v) }

// validate refuses an index record no training could have written.
//
// STRUCTURE ONLY — the shape of the bytes and the ranges, never a TUNING
// CONSTANT. A decoded record is a record another build wrote, and a check
// against this build's [IVFMaxLists] would refuse, as a writer fault, the
// index of a later build that raised the ceiling and wrote it at the same
// version because the record's shape did not change: every older node of the
// upgrade would fail the record on every redelivery. The ceilings are the
// trainer's to respect ([IVFLists]); a reader applies what the structure
// allows. The same goes for a verdict this build has not heard of: a record
// with no lists is "no index" whatever it calls the reason.
func (x IndexRecord) validate(model string, dim int) error {
	if model == "" || dim <= 0 {
		return fmt.Errorf("search: an index record names no embedding space — "+
			"an index trained in one space and probed with a query from another "+
			"ranks arithmetic on incompatible codes (model %q, width %d)", model, dim)
	}
	if !isToken(x.Log) {
		return fmt.Errorf("search: an index record names no vector log (%q) — "+
			"a process holding many partitions keys each one's decoded index "+
			"on it", x.Log)
	}
	if x.Measurement != nil {
		if err := x.Measurement.validate(); err != nil {
			return err
		}
	}
	if x.TrainedOn < 0 || x.Largest < 0 {
		return fmt.Errorf("search: an index trained on %d sources with a largest "+
			"list of %d", x.TrainedOn, x.Largest)
	}
	if x.Lists == 0 {
		if x.Why == "" || len(x.Centroids) != 0 || len(x.Rollout) != 0 {
			return fmt.Errorf("search: an index record with no lists must say "+
				"why (%q) and carry no centroids (%d bytes) and no rollout (%d "+
				"batches)", x.Why, len(x.Centroids), len(x.Rollout))
		}
		return nil
	}
	if x.Why != "" {
		return fmt.Errorf("search: an installed index carries the verdict %q", x.Why)
	}
	if x.Lists < 0 || x.Probes < 1 || x.Probes > x.Lists {
		return fmt.Errorf("search: an index of %d lists probing %d — a probe "+
			"reads at least one list and at most every one", x.Lists, x.Probes)
	}
	if want := 8 * CodeWords(dim) * x.Lists; len(x.Centroids) != want {
		return fmt.Errorf("search: an index of %d lists at %d dimensions carries "+
			"%d centroid bytes, not %d", x.Lists, dim, len(x.Centroids), want)
	}
	return validateRollout(x.Rollout)
}

// validateRollout refuses ranges that do not tile their sources: grouped by
// source in ascending order, each source's ranges contiguous from its start to
// its end. A gap is a row no batch ever re-files; an overlap is one two
// batches do.
func validateRollout(rollout []RolloutRange) error {
	if len(rollout) == 0 {
		return fmt.Errorf("search: an installed index carries no rollout — its " +
			"older rows could never be re-filed under it")
	}
	for i, r := range rollout {
		if !isToken(string(r.Source)) {
			return fmt.Errorf("search: rollout batch %d names the source %q", i, r.Source)
		}
		first := i == 0 || rollout[i-1].Source != r.Source
		last := i == len(rollout)-1 || rollout[i+1].Source != r.Source
		switch {
		case first && i > 0 && rollout[i-1].Source > r.Source:
			return fmt.Errorf("search: rollout batch %d's source %q follows %q — "+
				"the batches are grouped by source in ascending order, so a "+
				"source cannot appear twice", i, r.Source, rollout[i-1].Source)
		case first && r.From != "":
			return fmt.Errorf("search: rollout batch %d opens %s at %q rather than "+
				"at its start", i, r.Source, r.From)
		case !first && r.From != rollout[i-1].To:
			return fmt.Errorf("search: rollout batch %d starts %s at %q and the "+
				"batch before it ends at %q", i, r.Source, r.From, rollout[i-1].To)
		case r.To != "" && r.To <= r.From:
			return fmt.Errorf("search: rollout batch %d's range [%q, %q) is empty",
				i, r.From, r.To)
		case last && r.To != "":
			return fmt.Errorf("search: rollout batch %d closes %s at %q rather "+
				"than at its end", i, r.Source, r.To)
		case !last && r.To == "":
			return fmt.Errorf("search: rollout batch %d runs %s to its end with "+
				"another batch of it after", i, r.Source)
		}
	}
	return nil
}

// ReassignRecord re-files one batch of an index's rollout.
//
// IT NAMES THE BATCH AND NOT THE RANGE: the range is the one the index's own
// centroids record fixed ([IndexRecord] Rollout), so no publication of a batch
// can say anything but what every other publication of it said.
type ReassignRecord struct {
	// Index is the generation the rows are re-filed under: the packed
	// position of the centroids record that installed it. A batch naming
	// any other generation applies nothing — its index has been replaced.
	Index int64 `json:"index"`

	// Batch is which range of the index's rollout this re-files, and
	// Batches how many the rollout has, for an operator reading the log.
	Batch   int `json:"batch"`
	Batches int `json:"batches"`
}

// validate refuses a batch that cannot re-file anything.
func (r ReassignRecord) validate(subject Subject) error {
	if r.Index <= 0 {
		return fmt.Errorf("search: a reassign record names no index")
	}
	if r.Batch < 0 || r.Batch >= r.Batches {
		return fmt.Errorf("search: reassign batch %d of %d", r.Batch, r.Batches)
	}
	if subject != ReassignSubject(r.Index, r.Batch) {
		return fmt.Errorf("search: reassign batch %d of index %d on subject %s — "+
			"each batch of each index has its own subject, %s, so the compaction "+
			"keeps one statement of each", r.Batch, r.Index, subject,
			ReassignSubject(r.Index, r.Batch))
	}
	return nil
}

// MeasureRecord is a later measurement of an installed index, against the
// corpus as it is now: the probe count that meets the floor today, and what it
// measured.
//
// A RECORD OF ITS OWN rather than a re-published centroids record, because a
// probe count is a fact about how the index is READ and changes no row: a new
// centroids record is a new generation, and every row of the partition would
// be re-filed to change one number.
type MeasureRecord struct {
	// Index is the generation measured. Any other generation's measurement
	// applies nothing.
	Index int64 `json:"index"`

	// Probes is the smallest probe count that met the floor with no head
	// miss, which searches read from the moment this applies.
	Probes int `json:"probes"`

	// Measurement is what that count measured.
	Measurement Measurement `json:"measurement"`
}

// validate refuses a measurement that could not have come from a measuring.
func (m MeasureRecord) validate(subject Subject) error {
	if m.Index <= 0 || m.Probes < 1 {
		return fmt.Errorf("search: a measurement of index %d at %d probes", m.Index, m.Probes)
	}
	if subject != MeasureSubject(m.Index) {
		return fmt.Errorf("search: a measurement of index %d on subject %s — "+
			"each index's measurements have their own subject, %s", m.Index,
			subject, MeasureSubject(m.Index))
	}
	return m.Measurement.validate()
}
