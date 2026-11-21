package config

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// The CHARTER is the part of the company document every agent is handed word
// for word: its mission, its vision and its standing policies. It is one
// value here because two things treat the three fields as one (the rules
// below, and the `/config/charter` route a person edits them through), and a
// rule or a route that took them one at a time would let the three drift.

// The charter's bounds, in CHARACTERS (code points, as
// [utf8.RuneCountInString] counts them), unlike the tracker's byte caps: the
// editor shows "120 of 1024 characters", and a byte bound would refuse a
// French or Japanese mission the counter shows as inside it.
//
// EVERY CHARACTER IS IN EVERY SEAT'S SYSTEM PROMPT, re-sent each round
// (`internal/agent/prompts` inlines it whole), so the bound is a prompt
// budget: at most 28 672 characters, about seven thousand tokens, well above
// a real charter (Nimbus's vision is under 900 characters).
//
// The dashboard holds the same figures (`dashboard/src/contract/config.ts`),
// held by `TestTheDashboardBoundsTheCharterAtTheEnginesCaps`.
const (
	// MaxMissionChars bounds the mission: one or two sentences on what the
	// company is for.
	MaxMissionChars = 1024
	// MaxVisionChars bounds the vision, the longer statement of where the
	// company is going.
	MaxVisionChars = 2048
	// MaxPolicies bounds how many standing policies a company keeps.
	MaxPolicies = 25
	// MaxPolicyChars bounds one policy: a rule and the sentence or two that
	// explain it.
	MaxPolicyChars = 1024
)

// Charter is the company's mission, vision and standing policies.
//
// Policies is never nil on a value this package hands out, so its JSON is
// always a list: a reader telling "no policies" from "a field it did not
// send" is a reader that has to know which spelling means which.
type Charter struct {
	Mission  string   `json:"mission"`
	Vision   string   `json:"vision"`
	Policies []string `json:"policies"`
}

// CharterSettings are the company document's top-level keys the charter is,
// as [OrgDiff.Settings] names a changed one: what a charter write may change
// and nothing else (internal/api/configapi's admission).
var CharterSettings = []string{"mission", "vision", "policies"}

// Charter is c's charter, a copy: changing it changes nothing in c.
func (c *Company) Charter() Charter {
	return Charter{
		Mission:  c.Mission,
		Vision:   c.Vision,
		Policies: append([]string{}, c.Policies...),
	}
}

// Trimmed is ch with the space a person's editor leaves around each value
// removed, which is the one normalisation a charter write makes. Nothing
// else is rewritten: a value is stored as it was typed.
func (ch Charter) Trimmed() Charter {
	out := Charter{
		Mission:  strings.TrimSpace(ch.Mission),
		Vision:   strings.TrimSpace(ch.Vision),
		Policies: make([]string, len(ch.Policies)),
	}
	for i, p := range ch.Policies {
		out.Policies[i] = strings.TrimSpace(p)
	}
	return out
}

// Equal reports whether two charters say the same thing in the same order.
func (ch Charter) Equal(other Charter) bool {
	return ch.Mission == other.Mission && ch.Vision == other.Vision &&
		slices.Equal(ch.Policies, other.Policies)
}

// ValidateCharterChange reports every charter rule the write from prior to c
// breaks IN WHAT IT CHANGES; prior is the stored revision it replaces.
//
// Nil refuses nothing: a boot from company.yaml, a restore or a migration
// imports a document that ran under a build without these rules, and refusing
// it would stop the node booting. Judging the whole document would refuse
// every write for one old fault, so a part is judged only when the write
// changes it (mission, vision, a policy with new trimmed text, a count past
// the bound that the write raises, a text made to repeat more); the rest are
// warnings ([Company.CharterWarnings]). The mission and each policy are ONE
// LINE, a blank or repeated policy is refused, and every rule judges the
// TRIMMED value (a seat reads each inline: internal/agent/prompts).
func (c *Company) ValidateCharterChange(prior *Company) error {
	if prior == nil {
		return nil
	}
	was := prior.Charter().Trimmed()
	return c.Charter().Trimmed().faults(&was)
}

// CharterWarnings is every charter rule c breaks, whatever wrote it and
// whether or not a write is refused for it, as warnings located like the
// problems a write that changes the part is refused with. [Company.Warnings]
// carries them, so every write's answer, every apply's log, `crewlet
// validate` and an import name them.
func (c *Company) CharterWarnings() []Warning {
	return warningsOf(c.Charter().Trimmed().faults(nil))
}

// faults is every rule ch breaks in what differs from was; a nil was judges
// all of it. ch and was are trimmed.
func (ch Charter) faults(was *Charter) error {
	var p problems
	if was == nil || ch.Mission != was.Mission {
		if n := utf8.RuneCountInString(ch.Mission); n > MaxMissionChars {
			p.add(field("mission"), ErrOutOfRange,
				"the mission is %d characters; it can be at most %d. Every seat reads "+
					"it on every turn, so keep it to a sentence or two", n, MaxMissionChars)
		}
		if lineBreak(ch.Mission) {
			p.add(field("mission"), ErrShape,
				"the mission is one line: every seat reads it inline, so write it "+
					"without a line break")
		}
	}
	if was == nil || ch.Vision != was.Vision {
		if n := utf8.RuneCountInString(ch.Vision); n > MaxVisionChars {
			p.add(field("vision"), ErrOutOfRange,
				"the vision is %d characters; it can be at most %d. Every seat reads "+
					"it on every turn, so keep it to a short paragraph or two", n, MaxVisionChars)
		}
	}
	if n := len(ch.Policies); n > MaxPolicies && (was == nil || n > len(was.Policies)) {
		p.add(field("policies"), ErrOutOfRange,
			"the company has %d policies; it can keep at most %d. Every seat "+
				"reads all of them on every turn, so merge the ones that say "+
				"the same thing", n, MaxPolicies)
	}
	// How many times each text stood in the prior list, which a repeat is
	// judged against and an unchanged policy is recognised by.
	before := map[string]int{}
	if was != nil {
		for _, text := range was.Policies {
			before[text]++
		}
	}
	first := map[string]int{}
	seen := map[string]int{}
	for i, text := range ch.Policies {
		place := idx(field("policies"), i)
		seen[text]++
		if before[text] > 0 && seen[text] <= before[text] {
			// A policy the prior already held, as many times: unchanged.
			if _, ok := first[text]; !ok {
				first[text] = i
			}
			continue
		}
		if text == "" {
			p.add(place, ErrMissing, "policy %d is empty; write the rule, or remove it", i+1)
			continue
		}
		if n := utf8.RuneCountInString(text); n > MaxPolicyChars {
			p.add(place, ErrOutOfRange,
				"policy %d is %d characters; a policy can be at most %d. Split it "+
					"into two rules, or say it more briefly", i+1, n, MaxPolicyChars)
		}
		if lineBreak(text) {
			p.add(place, ErrShape,
				"policy %d is more than one line; every seat reads each policy as "+
					"one item of a list, so write it as one paragraph, or split it "+
					"into two policies", i+1)
		}
		if earlier, ok := first[text]; ok {
			p.add(place, ErrConflict,
				"policy %d repeats policy %d word for word; remove one of them",
				i+1, earlier+1)
			continue
		}
		first[text] = i
	}
	return p.err()
}

// lineBreak reports whether s breaks a line: a line feed, a carriage return,
// or one of the separators Unicode defines as ending a line. Callers hand it
// a trimmed value, so a trailing newline an editor leaves is no break.
func lineBreak(s string) bool {
	return strings.ContainsAny(s, "\n\r\v\f\u0085\u2028\u2029")
}
