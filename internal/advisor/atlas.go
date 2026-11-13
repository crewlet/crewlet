package advisor

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/authz"
)

// The classes a finding about the company's atlas can have. Each is read off
// the folded view or the sources' standings on every read and stored nowhere,
// so a finding ends when its condition does (ADR-0141).
const (
	// ClassAtlasUnowned is a confirmed service no seat or unit answers for.
	ClassAtlasUnowned Class = "atlas.unowned"
	// ClassAtlasOwnerGone is an entry whose owner the chart no longer holds.
	ClassAtlasOwnerGone Class = "atlas.owner_gone"
	// ClassAtlasUnmonitored is a confirmed service the internet reaches that
	// nothing is recorded as watching.
	ClassAtlasUnmonitored Class = "atlas.unmonitored"
	// ClassAtlasDisagreement is what the atlas's sources have disagreed on for
	// longer than [AtlasDisagreementAge].
	ClassAtlasDisagreement Class = "atlas.disagreement"
	// ClassAtlasSourceStale is a source that was read whole once and has not
	// been for [AtlasStaleReads] of its reads.
	ClassAtlasSourceStale Class = "atlas.source_stale"
)

// AtlasClasses is every class the atlas raises, in the order a reader fixes
// them.
func AtlasClasses() []Class {
	return []Class{ClassAtlasOwnerGone, ClassAtlasUnmonitored, ClassAtlasSourceStale,
		ClassAtlasUnowned, ClassAtlasDisagreement}
}

// AreaAtlas holds what the company's atlas shows needs a person, SourceAtlas
// is where such a finding was read, and SubjectAtlas what it is about.
const (
	AreaAtlas    Area        = "atlas"
	SourceAtlas  Source      = "atlas"
	SubjectAtlas SubjectKind = "atlas"
)

// [Answer.Unread]'s words for the atlas: the atlas itself, and its sources'
// standings.
const (
	UnreadAtlas        = "atlas"
	UnreadAtlasSources = "atlas_sources"
)

// The atlas thresholds, each tied to a rule the atlas already keeps.
const (
	// AtlasDisagreementAge is how long a disagreement waits in the atlas's
	// review before the advisor lists it: [Window], so every weekday has
	// passed and whoever reviews the atlas has had a working week to decide.
	AtlasDisagreementAge = Window
	// AtlasStaleReads is how many of a source's reads may pass with none read
	// whole. An entry is retired after two whole reads stop seeing it
	// (ADR-0129), so at three what should have left the map still stands on it.
	AtlasStaleReads = 3
	// MaxAtlasEntries is how many entries one finding names, the bound a
	// finding's records keep; its `entries` fact counts them all.
	MaxAtlasEntries = 5
)

// FixAtlasAct is one call of an atlas tool per entry a finding names, which
// the reader completes and sends as themselves (`POST /operator/act`,
// ADR-0024). The engine names the tool and what it already knows; nothing
// here sends it.
const FixAtlasAct FixKind = "atlas_act"

// AtlasAsk is what the reader adds to an atlas tool call before sending it.
type AtlasAsk string

// What a call can ask of the reader.
const (
	// AtlasAskNothing is a call that is complete as the engine built it.
	AtlasAskNothing AtlasAsk = ""
	// AtlasAskOwner is the seat or the unit that answers for the entry.
	AtlasAskOwner AtlasAsk = "owner"
	// AtlasAskMonitor is the monitor, or the monitoring account, that
	// watches the entry.
	AtlasAskMonitor AtlasAsk = "monitor"
	// AtlasAskValue is which of the values the sources hold is right.
	AtlasAskValue AtlasAsk = "value"
)

// AtlasAsks is every ask.
var AtlasAsks = []AtlasAsk{AtlasAskNothing, AtlasAskOwner, AtlasAskMonitor, AtlasAskValue}

// Valid reports whether a is a member of the closed set.
func (a AtlasAsk) Valid() bool { return slices.Contains(AtlasAsks, a) }

// The atlas tools a fix names: the authority table's own verbs, each a tool
// the operator surface opens to a person who holds the atlas app.
const (
	AtlasToolSetOwner = string(authz.ActionAtlasSetOwner)
	AtlasToolRelate   = string(authz.ActionAtlasRelate)
	AtlasToolConfirm  = string(authz.ActionAtlasConfirm)
	AtlasToolRefresh  = string(authz.ActionAtlasRefresh)
)

// AtlasAct is the tool call that fixes an atlas finding. Args is what every
// call carries; the entry it is about and what Asks names are added per call.
type AtlasAct struct {
	Tool string            `json:"tool"`
	Args map[string]string `json:"args,omitempty"`
	Asks AtlasAsk          `json:"asks,omitempty"`
}

