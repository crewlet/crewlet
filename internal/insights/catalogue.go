package insights

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/advisor"
)

// SchemaVersion is the record and catalogue format. A change to a published
// class is a new class id or a bump here, which turns the changed classes off
// until an administrator accepts them again.
const SchemaVersion = 1

// MaxRecordBytes bounds one encoded record, which the relay enforces too.
const MaxRecordBytes = 2048

// The bounds every class keeps, so a record stays a handful of bits.
const (
	maxFacts        = 4
	maxValues       = 6
	maxCombinations = 64
)

// Class is a catalogue id: which operating pattern a record describes.
type Class string

// FactKey names one fact a class declares.
type FactKey string

// Label is one value of a fact's closed set.
type Label string

// The facts a record may carry.
const (
	FactShare  FactKey = "share"
	FactPer100 FactKey = "per_100_agent_comments"
	FactCount  FactKey = "count"
	FactSeats  FactKey = "seats"
	FactPeople FactKey = "people"
	FactAsked  FactKey = "asked_share"
)

// Fact declares one fact of a class and the closed set it holds.
type Fact struct {
	Key    FactKey `json:"key"`
	Values []Label `json:"values"`
	// Floors are each label's lower bound, in order, and Reads the evidence
	// keys the fact is read from (a sum's keys joined by "+"), none when folded.
	Floors []int64  `json:"floors"`
	Reads  []string `json:"reads,omitempty"`
	// of reads the fact off one finding's evidence: its label, or false for a
	// finding whose evidence cannot say.
	of func(ev map[string]int64) (Label, bool)
	// folded is a fact about how many findings raised the class, not one of them.
	folded bool
}

