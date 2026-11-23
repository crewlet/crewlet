package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

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
// cuts to fit rather than refuse: losing the record would lose the phase.

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

	// Omitted is how many findings the cut dropped from the list's tail, and
	// SeatOmitted how many of an agent's own ([Finding.Seat]) it folded away,
	// which each agent's access record still carries ([FitFindings]).
	Omitted     int `json:"omitted,omitempty"`
	SeatOmitted int `json:"seat_omitted,omitempty"`
}

// Named reports whether the status names a findings record to read.
func (r FindingsRecord) Named() bool { return r.SHA256 != "" }

// refold is a fold's counts beside the name of the record its state was read
// with, which [CoordStore.RecordPass] replaces: kept so a fold that found
// nothing removes the record its status named, and writes nothing more where
// it named none.
func (r FindingsRecord) refold(counts FindingsRecord) FindingsRecord {
	counts.Bytes, counts.SHA256, counts.Count = r.Bytes, r.SHA256, r.Count
	return counts
}

// MaxActionURLBytes is the longest link, as encoded, a report or a finding
// keeps: 2 KiB, past the 2,000 characters browsers and vendors reliably
// carry, so a link it drops was not one a person could follow anyway. The
// rest of a record's text is cut; a cut link would lead somewhere else.
const MaxActionURLBytes = 2 << 10

// FitFindings is findings, already [Promote]d and [bound], cut to what one
// surface's findings record keeps ([coord.MaxIntegrationFindingsBytes]) as
// encoded, and the counts of what it left out. Cut in this order until it
// fits: an agent's own findings (the first excepted) are folded into a
// count; every subject list is cut to a common length, halved until the list
// fits, each counting what it lost; findings are dropped from the tail. The
// first, the one the report names, is never dropped, so the report and its
// "(and N more)", classified before the cut, stay true.
func FitFindings(findings []Finding) ([]Finding, FindingsRecord) {
	kept, rec, _ := fitFindings(findings)
	return kept, rec
}

// fitFindings is [FitFindings], and every finding it left out or cut, whole,
// for [LogWhole].
func fitFindings(findings []Finding) (kept []Finding, rec FindingsRecord, unkept []Finding) {
	if listBytes(findingSizes(findings)) <= coord.MaxIntegrationFindingsBytes {
		return findings, rec, nil
	}
	kept = make([]Finding, 0, len(findings))
	for i, f := range findings {
		if i > 0 && f.Seat {
			rec.SeatOmitted++
			unkept = append(unkept, f)
			continue
		}
		kept = append(kept, f)
	}
	whole, sizes := kept, findingSizes(kept)
	for limit := longestSubjects(whole); listBytes(sizes) > coord.MaxIntegrationFindingsBytes && limit > 0; {
		limit /= 2
		kept = withSubjectsCut(whole, limit)
		for i, f := range whole {
			// Only a list this step cut changed size.
			if len(f.Subjects) > limit {
				sizes[i] = findingSize(kept[i])
			}
		}
	}
	for listBytes(sizes) > coord.MaxIntegrationFindingsBytes && len(sizes) > 1 {
		sizes = sizes[:len(sizes)-1]
		rec.Omitted++
	}
	for i, f := range whole {
		if i >= len(sizes) || kept[i].SubjectsOmitted != f.SubjectsOmitted {
			unkept = append(unkept, f)
		}
	}
	return kept[:len(sizes)], rec, unkept
}

// counted is findings' [FindingsRecord.PersonOwes] and
// [FindingsRecord.Naming].
func counted(findings []Finding) FindingsRecord {
	var rec FindingsRecord
	for _, f := range findings {
		if _, actor := f.Kind.Verdict(); actor.WaitsOnAPerson() {
			rec.PersonOwes++
		}
		if f.Subject != "" || len(f.Subjects) > 0 {
			rec.Naming++
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
// the brackets, each finding and a comma between each two.
func listBytes(sizes []int) int {
	total := 2 + max(len(sizes)-1, 0)
	for _, n := range sizes {
		total += n
	}
	return total
}

// longestSubjects is the length of the longest subject list among findings.
func longestSubjects(findings []Finding) int {
	most := 0
	for _, f := range findings {
		most = max(most, len(f.Subjects))
	}
	return most
}

// withSubjectsCut is findings with every subject list cut to limit, a copy,
// each counting what it lost.
func withSubjectsCut(findings []Finding, limit int) []Finding {
	out := make([]Finding, len(findings))
	for i, f := range findings {
		if len(f.Subjects) > limit {
			f.SubjectsOmitted += len(f.Subjects) - limit
			f.Subjects = f.Subjects[:limit:limit]
		}
		out[i] = f
	}
	return out
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