// AtlasEntry is one entry a finding names: enough to link it and to name it
// to a tool. Owner is who the atlas names for it, where the finding is about
// that.
type AtlasEntry struct {
	ID    uuid.UUID    `json:"id"`
	Kind  atlas.Kind   `json:"kind"`
	Key   string       `json:"key"`
	Name  string       `json:"name" untrusted:"name"`
	Owner *atlas.Owner `json:"owner,omitempty"`
}

// AtlasSource is one source the atlas reads, as far as staleness goes.
type AtlasSource struct {
	// Kind is the integration's key and Name what a person calls it.
	Kind string
	Name string
	// LastCompleteAt is when it was last read whole, zero where it never was.
	LastCompleteAt time.Time
}

// AtlasEvidence is the atlas as one reader may read it at Until.
type AtlasEvidence struct {
	Until time.Time
	// View is the folded atlas, as the reader's own read of it answers.
	View atlas.View
	// Sources is every source the company connects, and Interval how often
	// each is read on its own.
	Sources  []AtlasSource
	Interval time.Duration
	// Holds reports whether the chart the node runs holds an owner the atlas
	// names: the reading the atlas's own owner write refuses by.
	Holds func(atlas.Owner) bool
	// Names says the reader holds the atlas app (ADR-0127). Without it a
	// finding counts entries and names none, and offers nothing to send.
	Names bool
	// ViewUnread and SourcesUnread say the caller could not read that half,
	// so its classes raise nothing and the answer names it.
	ViewUnread, SourcesUnread bool
}

// WithAtlas is answer with the findings the atlas raises.
func WithAtlas(answer Answer, ev AtlasEvidence) Answer {
	answer.Findings = append(answer.Findings, DetectAtlas(ev)...)
	sortFindings(answer.Findings)
	if ev.ViewUnread {
		answer.Unread = append(answer.Unread, UnreadAtlas)
	}
	if ev.SourcesUnread {
		answer.Unread = append(answer.Unread, UnreadAtlasSources)
	}
	return answer
}

// DetectAtlas is every finding the atlas raises: one per class over the
// entries, and one per stale source. It reads records only and calls no model.
func DetectAtlas(ev AtlasEvidence) []Finding {
	out := []Finding{}
	if !ev.ViewUnread {
		for _, d := range atlasDetectors() {
			if entries := d.entries(ev); len(entries) > 0 {
				out = append(out, atlasFinding(d, entries, ev))
			}
		}
	}
	if !ev.SourcesUnread {
		out = append(out, staleSources(ev)...)
	}
	return out
}

// atlasDetector is one class read off the entries: which it names, how costly
// it is, its words for a count of them, and the call that fixes each.
type atlasDetector struct {
	class    Class
	severity Severity
	entries  func(ev AtlasEvidence) []AtlasEntry
	words    func(n int) (title, why string)
	sentence string
	act      AtlasAct
}

// atlasDetectors is every entry class's detector, built at first use so the
// package's start allocates nothing for a list most processes never read.
var atlasDetectors = sync.OnceValue(func() []atlasDetector {
	return []atlasDetector{
		{
			class: ClassAtlasOwnerGone, severity: SeverityCosts, entries: ownerGone,
			words: func(n int) (string, string) {
				return fmt.Sprintf("%s by a seat or a unit that left the chart.", counted(n, "entry is owned", "entries are owned")),
					"The atlas still names an owner the company no longer has, so a question about " +
						"it, or a change its sources report, reaches nobody who can answer."
			},
			sentence: "Name the seat or the unit that answers for each now.",
			act:      AtlasAct{Tool: AtlasToolSetOwner, Asks: AtlasAskOwner},
		},
		{
			class: ClassAtlasUnmonitored, severity: SeverityCosts, entries: unmonitored,
			words: func(n int) (string, string) {
				return fmt.Sprintf("%s the internet and nothing is recorded as watching %s.",
						counted(n, "service is open to", "services are open to"), plural(n, "it", "them")),
					"Each exposes an address outside the company's own networks, and the atlas holds " +
						"no monitor for the service or for that address, so the first to see it fail " +
						"may be whoever uses it."
			},
			sentence: "Record the monitor, or the monitoring account, that watches each.",
			act: AtlasAct{Tool: AtlasToolRelate, Asks: AtlasAskMonitor,
				Args: map[string]string{"relation": string(atlas.RelationMonitoredBy)}},
		},
		{
			class: ClassAtlasUnowned, severity: SeverityNote, entries: unowned,
			words: func(n int) (string, string) {
				return fmt.Sprintf("%s no owner.", counted(n, "service has", "services have")),
					"Nobody is named as answering for a service the company runs, so what changes " +
						"on it or breaks has nobody to go to."
			},
			sentence: "Name the seat or the unit that answers for each.",
			act:      AtlasAct{Tool: AtlasToolSetOwner, Asks: AtlasAskOwner},
		},
		{
			class: ClassAtlasDisagreement, severity: SeverityNote, entries: disagreements,
			words: func(n int) (string, string) {
				days := int(AtlasDisagreementAge / (24 * time.Hour))
				return fmt.Sprintf("%s a disagreement nobody has decided in %d days.",
						counted(n, "entry holds", "entries hold"), days),
					"Two sources say different things about it, the atlas shows both, and nothing " +
						"settles it but a person: until one does, a reader cannot tell which is true."
			},
			sentence: "Decide each in the atlas's review: keep the value shown or take the other.",
			act:      AtlasAct{Tool: AtlasToolConfirm, Asks: AtlasAskValue},
		},
	}
})

