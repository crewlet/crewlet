package advisor

import (
	"encoding/json"
	"strconv"
	"strings"
)

// A FINDING'S PREMISE (ADR-0166): the one fact an investigation's finding rests
// on, stated so the engine can ask it again: a read, its parameters, the path
// to a number in its answer, and how that number must compare. The hourly
// pass asks it again for every open finding that states one, and a premise
// that no longer holds resolves the finding, so a finding never outlives the
// fact it was about (an item that was in review and is done).

// Premise is a re-checkable fact.
type Premise struct {
	Read   string         `json:"read"`
	Params map[string]any `json:"params,omitempty"`
	Path   string         `json:"path"`
	Op     PremiseOp      `json:"op"`
	Value  float64        `json:"value"`
}

// PremiseOp is how a premise's number compares.
type PremiseOp string

// The comparisons.
const (
	PremiseEq PremiseOp = "eq"
	PremiseNe PremiseOp = "ne"
	PremiseGe PremiseOp = "ge"
	PremiseGt PremiseOp = "gt"
	PremiseLe PremiseOp = "le"
	PremiseLt PremiseOp = "lt"
)

// Holds reports whether n satisfies the premise's comparison, false for an
// operator this build does not know.
func (p Premise) Holds(n float64) bool {
	switch p.Op {
	case PremiseEq:
		return n == p.Value
	case PremiseNe:
		return n != p.Value
	case PremiseGe:
		return n >= p.Value
	case PremiseGt:
		return n > p.Value
	case PremiseLe:
		return n <= p.Value
	case PremiseLt:
		return n < p.Value
	}
	return false
}

// SubmittedPremise is a premise as an investigation submits it: the number it
// rests on, cited as a fact is (read_N.path), and how it must compare.
type SubmittedPremise struct {
	From  string    `json:"from"`
	Op    PremiseOp `json:"op"`
	Value float64   `json:"value"`
}

// ReadSource is the read an investigation's answer came from: what to ask to
// read it again.
type ReadSource struct {
	Read   string
	Params map[string]any
}

// ServeRead records one read's answer, as Serve does, and the read it came
// from, so a premise citing it can be asked again.
func (s *Served) ServeRead(name string, source ReadSource, answer any, records []Record) error {
	if err := s.Serve(name, answer, records); err != nil {
		return err
	}
	if s.sources == nil {
		s.sources = map[string]ReadSource{}
	}
	s.sources[name] = source
	return nil
}

// premiseOf is the premise a submission states, built from the read its
// citation names, and whether it holds now; false where it names no read the
// run made, or a number not at that path.
func premiseOf(p SubmittedPremise, served *Served) (Premise, bool) {
	name, path, ok := strings.Cut(p.From, ".")
	source, read := served.sources[name]
	if !ok || !read || path == "" {
		return Premise{}, false
	}
	out := Premise{Read: source.Read, Params: source.Params, Path: path, Op: p.Op, Value: p.Value}
	n, found := ReadPath(served.answers[name], path)
	if !found || !out.Holds(n) {
		return Premise{}, false
	}
	return out, true
}

// ReadPath is the number at path in a decoded JSON answer (open_work.blockers[0].blocks
// without its tool's name), false where there is none.
func ReadPath(v any, path string) (float64, bool) {
	for path != "" {
		var step string
		step, path, _ = strings.Cut(path, ".")
		name, index, indexed := strings.Cut(strings.TrimSuffix(step, "]"), "[")
		if name != "" {
			m, ok := v.(map[string]any)
			if !ok {
				return 0, false
			}
			if v, ok = m[name]; !ok {
				return 0, false
			}
		}
		if indexed {
			list, ok := v.([]any)
			i, err := strconv.Atoi(index)
			if !ok || err != nil || i < 0 || i >= len(list) {
				return 0, false
			}
			v = list[i]
		}
	}
	n, ok := v.(float64)
	return n, ok
}

// DecodeAnswer is an answer as ReadPath reads it: its JSON, decoded.
func DecodeAnswer(answer any) (any, error) {
	raw, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	var v any
	return v, json.Unmarshal(raw, &v)
}
