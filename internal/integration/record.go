package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
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

	// What the cut left out ([FitFindings]): Omitted how many findings it
	// dropped; SeatOmitted how many of an agent's own ([Finding.Seat]) it kept
	// without their words (on each agent's access record), WordsOmitted how
	// many others; NamesOmitted how many findings that may name a seat
	// ([mayNameSeat]) it dropped or cut the subject list of. Only the last
	// can hide a seat from a reader judging one, which then cannot say.
	Omitted      int `json:"omitted,omitempty"`
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
// encoded, and the counts of what it left out. Names go after words and what
// a person owes after what nobody does, each step only while the list is still
// past the bound, from the tail: agents' own findings lose their words (on the
// agent's access record); findings no person owes lose theirs, then go; those
// a person owes lose theirs; subject lists are cut to a common length, the
// first finding's last; those a person owes go. The first, the one the report
// names, is never dropped, so the report and its "(and N more)", classified
// before the cut, stay true.
func FitFindings(findings []Finding) ([]Finding, FindingsRecord) {
	kept, rec, _ := fitFindings(findings)
	return kept, rec
}

// fitFindings is [FitFindings], and the index in findings of every finding it
// left out or cut, for [LogWhole].
func fitFindings(findings []Finding) (kept []Finding, rec FindingsRecord, unkept []int) {
	sizes := findingSizes(findings)
	over := func() bool { return listBytes(sizes) > coord.MaxIntegrationFindingsBytes }
	if !over() {
		return findings, rec, nil
	}
	kept = slices.Clone(findings)
	cut, dropped := map[int]bool{}, map[int]bool{}
	// each runs step on the findings past the first that pick chooses, from
	// the tail, while the list is still past the bound.
	each := func(pick func(Finding) bool, step func(int)) {
		for i := len(kept) - 1; i > 0 && over(); i-- {
			if !dropped[i] && pick(kept[i]) {
				step(i)
			}
		}
	}
	wordless := func(counter *int) func(int) {
		return func(i int) {
			f := &kept[i]
			if f.Title == "" && f.Detail == "" && f.Remedy == "" && f.ActionURL == "" {
				return
			}
			f.Title, f.Detail, f.Remedy, f.ActionURL = "", "", "", ""
			sizes[i], cut[i] = findingSize(*f), true
			*counter++
		}
	}
	drop := func(i int) {
		dropped[i], cut[i], sizes[i] = true, true, -1
		rec.Omitted++
		if mayNameSeat(kept[i]) {
			rec.NamesOmitted++
		}
	}
	own := func(f Finding) bool { return f.Seat }
	unowed := func(f Finding) bool { return !personOwed(f) }
	every := func(Finding) bool { return true }
	each(own, wordless(&rec.SeatOmitted))
	each(unowed, wordless(&rec.WordsOmitted))
	each(unowed, drop)
	each(every, wordless(&rec.WordsOmitted))
	cutSubjects(kept, dropped, sizes, over, cut, &rec)
	each(every, drop)
	out := make([]Finding, 0, len(kept))
	for i, f := range kept {
		if !dropped[i] {
			out = append(out, f)
		}
	}
	return out, rec, slices.Sorted(maps.Keys(cut))
}

// cutSubjects cuts the subject lists of the findings past the first that are
// still kept to a common length, halved while the list is past its bound, and
// only then the first finding's, counting on each what it lost and on rec each
// list whose names may be a seat's ([mayNameSeat]).
func cutSubjects(kept []Finding, dropped map[int]bool, sizes []int, over func() bool, cut map[int]bool,
	rec *FindingsRecord) {

	listed := map[int]bool{}
	for _, first := range []bool{false, true} {
		longest := 0
		for i, f := range kept {
			if !dropped[i] && (i == 0) == first {
				longest = max(longest, len(f.Subjects))
			}
		}
		for limit := longest; over() && limit > 0; {
			limit /= 2
			for i := range kept {
				f := &kept[i]
				if dropped[i] || (i == 0) != first || len(f.Subjects) <= limit {
					continue
				}
				if !listed[i] && mayNameSeat(*f) {
					rec.NamesOmitted++
				}
				listed[i] = true
				f.SubjectsOmitted += len(f.Subjects) - limit
				f.Subjects = f.Subjects[:limit:limit]
				sizes[i], cut[i] = findingSize(*f), true
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
// findingSizes is each finding's length as its record stores it.
func findingSizes(findings []Finding) []int {
	sizes := make([]int, len(findings))
	for i, f := range findings {
		sizes[i] = findingSize(f)
	}
	return sizes
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

// listBytes is the length of a list of findings of sizes as JSON writes it:
// the brackets, each finding and a comma between each two, a negative size
// being a finding dropped.
func listBytes(sizes []int) int {
	total, n := 2, 0
	for _, size := range sizes {
		if size >= 0 {
			total += size
			n++
		}
	}
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
