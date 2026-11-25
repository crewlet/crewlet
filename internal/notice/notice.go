// Package notice is how a condition the engine detects reaches a company's
// administrators: a platform notice, keyed by its class and subject, open
// exactly while the condition holds, with nothing to answer.
//
// # Read, never written
//
// A notice is not a record (ADR-0116). Each producer reads it off facts the
// fleet already keeps (every node's heartbeat alarm marks, a finding the
// advisor raises), so a notice cannot outlive its condition, a restart cannot
// raise it again, two nodes reporting one condition give one notice, and
// whoever administers the company when it is read is who it reaches.
//
// # One notice per condition
//
// A condition the advisor also finds is one notice under the finding's class
// and subject, so the dashboard attaches the finding's fix to it: the backup
// alarm is the advisor's no-verified-backup finding, never a second notice
// beside it.
//
// # What the growth products meet
//
// Two notices are about the vendors the growth products call, each worded in
// Title and Person: an API version this build calls that its vendor retires
// within [RetiringWithin] (read off the build's own catalogs) or says is
// deprecated, and a vendor refusing the tokens a node signs because the node's
// clock is off. The vendor's word and the clock are met by one node's process,
// so each node publishes them on its status record (internal/growth's
// Signals) and every node reads the fleet's: a skew closes once a later signed
// request is accepted, and a deprecation a day after the vendor last said it.
//
// # The redirect a person registered
//
// A client a person registered at its vendor (a Google consent product's, an
// MCP server's own) lists the redirect URIs a sign-in may come back to, and
// only a person changes that list. Each grant records the redirect it came
// back through, so [FromRedirects] reads one notice off the clients whose last
// grant came back elsewhere than the answering node's callback now: an address
// moved at a restart. It names each client and closes for it once it signs in
// through the new address; a grant recorded without a redirect proves nothing.
package notice

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ClassAlarm is the class of an alarm that no finding carries; its subject is
// the alarm, named by its kind.
const ClassAlarm = "alarm"

// ClassConversationHeld is a conversation the engine holds because it failed the
// same way twice (ADR-0117); its subject is the seat, Name the conversation,
// and Cause how it failed: [CauseContextExhausted], [CauseOutOfMemory] or
// [CauseRequestRefused].
const ClassConversationHeld = "conversation.held"

// The causes of a held conversation ([Notice.Cause]), as its hold names them.
const (
	CauseContextExhausted = "context_exhausted"
	CauseOutOfMemory      = "out_of_memory"
	CauseRequestRefused   = "request_refused"
)

// SubjectAlarm is what an alarm's notice is about: Name is the alarm's kind.
const SubjectAlarm advisor.SubjectKind = "alarm"

// AlarmAfter is how long an alarm stands before it reaches the administrators:
// the deferral grace, past which the engine itself stops waiting for a
// condition to clear on its own (ADR-0015: the threshold is borrowed).
const AlarmAfter = statelog.DeferralGrace

// Notice is one condition an administrator should know about.
type Notice struct {
	// Class is the class of the finding whose fix ends it, or [ClassAlarm].
	Class   string          `json:"class"`
	Subject advisor.Subject `json:"subject"`
	// Since is when the condition began, zero where its producer cannot say.
	Since time.Time `json:"since,omitzero"`
	// Nodes are the nodes an alarm stands on, sorted; empty for a finding.
	Nodes []string `json:"nodes,omitempty"`
	// Cause is how a held conversation failed; empty for every other notice.
	Cause string `json:"cause,omitempty"`
	// Alarm is the alarm kind that raised a finding's notice, so a reader
	// without the finding can still say what stands; empty otherwise.
	Alarm string `json:"alarm,omitempty"`
	// Title and Person are the raising alarm's words for a company owner
	// ([statelog.Alarm.Title]); empty for a notice no alarm raised, and for
	// a kind this build does not raise.
	Title  string `json:"title,omitempty"`
	Person string `json:"person,omitempty"`
	// Finding is the Advisor's finding whose fix ends the notice, nil where
	// none stands: an alarm no finding carries, or a held conversation.
	Finding *FindingRef `json:"finding"`
	// Redirect is an [ClassOAuthRedirectMoved] notice's new URI and stale
	// clients; nil on every other notice.
	Redirect *RedirectMoved `json:"redirect,omitempty"`
}