// atlasFinding is one class's finding over entries, as the reader may read it.
func atlasFinding(d atlasDetector, entries []AtlasEntry, ev AtlasEvidence) Finding {
	title, why := d.words(len(entries))
	f := Finding{
		Class: d.class, Severity: d.severity, Actor: ActorOwner, Source: SourceAtlas, Area: AreaAtlas,
		Subject: Subject{Kind: SubjectAtlas}, Title: title, Why: why,
		Evidence: []Fact{{Key: "entries", Value: int64(len(entries))}},
	}
	return named(f, entries, d.sentence, d.act, ev)
}

// named gives f its entries and its fix where the reader holds the atlas app.
// WITHOUT IT THE FINDING COUNTS AND NAMES NOTHING: no entry, no id, and no
// call, which the atlas's own tools would refuse them anyway (ADR-0127).
func named(f Finding, entries []AtlasEntry, sentence string, act AtlasAct, ev AtlasEvidence) Finding {
	if !ev.Names {
		f.Fix = Fix{Kind: FixNone, Options: []Option{},
			Sentence: "Reading the atlas takes the Atlas permission. Somebody who holds it sees which, and can fix it."}
		return f
	}
	f.Entries = entries[:min(len(entries), MaxAtlasEntries)]
	f.Fix = Fix{Kind: FixAtlasAct, Sentence: sentence, Options: []Option{}, Act: &act}
	return f
}

// current reports whether e is on the map as something the company stands
// behind: confirmed, so neither retired nor a report nobody has checked.
func current(e *atlas.Entry) bool { return e.State == atlas.StateConfirmed }

// ownerOf is who answers for e: the seat or the unit a person or the engine's
// records name, never one only suggested ([atlas.Summaries.Of]'s reading).
func ownerOf(e *atlas.Entry) *atlas.Owner {
	for _, ed := range e.Relations {
		if ed.Relation == atlas.RelationOwnedBy && !ed.Suggested && ed.External != "" {
			return &atlas.Owner{Kind: ed.External, Name: ed.Name}
		}
	}
	return nil
}

func entryOf(e *atlas.Entry) AtlasEntry {
	return AtlasEntry{ID: e.ID, Kind: e.Ref.Kind, Key: e.Ref.Key, Name: e.Name}
}

// enginesOwn reports an entry only the engine's own records name: the engine
// itself, which its nodes' presence draws (ADR-0142). Nobody put it on the
// map, so nobody is asked who answers for it.
func enginesOwn(e *atlas.Entry) bool {
	return len(e.Sources) > 0 && !slices.ContainsFunc(e.Sources, func(s atlas.Source) bool {
		return s.Kind != atlas.SourceRecords
	})
}

// unowned is every confirmed service with no owner that somebody, a person,
// the settings or an integration, put on the map.
func unowned(ev AtlasEvidence) []AtlasEntry {
	var out []AtlasEntry
	for i := range ev.View.Entries {
		e := &ev.View.Entries[i]
		if e.Ref.Kind == atlas.KindService && current(e) && !enginesOwn(e) && ownerOf(e) == nil {
			out = append(out, entryOf(e))
		}
	}
	return out
}

// ownerGone is every entry on the map whose owner is a seat or a unit the
// chart does not hold. Any kind: an owner is named on whatever it answers for.
func ownerGone(ev AtlasEvidence) []AtlasEntry {
	var out []AtlasEntry
	if ev.Holds == nil {
		return out
	}
	for i := range ev.View.Entries {
		e := &ev.View.Entries[i]
		owner := ownerOf(e)
		if e.State == atlas.StateRetired || owner == nil || ev.Holds(*owner) {
			continue
		}
		entry := entryOf(e)
		entry.Owner = owner
		out = append(out, entry)
	}
	return out
}

