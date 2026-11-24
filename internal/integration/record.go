package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A surface's row is two records in the fleet's store (ADR-0175, "The
// integrations bucket declares its classes"): its status, a [State] at most
// [coord.MaxIntegrationStatusBytes], read at the term by every reader that
// decides something on it, and its findings, at most
// [coord.MaxIntegrationFindingsBytes], named on the status by a
// [FindingsRecord] so only a reader that shows them pays their crossing. Both
// are stored without HTML escaping, so every widest form is counted in the
// bytes JSON writes a control character or U+2028 as, six apiece. Each writer
// cuts to fit rather than refuse: losing the record would lose the phase. A
// cut may hide a seat (NamesOmitted): nothing bounds a company's seats, and
// 500 to 1,000 wordless findings naming one fill the record, so each reader
// judging a seat says it cannot tell rather than read the cut as none.

// FindingsRecord names a surface's findings record on its status, and counts
// what a reader of the status alone needs: what a person owes, and what the
// record left out.
type FindingsRecord struct {
	// Bytes and SHA256 are the record's length and digest (hex) as stored,
	// zero where the pass found nothing. A reader whose record differs reads
	// the findings as unread ([State.FindingsUnread]).
	Bytes  int    `json:"bytes,omitempty"`
	SHA256 string `json:"sha256,omitempty"`

	// Count is how many findings the record keeps.
	Count int `json:"count,omitempty"`

	// PersonOwes is how many of the pass's findings a person owes
	// ([Actor.WaitsOnAPerson]), and Naming how many name what they are about
	// (a Subject or Subjects), both counted before the cut: a reader asking
	// whether a person holds up one seat needs the findings only where both
	// are above zero, and judges from the status otherwise.
	PersonOwes int `json:"person_owes,omitempty"`
	Naming     int `json:"naming,omitempty"`

	// Expiring is how many of the pass's findings are a credential expiring
	// ([FindingCredentialExpiring]), counted before the cut: a person's
	// deadline the tools' roll-up reads from the status alone.
	Expiring int `json:"expiring,omitempty"`

	// What the cut left out ([FitFindings]), each finding counted once:
	// Omitted how many it dropped, OwedOmitted how many of those a person
	// owes; SeatOmitted how many of an agent's own ([Finding.Seat]) it kept
	// without their words (on each agent's access record), WordsOmitted how
	// many others. NamesOmitted, apart, is how many findings that may name a
	// seat ([mayNameSeat]) it dropped or cut the subject list of: only these
	// can hide a seat from a reader judging one, which then cannot say.
	Omitted      int `json:"omitted,omitempty"`
	OwedOmitted  int `json:"owed_omitted,omitempty"`
	SeatOmitted  int `json:"seat_omitted,omitempty"`
	WordsOmitted int `json:"words_omitted,omitempty"`
	NamesOmitted int `json:"names_omitted,omitempty"`
}

// Named reports whether the status names a findings record to read.
func (r FindingsRecord) Named() bool { return r.SHA256 != "" }

// MaxActionURLBytes is the longest link, as encoded, a report or a finding
// keeps: 2 KiB, past the 2,000 characters browsers and vendors reliably
// carry, so a link it drops was not one a person could follow anyway. The
// rest of a record's text is cut; a cut link would lead somewhere else.
const MaxActionURLBytes = 2 << 10

// FitFindings is findings, already [Promote]d and [bound], cut to what one
// surface's findings record keeps ([coord.MaxIntegrationFindingsBytes]) as
// encoded, and the counts of what it left out. Every word goes before any
// name, each step from the tail and only while the list is still past the
// bound: agents' own findings lose their words (on the agent's access record),
// then findings no person owes, then those a person owes; findings that can
// name no seat ([mayNameSeat]) go, those no person owes first; subject lists
// are cut to a common length, the first finding's last; the rest go, those no
// person owes first. The first, the one the report names, is never dropped
// and keeps its words, so the report and its "(and N more)", classified
// before the cut, stay true.
func FitFindings(findings []Finding) ([]Finding, FindingsRecord) {
	return fitList(findings, newListSize(findings))
}

// fitList is [FitFindings] of findings, whose sizes l holds, marking on l
// every finding it left out or cut ([listSize.isCut]), for [LogWhole].
func fitList(findings []Finding, l *listSize) (kept []Finding, rec FindingsRecord) {
	over := l.over
	if !over() {
		return findings, rec
	}
	kept = slices.Clone(findings)
	wordless, listed, dropped := map[int]bool{}, map[int]bool{}, map[int]bool{}
	// each runs step on the findings past the first that pick chooses, from
	// the tail, while the list is still past the bound.
	each := func(pick func(Finding) bool, step func(int)) {
		for i := len(kept) - 1; i > 0 && over(); i-- {
			if !dropped[i] && pick(kept[i]) {
				step(i)
			}
		}
	}
	strip := func(i int) {
		f := &kept[i]
		if f.Title == "" && f.Detail == "" && f.Remedy == "" && f.ActionURL == "" {
			return
		}
		f.Title, f.Detail, f.Remedy, f.ActionURL = "", "", "", ""
		l.resize(i, *f)
		wordless[i] = true
	}
	drop := func(i int) {
		l.drop(i)
		dropped[i] = true
	}
	own := func(f Finding) bool { return f.Seat }
	unowed := func(f Finding) bool { return !personOwed(f) }
	nameless := func(owes bool) func(Finding) bool {
		return func(f Finding) bool { return !mayNameSeat(f) && personOwed(f) == owes }
	}
	each(own, strip)
	each(unowed, strip)
	each(personOwed, strip)
	each(nameless(false), drop)
	each(nameless(true), drop)
	cutSubjects(kept, dropped, l, listed)
	each(unowed, drop)
	each(personOwed, drop)
	return counts(findings, kept, wordless, listed, dropped, rec, l)
}

// counts is what [fitList] kept, each finding counted once on rec by what the
// cut left of it (dropped, or kept without its words), apart from whether it
// may have hidden a seat's name; and every finding it cut, marked on l.
func counts(findings, kept []Finding, wordless, listed, dropped map[int]bool,
	rec FindingsRecord, l *listSize) ([]Finding, FindingsRecord) {

	out := make([]Finding, 0, len(kept))
	for i, f := range kept {
		if !dropped[i] {
			out = append(out, f)
		}
		if !dropped[i] && !wordless[i] && !listed[i] {
			continue
		}
		l.cut[i] = true
		switch {
		case dropped[i]:
			rec.Omitted++
			if personOwed(f) {
				rec.OwedOmitted++
			}
		case wordless[i] && f.Seat:
			rec.SeatOmitted++
		case wordless[i]:
			rec.WordsOmitted++
		}
		if mayNameSeat(findings[i]) && (dropped[i] || listed[i]) {
			rec.NamesOmitted++
		}
	}
	return out, rec
}

// cutSubjects cuts the subject lists of the findings past the first that are
// still kept to a common length, halved while the list is past its bound, and
// only then the first finding's, counting on each what it lost and marking
// each in listed.
func cutSubjects(kept []Finding, dropped map[int]bool, l *listSize, listed map[int]bool) {
	for _, first := range []bool{false, true} {
		longest := 0
		for i, f := range kept {
			if !dropped[i] && (i == 0) == first {
				longest = max(longest, len(f.Subjects))
			}
		}
		for limit := longest; l.over() && limit > 0; {
			limit /= 2
			for i := range kept {
				f := &kept[i]
				if dropped[i] || (i == 0) != first || len(f.Subjects) <= limit {
					continue
				}
				f.SubjectsOmitted += len(f.Subjects) - limit
				f.Subjects = f.Subjects[:limit:limit]
				l.resize(i, *f)
				listed[i] = true
			}
		}
	}
}

// personOwed reports whether a person owes what f found ([Actor.WaitsOnAPerson]).
func personOwed(f Finding) bool {
	_, actor := f.Kind.Verdict()
	return actor.WaitsOnAPerson()
}

// names reports whether f names what it is about.
func names(f Finding) bool { return f.Subject != "" || len(f.Subjects) > 0 }

// mayNameSeat reports whether a reader judging one seat matches f's names: f
// names something and is no advisory (its phase is not ready), which neither
// the roster nor the credential judgement holds a seat on.
func mayNameSeat(f Finding) bool {
	phase, _ := f.Kind.Verdict()
	return names(f) && phase != PhaseReady
}

// counted is findings' [FindingsRecord.PersonOwes], [FindingsRecord.Naming]
// and [FindingsRecord.Expiring].
func counted(findings []Finding) FindingsRecord {
	var rec FindingsRecord
	for _, f := range findings {
		if personOwed(f) {
			rec.PersonOwes++
		}
		if names(f) {
			rec.Naming++
		}
		if f.Kind == FindingCredentialExpiring {
			rec.Expiring++
		}
	}
	return rec
}

// NeedsFindingsFor reports whether a reader asking if a person holds up one
// seat must read the findings: only where a person owes one and one names
// what it is about.
func (r FindingsRecord) NeedsFindingsFor() bool { return r.PersonOwes > 0 && r.Naming > 0 }

// listSize is a list of findings' lengths as their record stores them, and
// the list's own, kept as each finding is resized or dropped: a fit asks
// whether the list is past its bound after every finding it cuts, and summing
// every length on each ask made a pass of n findings cost n squared.
type listSize struct {
	sizes []int
	// total is the list's length ([listSize.sum]) and kept how many of its
	// findings are not dropped.
	total, kept int
	// cut is, by index, each finding the fit left out or cut.
	cut []bool
	// read counts the lengths and marks read, the work a fit and [LogWhole]
	// are held linear in.
	read int
}

// newListSize is findings' lengths, summed once.
func newListSize(findings []Finding) *listSize {
	l := &listSize{sizes: make([]int, len(findings)), kept: len(findings), cut: make([]bool, len(findings))}
	for i, f := range findings {
		l.sizes[i] = findingSize(f)
	}
	l.total = l.sum()
	return l
}

// over reports whether the list is past its record's bound.
func (l *listSize) over() bool { return l.total > coord.MaxIntegrationFindingsBytes }

// resize is finding i at its new length, as f.
func (l *listSize) resize(i int, f Finding) {
	size := findingSize(f)
	l.total += size - l.sizes[i]
	l.sizes[i] = size
	l.read++
}

// drop takes finding i out of the list, and a comma with it.
func (l *listSize) drop(i int) {
	l.total -= l.sizes[i]
	if l.kept > 1 {
		l.total--
	}
	l.kept--
	l.sizes[i] = -1
	l.read++
}

// isCut reports whether the fit left finding i out or cut it.
func (l *listSize) isCut(i int) bool {
	l.read++
	return l.cut[i]
}

// findingSize is one finding's length as its record stores it; past any
// bound where it cannot be encoded, so it is cut, never stored.
func findingSize(f Finding) int {
	raw, err := encodeRecord(f)
	if err != nil {
		return coord.MaxIntegrationFindingsBytes + 1
	}
	return len(raw)
}

// sum is the list's length as JSON writes it, read from every length: the
// brackets, each finding and a comma between each two, a negative length
// being a finding dropped.
func (l *listSize) sum() int {
	total, n := 2, 0
	for _, size := range l.sizes {
		if size >= 0 {
			total += size
			n++
		}
	}
	l.read += len(l.sizes)
	return total + max(n-1, 0)
}

// encodeFindings is a findings record as stored: JSON without HTML escaping,
// so a link's & is one byte, never six.
func encodeFindings(findings []Finding) ([]byte, error) {
	return encodeRecord(findings)
}

// encodeRecord is v as a record is stored, without HTML escaping or the
// encoder's trailing newline.
func encodeRecord(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// encodeStatus is state's status as stored, within
// [coord.MaxIntegrationStatusBytes], and whether its text was cut to fit:
// its fault halved first, then its report's sentence, then the report's link
// dropped. The address and the registration's name are kept exact, since a
// reader compares them with the ones in force, and internal/config bounds
// them (MaxExternalURLLength, MaxWebhookNameLength).
func encodeStatus(state State) ([]byte, bool, error) {
	cut := false
	for {
		raw, err := encodeRecord(state)
		if err != nil {
			return nil, cut, fmt.Errorf("integration: encode the %s status: %w", state.Kind, err)
		}
		if len(raw) <= coord.MaxIntegrationStatusBytes {
			return raw, cut, nil
		}
		switch {
		case state.LastError != "":
			state.LastError = halved(state.LastError)
		case state.Report.Detail != "":
			state.Report.Detail = halved(state.Report.Detail)
		case state.Report.ActionURL != "":
			state.Report.ActionURL = ""
		default:
			return nil, cut, coord.HoldValue(coord.IntegrationStatusKey(state.Kind.String()), len(raw),
				coord.MaxIntegrationStatusBytes)
		}
		cut = true
	}
}

// halved is s cut to half its bytes, marked, or nothing where so little is
// left that the marker would not shorten it.
func halved(s string) string {
	if len(s) <= 8 {
		return ""
	}
	return textcut.Ellipsis(s, len(s)/2)
}

// digest is the hex SHA-256 a [FindingsRecord] names a record's bytes by.
func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