// FindingRef names the finding a notice is ended by, as a row draws it.
type FindingRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Fix is the finding's fix in one sentence.
	Fix string `json:"fix"`
}

// Listed is a finding the Advisor keeps, with its id.
type Listed struct {
	ID      string
	Finding advisor.Finding
}

// WithFindings is notices with each one's finding named, where a listed
// finding has its class and subject.
func WithFindings(notices []Notice, listed []Listed) []Notice {
	for i := range notices {
		for _, l := range listed {
			if string(l.Finding.Class) == notices[i].Class && l.Finding.Subject == notices[i].Subject {
				notices[i].Finding = &FindingRef{ID: l.ID, Title: l.Finding.Title, Fix: l.Finding.Fix.Sentence}
				break
			}
		}
	}
	return notices
}

// key is what makes two notices one.
type key struct {
	class   string
	subject advisor.Subject
}

func (n Notice) key() key { return key{n.Class, n.Subject} }

// NodeAlarms is one live node's alarm marks, as its last heartbeat carried them.
type NodeAlarms struct {
	Node  string
	Marks []coord.AlarmMark
}

// alarmFindings are the alarms the advisor reads as a finding, each under the
// finding's class and subject.
var alarmFindings = map[string]Notice{
	string(statelog.KindBackupAge): {Class: string(advisor.ClassNoVerifiedBackup),
		Subject: advisor.Subject{Kind: advisor.SubjectCompany}},
}

// FromAlarms is one notice per alarm kind whose earliest raise on any node is
// at least [AlarmAfter] before now, naming every node it stands on. A mark that
// says no raise instant has no known age, so alone it is not past the threshold.
func FromAlarms(nodes []NodeAlarms, now time.Time) []Notice {
	var raised []Notice
	for _, n := range nodes {
		for _, m := range n.Marks {
			notice, ok := alarmFindings[m.Kind]
			if ok {
				notice.Alarm = m.Kind
			} else {
				notice = Notice{Class: ClassAlarm, Subject: advisor.Subject{Kind: SubjectAlarm, Name: m.Kind}}
			}
			notice.Since, notice.Nodes = m.Since, []string{n.Node}
			notice.Title, notice.Person, _ = statelog.PersonWords(statelog.Kind(m.Kind))
			raised = append(raised, notice)
		}
	}
	return slices.DeleteFunc(Merge(raised), func(n Notice) bool {
		return n.Since.IsZero() || now.Sub(n.Since) < AlarmAfter
	})
}

// FromFindings is one notice per finding that blocks the company and is the
// owner's to fix. A finding an alarm raises is the alarm's notice, which waits
// for [AlarmAfter], so it is left to [FromAlarms].
func FromFindings(findings []advisor.Finding) []Notice {
	alarmed := map[key]bool{}
	for _, n := range alarmFindings {
		alarmed[n.key()] = true
	}
	var out []Notice
	for _, f := range findings {
		n := Notice{Class: string(f.Class), Subject: f.Subject}
		if f.Severity != advisor.SeverityBlocks || f.Actor != advisor.ActorOwner || alarmed[n.key()] {
			continue
		}
		out = append(out, n)
	}
	return Merge(out)
}