// Entry is one class of the catalogue: its fixed words and its facts.
type Entry struct {
	Class   Class  `json:"class"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Facts   []Fact `json:"facts"`
	// worse orders two findings of the class, the worse first: whose facts
	// the folded record carries.
	worse func(a, b map[string]int64) bool
}

// bucket is a closed set of labels over a share or a count: each label holds
// from its floor up to the next one's.
type bucket struct {
	floors []int64
	labels []Label
}

// of is the label n falls in, or false below the first floor.
func (b bucket) of(n int64) (Label, bool) {
	for i := len(b.floors) - 1; i >= 0; i-- {
		if n >= b.floors[i] {
			return b.labels[i], true
		}
	}
	return "", false
}

// percent is 100*num/den rounded down, or false where den cannot divide.
func percent(num, den int64) (int64, bool) {
	if den <= 0 || num < 0 || num > den {
		return 0, false
	}
	return num * 100 / den, true
}

// shareFact is a fact holding num/den of the evidence as a percent bucket.
func shareFact(key FactKey, b bucket, num, den string) Fact {
	return Fact{Key: key, Values: b.labels, Floors: b.floors, Reads: []string{num, den}, of: func(ev map[string]int64) (Label, bool) {
		p, ok := percent(sum(ev, num), ev[den])
		if !ok {
			return "", false
		}
		return b.of(p)
	}}
}

// countFact is a fact holding one evidence count as a bucket.
func countFact(key FactKey, b bucket, count string) Fact {
	return Fact{Key: key, Values: b.labels, Floors: b.floors, Reads: []string{count}, of: func(ev map[string]int64) (Label, bool) {
		return b.of(ev[count])
	}}
}

// foldedFact is how many findings raised the class, as a bucket.
func foldedFact(key FactKey) Fact {
	return Fact{Key: key, Values: findingsBucket.labels, Floors: findingsBucket.floors, folded: true}
}

// sum is the evidence keys named by keys, joined with "+", added up.
func sum(ev map[string]int64, keys string) int64 {
	var n int64
	for _, k := range strings.Split(keys, "+") {
		n += ev[k]
	}
	return n
}

// byShare orders two findings by num/den, the larger first; a finding that
// cannot divide is never the worse.
func byShare(num, den string) func(a, b map[string]int64) bool {
	share := func(ev map[string]int64) float64 {
		if ev[den] <= 0 {
			return -1
		}
		return float64(sum(ev, num)) / float64(ev[den])
	}
	return func(a, b map[string]int64) bool { return share(a) > share(b) }
}

// byCount orders two findings by one count, the larger first.
func byCount(count string) func(a, b map[string]int64) bool {
	return func(a, b map[string]int64) bool { return a[count] > b[count] }
}

// The buckets, each starting at its class's threshold so a record never says
// less than the advisor needed to raise it.
var (
	findingsBucket = bucket{[]int64{1, 2, 4}, []Label{"1", "2-3", "4+"}}
	noopBucket     = bucket{[]int64{25, 35, 50, 75}, []Label{"25-34", "35-49", "50-74", "75+"}}
	auxBucket      = bucket{[]int64{15, 25, 40, 60}, []Label{"15-24", "25-39", "40-59", "60+"}}
	waitingBucket  = bucket{[]int64{20, 30, 50}, []Label{"20-29", "30-49", "50+"}}
	overtakenBkt   = bucket{[]int64{5, 10, 20, 40}, []Label{"5-9", "10-19", "20-39", "40+"}}
	per100Bucket   = bucket{[]int64{1, 2, 5, 10, 25}, []Label{"1", "2-4", "5-9", "10-24", "25+"}}
	namedBucket    = bucket{[]int64{5, 10, 20, 50}, []Label{"5-9", "10-19", "20-49", "50+"}}
	idleBucket     = bucket{[]int64{1, 2, 4, 10}, []Label{"1", "2-3", "4-9", "10+"}}
	noticesBucket  = bucket{[]int64{350, 700, 1400}, []Label{"350-699", "700-1399", "1400+"}}
	askedBucket    = bucket{[]int64{0, 5, 10, 25}, []Label{"0-4", "5-9", "10-24", "25+"}}
)

// catalogue is every class a record may describe: a class the advisor raises
// from its own evidence, read off the facts its detector writes.
var catalogue = []Entry{{
	Class: Class(advisor.ClassAgentMentionsWithoutAsk), Title: "Agents woke each other with comments that asked nothing",
	Summary: "Wakes per 100 agent comments, and how many agents were woken.",
	Facts: []Fact{
		{Key: FactPer100, Values: per100Bucket.labels, Floors: per100Bucket.floors,
			Reads: []string{"woken", "agent_comments"}, of: func(ev map[string]int64) (Label, bool) {
				if ev["agent_comments"] <= 0 || ev["woken"] < 0 {
					return "", false
				}
				return per100Bucket.of(ev["woken"] * 100 / ev["agent_comments"])
			}},
		countFact(FactSeats, findingsBucket, "agents"),
	},
	worse: byShare("woken", "agent_comments"),
}, {
	Class: Class(advisor.ClassPersonRequestsWithoutAsk), Title: "Agents named a person without asking them a question",
	Summary: "Comments naming the most-named person, and how many people were named.",
	Facts:   []Fact{countFact(FactCount, namedBucket, "named"), foldedFact(FactPeople)},
	worse:   byCount("named"),
}, {
	Class: Class(advisor.ClassAskOvertaken), Title: "People answered questions that no longer needed an answer",
	Summary: "The share of answers that went to a question asked again or already finished.",
	Facts:   []Fact{shareFact(FactShare, overtakenBkt, "superseded+moot", "answers")},
	worse:   byShare("superseded+moot", "answers"),
}, {
	Class: Class(advisor.ClassReviewIdle), Title: "Work in review waited a day for the agent holding it",
	Summary: "Idle reviews held by the agent holding the most, and how many agents held one.",
	Facts:   []Fact{countFact(FactCount, idleBucket, "idle"), foldedFact(FactSeats)},
	worse:   byCount("idle"),
}, {
	Class: Class(advisor.ClassNoVerifiedBackup), Title: "No recent verified backup",
	Summary: "The class alone.",
	Facts:   []Fact{},
	worse:   func(a, b map[string]int64) bool { return false },
}, {
	Class: Class(advisor.ClassNoticeLoad), Title: "A person was sent more notices than anyone reads",
	Summary: "Notices to the most-notified person, the share of them asking something, and how many people.",
	Facts: []Fact{countFact(FactCount, noticesBucket, "notices"),
		shareFact(FactAsked, askedBucket, "addressed", "notices"), foldedFact(FactPeople)},
	worse: byCount("notices"),
}, {
	Class: Class(advisor.ClassNoopShare), Title: "An agent was woken for turns that changed nothing",
	Summary: "The share of the worst agent's turns that committed nothing, and how many agents.",
	Facts:   []Fact{shareFact(FactShare, noopBucket, "nothing", "turns"), foldedFact(FactSeats)},
	worse:   byShare("nothing", "turns"),
}, {
	Class: Class(advisor.ClassPickupStillWaiting), Title: "Agents picked up work that could not move yet",
	Summary: "The share of pickups that ended in words only.",
	Facts:   []Fact{shareFact(FactShare, waitingBucket, "words", "pickups")},
	worse:   byShare("words", "pickups"),
}, {
	Class: Class(advisor.ClassAuxiliaryShare), Title: "An agent spent a large share of its tokens on auxiliary calls",
	Summary: "The worst agent's auxiliary share of its tokens, and how many agents.",
	Facts:   []Fact{shareFact(FactShare, auxBucket, "auxiliary", "tokens"), foldedFact(FactSeats)},
	worse:   byShare("auxiliary", "tokens"),
}}

// entryOf is class's catalogue entry.
func entryOf(c Class) (Entry, bool) {
	i := slices.IndexFunc(catalogue, func(e Entry) bool { return e.Class == c })
	if i < 0 {
		return Entry{}, false
	}
	return catalogue[i], true
}

// Catalogue is every class a record may describe.
func Catalogue() []Entry { return slices.Clone(catalogue) }

type catalogueExport struct {
	Schema   int               `json:"schema"`
	Patterns map[string]string `json:"patterns"`
	Limits   map[string]int    `json:"limits"`
	Classes  []Entry           `json:"classes"`
}

// CatalogueJSON is the generated export, `schema/insights-catalogue.json`,
// which the relay vendors so both ends refuse the same unknowns.
func CatalogueJSON() ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(catalogueExport{Schema: SchemaVersion,
		Patterns: map[string]string{"week": weekShape().String(), "version": versionShape().String()},
		Limits:   map[string]int{"record.bytes": MaxRecordBytes, "facts.count": maxFacts},
		Classes:  catalogue})
	if err != nil {
		return nil, fmt.Errorf("insights: encode the catalogue: %w", err)
	}
	return out.Bytes(), nil
}
