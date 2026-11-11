// Package delegated is delegated decisions: what a request that waits on a person is FOR, and how
// an agent seat a company names decides it in that person's place, within
// rules the engine enforces (ADR-0125).
//
// # A typed act, never prose
//
// A held call and an ask carry their act ([Kind]), set by the engine from the
// structured data that made the request: the held ssh_operate call, or a
// decision a tool composed for one exact call. Nothing reads an act out of a
// model's words, and an act the engine did not set is [KindOther], which is
// never delegated.
//
// # A delegate decides, the engine bounds
//
// A rule of the company document's `delegated_decisions` block names the kinds it
// delegates, one delegate seat (or two for a panel), and the people an
// escalation is put to. A delegate answers allow, escalate or deny with a
// confidence; [Combine] is what a request's answers come to, and every limit
// that turns an allow into an escalation is the engine's, checked before the
// verdict acts: a request the delegate made, asked for, handed on or leads; a
// cap per kind and delegate per hour ([DecisionsPerHour]); a request that
// touches access, budgets or spend, data deletion, secrets or the company
// document, or one on an item labelled security, privacy or legal; and each
// kind's own conditions. A request no delegate answered in [Deadline], and
// every request once delegated decisions are turned off, goes to people.
//
// # What a person still sees
//
// Escalations, with the delegate's findings first; a daily digest of what was
// decided for them; a spot-check of one decision in N after the fact
// ([Sampled]); and every verdict in the access log.
package delegated

import (
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"strings"
	"time"
)

// Kind is what a request is for: its act.
type Kind string

// The acts this build knows.
const (
	// KindSSHOperate is an agent's command on a server as its operate user.
	KindSSHOperate Kind = "ssh_operate"
	// KindOther is every request whose act the engine did not set: never
	// delegated.
	KindOther Kind = "other"
)

// Kinds is every act, the delegable first.
func Kinds() []Kind { return []Kind{KindSSHOperate, KindOther} }

// Valid reports whether k is an act this build knows.
func (k Kind) Valid() bool { return slices.Contains(Kinds(), k) }

// Delegable reports whether a rule may delegate k: every act but other.
func (k Kind) Delegable() bool { return k.Valid() && k != KindOther }

// DelegableKinds is every act a rule may name.
func DelegableKinds() []Kind {
	var out []Kind
	for _, k := range Kinds() {
		if k.Delegable() {
			out = append(out, k)
		}
	}
	return out
}

// Verdict is a delegate's answer, or what a request's answers came to.
type Verdict string

// The three verdicts.
const (
	Allow    Verdict = "allow"
	Escalate Verdict = "escalate"
	Deny     Verdict = "deny"
)

// Verdicts is every verdict, in the order a delegate is offered them.
func Verdicts() []Verdict { return []Verdict{Allow, Escalate, Deny} }

// Valid reports whether v is a verdict.
func (v Verdict) Valid() bool { return slices.Contains(Verdicts(), v) }

// Confidence is how sure a delegate is of its verdict.
type Confidence string

// The two confidences.
const (
	High Confidence = "high"
	Low  Confidence = "low"
)

// Valid reports whether c is a confidence.
func (c Confidence) Valid() bool { return c == High || c == Low }

// The engine's closed words for why a request reached people over its
// delegates, or how its answers combined; each kind adds its own (an SSH
// always-escalate class's name).
const (
	OverruledSelf            = "self"
	OverruledLeads           = "leads"
	OverruledRateLimit       = "rate_limit"
	OverruledServerRateLimit = "server_rate_limit"
	OverruledLowConfidence   = "low_confidence"
	OverruledDisagreement    = "disagreement"
	OverruledDeadline        = "deadline"
	OverruledOff             = "delegation_off"
	OverruledLabel           = "sensitive_label"
)

// Given is one delegate's answer.
type Given struct {
	By         string
	Verdict    Verdict
	Confidence Confidence
}

// Outcome is what a request's answers come to so far.
type Outcome struct {
	// Decided is false while a panel waits on its other delegate.
	Decided bool
	Verdict Verdict
	// Overruled is why the outcome is not the answers' own verdict.
	Overruled []string
}

// Combine is what the answers given so far by delegates come to: an
// escalation or a low confidence from any delegate escalates at once; once
// every delegate answered, all allow allows and all deny denies, and anything
// else is a disagreement, which escalates.
func Combine(delegates []string, given []Given) Outcome {
	for _, g := range given {
		switch {
		case g.Confidence != High:
			return Outcome{Decided: true, Verdict: Escalate, Overruled: []string{OverruledLowConfidence}}
		case g.Verdict == Escalate:
			return Outcome{Decided: true, Verdict: Escalate}
		}
	}
	for _, d := range delegates {
		if !slices.ContainsFunc(given, func(g Given) bool { return g.By == d }) {
			return Outcome{}
		}
	}
	if len(given) == 0 {
		return Outcome{}
	}
	first := given[0].Verdict
	for _, g := range given[1:] {
		if g.Verdict != first {
			return Outcome{Decided: true, Verdict: Escalate, Overruled: []string{OverruledDisagreement}}
		}
	}
	return Outcome{Decided: true, Verdict: first}
}

// The categories every kind escalates, whatever its delegate answers; each
// kind says which of its requests touch one.
const (
	CategoryAccess   = "access_grants"
	CategoryBudget   = "budget_or_spend"
	CategoryDeletion = "data_deletion"
	CategorySecrets  = "secrets"
	CategoryCompany  = "company_document"
)

// Categories is every always-escalate category, in the order a reader is told.
func Categories() []string {
	return []string{CategoryAccess, CategoryBudget, CategoryDeletion, CategorySecrets, CategoryCompany}
}

// SensitiveLabels are the work item labels whose requests always escalate.
func SensitiveLabels() []string { return []string{"security", "privacy", "legal"} }

// SensitiveLabel is the first of labels that is sensitive, ignoring case, and
// whether there is one.
func SensitiveLabel(labels []string) (string, bool) {
	for _, l := range labels {
		if slices.Contains(SensitiveLabels(), strings.ToLower(strings.TrimSpace(l))) {
			return l, true
		}
	}
	return "", false
}

// DecisionsPerHour bounds one delegate's allows of one kind in one clock hour
// (UTC), past which its requests go to people.
//
// THIRTY: one allow every two minutes, all hour. Requests past that are coming
// faster than any reviewer reads them, which is a loop or an incident.
const DecisionsPerHour = 30

// Deadline is how long a request waits on its delegates before it goes to
// people: a delegate's turn starts within a minute on an idle seat, and
// fifteen covers a launch retry and a queue of several requests.
const Deadline = 15 * time.Minute

// DefaultSampleOneIn is the spot-check rate a rule naming none takes: one
// decision in ten shown to people after the fact, a handful a week for a busy
// delegate.
const DefaultSampleOneIn = 10

// Sampled reports whether the decision of request is one of the one in oneIn
// a person spot-checks, by the request's digest: the same request is always
// the same answer, and no counter has to agree across the fleet.
func Sampled(request string, oneIn int) bool {
	if oneIn <= 1 {
		return oneIn == 1
	}
	sum := sha256.Sum256([]byte("crewlet.delegated.sample/" + request))
	return binary.BigEndian.Uint64(sum[:8])%uint64(oneIn) == 0
}