// FromConversationHolds is one notice per held conversation of a seat handleOf
// names; a hold of a seat the company no longer has is left to its removal.
func FromConversationHolds(holds []coord.ConversationHold, handleOf func(uuid.UUID) (string, bool)) []Notice {
	var out []Notice
	for _, h := range holds {
		handle, known := handleOf(h.Seat)
		if !known {
			continue
		}
		out = append(out, Notice{Class: ClassConversationHeld, Since: h.At, Cause: h.Kind,
			Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: handle, Name: h.Conversation}})
	}
	return Merge(out)
}

// ClassAPIVersionRetiring is a vendor API version this build calls that its
// vendor retires within [RetiringWithin], or says is deprecated; its subject is
// the integration, Name its kind and the version ("metaads/v23.0").
const ClassAPIVersionRetiring = "api_version_retiring"

// ClassClockSkew is a vendor refusing the tokens a node signs because the
// node's clock is more than [growth.ClockSkewTolerance] from the vendor's; its
// subject is the integration, Name its kind, and Nodes the nodes it stands on.
const ClassClockSkew = "clock_skew"

// RetiringWithin is how far ahead of a vendor's retirement of an API version
// its notice opens: 90 days, the span vendors announce a retirement over
// (Meta's version deprecations, Apple's API versions) and room for an upgrade.
const RetiringWithin = 90 * 24 * time.Hour

// NodeGrowth is one live node's growth signals, as its status record carries
// them.
type NodeGrowth struct {
	Node    string
	Signals coord.GrowthSignals
}

// FromCatalogs is one notice per API version this build calls whose vendor
// retires it within [RetiringWithin] of now, or a node heard the vendor call
// deprecated, keyed by the catalog's kind and version.
func FromCatalogs(catalogs []growth.Catalog, nodes []NodeGrowth, now time.Time) []Notice {
	api := map[string]string{}
	var out []Notice
	for _, c := range catalogs {
		api[c.Kind] = c.API
		if c.Retires.IsZero() || c.Retires.Sub(now) > RetiringWithin {
			continue
		}
		out = append(out, retiring(c.Kind, c.API, c.Version, c.Retires, c.Retires.Add(-RetiringWithin), "", now))
	}
	for _, n := range nodes {
		for _, d := range n.Signals.Deprecations {
			name := cmp.Or(api[d.Product], d.Product)
			out = append(out, retiring(d.Product, name, d.Version, d.Retires, d.Since, n.Node, now))
		}
	}
	return Merge(out)
}

// retiring is one retirement's notice, in the words a company owner reads.
func retiring(kind, api, version string, retires, since time.Time, node string, now time.Time) Notice {
	n := Notice{Class: ClassAPIVersionRetiring, Since: since,
		Subject: advisor.Subject{Kind: advisor.SubjectIntegration, Name: kind + "/" + version},
		Title:   fmt.Sprintf("%s %s is retiring", api, version)}
	if node != "" {
		n.Nodes = []string{node}
	}
	switch {
	case retires.IsZero():
		n.Person = fmt.Sprintf("This Crewlet build calls %s %s, which the vendor has deprecated. Update "+
			"Crewlet before the vendor retires it.", api, version)
	case !retires.After(now):
		n.Person = fmt.Sprintf("This Crewlet build calls %s %s, which the vendor retired on %s. Update "+
			"Crewlet now.", api, version, retires.UTC().Format("2 January 2006"))
	default:
		n.Person = fmt.Sprintf("This Crewlet build calls %s %s, which the vendor retires on %s. Update "+
			"Crewlet before then.", api, version, retires.UTC().Format("2 January 2006"))
	}
	return n
}