// watched reports whether a monitor, or a monitoring account, is recorded for e.
func watched(e *atlas.Entry) bool {
	return slices.ContainsFunc(e.Relations, func(ed atlas.Edge) bool {
		return ed.Relation == atlas.RelationMonitoredBy
	})
}

// unmonitored is every confirmed service that exposes an address the
// internet reaches, where neither it nor any address it exposes is watched.
// PUBLIC IS WHAT THE RECORD SAYS, never a guess: an exposed endpoint whose
// host is not on a private network ([atlas.Ref.Private]).
func unmonitored(ev AtlasEvidence) []AtlasEntry {
	index := ev.View.Index()
	var out []AtlasEntry
	for i := range ev.View.Entries {
		e := &ev.View.Entries[i]
		if e.Ref.Kind != atlas.KindService || !current(e) || watched(e) {
			continue
		}
		public, covered := false, false
		for _, ed := range e.Relations {
			at, known := index[ed.To]
			if ed.Relation != atlas.RelationExposes || ed.External != "" || !known {
				continue
			}
			endpoint := &ev.View.Entries[at]
			if endpoint.State == atlas.StateRetired || endpoint.Ref.Private() {
				continue
			}
			public = true
			covered = covered || watched(endpoint)
		}
		if public && !covered {
			out = append(out, entryOf(e))
		}
	}
	return out
}

// disagreements is every entry on the map holding a fact or a relation its
// sources have disagreed on since before [AtlasDisagreementAge] ago, counted
// from the newest claim in it, which is when it began to stand as it does.
func disagreements(ev AtlasEvidence) []AtlasEntry {
	before := ev.Until.Add(-AtlasDisagreementAge)
	old := func(standing atlas.Standing, shown time.Time, others []atlas.Claimed) bool {
		if standing == atlas.StandingSettled {
			return false
		}
		for _, o := range others {
			if o.ObservedAt.After(shown) {
				shown = o.ObservedAt
			}
		}
		return shown.Before(before)
	}
	var out []AtlasEntry
	for i := range ev.View.Entries {
		e := &ev.View.Entries[i]
		if e.State == atlas.StateRetired {
			continue
		}
		waits := slices.ContainsFunc(e.Facts, func(f atlas.FactView) bool {
			return old(f.Standing, f.ObservedAt, f.Others)
		}) || slices.ContainsFunc(e.Relations, func(ed atlas.Edge) bool {
			return old(ed.Standing, ed.ObservedAt, ed.Others)
		})
		if waits {
			out = append(out, entryOf(e))
		}
	}
	return out
}

// staleSources is one finding per source that was read whole once and has not
// been for [AtlasStaleReads] of its reads. A source never read whole is not
// one that stopped: its own standing and its integration's findings say why.
func staleSources(ev AtlasEvidence) []Finding {
	var out []Finding
	if ev.Interval <= 0 {
		return out
	}
	for _, s := range ev.Sources {
		quiet := ev.Until.Sub(s.LastCompleteAt)
		if s.LastCompleteAt.IsZero() || quiet < AtlasStaleReads*ev.Interval {
			continue
		}
		f := Finding{
			Class: ClassAtlasSourceStale, Severity: SeverityCosts, Actor: ActorOwner, Source: SourceAtlas,
			Area: AreaAtlas, Subject: Subject{Kind: SubjectIntegration, Name: s.Name},
			Path:  "integrations." + s.Kind,
			Title: fmt.Sprintf("The atlas has not read %s whole in %s.", s.Name, hoursOrDays(quiet)),
			Why: "What it last read still stands on the map and looks current: a change at the " +
				"vendor since then is not on it, and nothing it stopped listing has left.",
			Evidence: []Fact{{Key: "hours", Value: int64(quiet / time.Hour)}},
		}
		out = append(out, named(f, nil, "Ask for a read now; its standing in the atlas's sources says what stopped it.",
			AtlasAct{Tool: AtlasToolRefresh, Args: map[string]string{"integration": s.Kind}}, ev))
	}
	return out
}

// counted is n with the words for one or for many.
func counted(n int, one, many string) string {
	return fmt.Sprintf("%d %s", n, plural(n, one, many))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// hoursOrDays is d as a reader counts it: whole hours under two days, whole
// days from there.
func hoursOrDays(d time.Duration) string {
	if d < 48*time.Hour {
		return counted(int(d/time.Hour), "hour", "hours")
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}
