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

// The charter's bounds, in CHARACTERS (Unicode code points, as
// [utf8.RuneCountInString] counts them).
//
// CHARACTERS RATHER THAN BYTES, unlike the tracker's caps, because the person
// these bounds speak to is writing prose on a page that counts what they type:
// the editor shows "120 of 1024 characters", and a byte bound would refuse a
// French or Japanese mission the counter shows as inside it. The prompt budget
// below holds either way, since a code point is at most four bytes.
//
// EVERY CHARACTER OF THE CHARTER IS IN EVERY SEAT'S SYSTEM PROMPT, re-sent on
// every round of every turn the company runs (`internal/agent/prompts` inlines
// it whole, deliberately, so a seat never needs a knowledge round trip to know
// what the company is for). So the bound is a prompt budget rather than a
// storage one: at these figures the whole charter is at most 28 672
// characters, about seven thousand tokens of Latin text, which is the most a
// company can make every turn carry before its own work. The figures sit well
// above what a real charter is: the shipped Nimbus example's vision is under
// 900 characters and its longest policy under 300, and a written charter of
// eight long policies (the dashboard's FairSplit fixture) has none over 300.
//
// The dashboard holds the same figures (`dashboard/src/contract/config.ts`),
// held to these by `TestTheDashboardBoundsTheCharterAtTheEnginesCaps`, so a
// draft past one is marked before anybody presses Save.
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
// breaks IN WHAT IT CHANGES. Prior is the stored revision the write replaces.
//
// # Nil refuses nothing
//
// With no stored version there is nothing the write changes a part FROM, and
// the writes that have none are the ones that must never be refused for an
// old file: a node booting an empty store from its company.yaml, a restore
// and a migration, each importing a document that ran yesterday under a build
// without these rules. Refused there, an upgrade would leave a node unable to
// boot. Such a document is admitted, and its faults are warnings
// ([Company.CharterWarnings]) like any other untouched fault.
//
// # Judged on the change, never on the document
//
// The charter rules arrived after companies were written, and a company
// whose stored mission runs long, or whose policies number thirty, must still
// be able to raise a budget or add a seat. Judged on the whole document, as
// [Company.Validate] judges its own rules, every write would carry that one
// old fault and be refused for it: the company would be locked out of every
// setting by a sentence nobody was editing. So a part is judged only when the
// write changes it:
//
//   - the mission and the vision, when the write changes them;
//   - a policy, when its text (trimmed) is not one of the prior's: a new or
//     an edited policy;
//   - the number of policies, when it is past the bound AND the write adds to
//     it, so removing a policy from a list already over the bound is admitted;
//   - a repeated policy, when the write makes a text appear more times than
//     it did.
//
// A fault the write did not touch is reported by [Company.Warnings] instead,
// on every write and every apply, which is how the company finds out about it.
//
// THE MISSION AND EACH POLICY ARE ONE LINE. A seat reads the mission inline
// after its label and each policy as one item of a list, so a line break
// cannot mean in the prompt what it shows on the page; the prompt folds one
// that a stored revision carries, and quotes a vision line by line, so no line
// stands alone either way (internal/agent/prompts). The vision is the one
// field written in paragraphs.
//
// A blank policy is refused rather than skipped, because it is a line of the
// prompt that says nothing, and a repeated one because the second copy is
// always a mistake: the order of the list is the order every seat reads it in,
// and nothing about a rule changes by saying it twice. Every rule judges the
// value as it is stored and read, TRIMMED: a policy of spaces is blank, two
// that differ only by the space around them are one, and a value at its bound
// with space around it is at its bound.
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