// FromClockSkews is one notice per product whose vendor refuses the tokens a
// node signs for the node's clock, naming every such node and worded for the
// one furthest off.
func FromClockSkews(nodes []NodeGrowth) []Notice {
	type worst struct {
		notice Notice
		skew   coord.GrowthClockSkew
	}
	by := map[string]*worst{}
	var order []string
	for _, n := range nodes {
		for _, c := range n.Signals.ClockSkews {
			w, seen := by[c.Product]
			if !seen {
				w = &worst{notice: Notice{Class: ClassClockSkew, Since: c.Since,
					Subject: advisor.Subject{Kind: advisor.SubjectIntegration, Name: c.Product}}, skew: c}
				by[c.Product] = w
				order = append(order, c.Product)
			}
			w.notice.Nodes = append(w.notice.Nodes, n.Node)
			if abs(c.OffsetSeconds) > abs(w.skew.OffsetSeconds) {
				w.skew = c
			}
			if !c.Since.IsZero() && (w.notice.Since.IsZero() || c.Since.Before(w.notice.Since)) {
				w.notice.Since = c.Since
			}
		}
	}
	out := make([]Notice, 0, len(order))
	for _, product := range order {
		w := by[product]
		vendor := cmp.Or(w.skew.Vendor, product)
		way := "ahead of"
		if w.skew.OffsetSeconds < 0 {
			way = "behind"
		}
		who, fix := "This node's clock is", "Fix the node's time synchronisation."
		if len(w.notice.Nodes) > 1 {
			who = fmt.Sprintf("The clocks of %d nodes are up to", len(w.notice.Nodes))
			fix = "Fix their time synchronisation."
		}
		w.notice.Title = fmt.Sprintf("%s refuses the tokens Crewlet signs", vendor)
		w.notice.Person = fmt.Sprintf("%s %s %s %s's, so %s refuses the tokens Crewlet signs. %s",
			who, statelog.Spoken(time.Duration(abs(w.skew.OffsetSeconds))*time.Second), way, vendor, vendor, fix)
		out = append(out, w.notice)
	}
	return Merge(out)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Read is every notice the advisor's findings, the fleet's alarms, the
// engine's held conversations, what the growth products meet ([FromCatalogs],
// [FromClockSkews]) and the OAuth clients a moved address left behind
// ([FromRedirects]) raise at now.
func Read(findings []advisor.Finding, fleet []NodeAlarms, held, vendors, redirects []Notice, now time.Time) []Notice {
	return Merge(FromFindings(findings), FromAlarms(fleet, now), held, vendors, redirects)
}

// Merge is lists as one notice per class and subject: the earliest instant any
// of them began, and every node any stands on. Ordered oldest first, then by
// class and subject, so two reads of one fleet list them alike.
func Merge(lists ...[]Notice) []Notice {
	at := map[key]int{}
	out := []Notice{}
	for _, list := range lists {
		for _, n := range list {
			i, seen := at[n.key()]
			if !seen {
				at[n.key()] = len(out)
				n.Nodes = slices.Clone(n.Nodes)
				out = append(out, n)
				continue
			}
			m := &out[i]
			if !n.Since.IsZero() && (m.Since.IsZero() || n.Since.Before(m.Since)) {
				m.Since = n.Since
			}
			m.Nodes = append(m.Nodes, n.Nodes...)
		}
	}
	for i := range out {
		slices.Sort(out[i].Nodes)
		out[i].Nodes = slices.Compact(out[i].Nodes)
	}
	slices.SortFunc(out, func(a, b Notice) int {
		return cmp.Or(compareSince(a.Since, b.Since), cmp.Compare(a.Class, b.Class),
			cmp.Compare(string(a.Subject.Kind), string(b.Subject.Kind)), cmp.Compare(a.Subject.Name, b.Subject.Name),
			cmp.Compare(a.Subject.Seat, b.Subject.Seat), cmp.Compare(a.Subject.Unit, b.Subject.Unit),
			cmp.Compare(a.Subject.Schedule, b.Subject.Schedule))
	})
	return out
}

// compareSince orders known instants oldest first, and an unknown one last.
func compareSince(a, b time.Time) int {
	switch {
	case a.IsZero() || b.IsZero():
		return cmp.Compare(boolInt(a.IsZero()), boolInt(b.IsZero()))
	default:
		return a.Compare(b)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
