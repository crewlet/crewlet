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
package notice

import (
	"cmp"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ClassAlarm is the class of an alarm that no finding carries; its subject is
// the alarm, named by its kind.
const ClassAlarm = "alarm"

// ClassConversationHeld is a conversation the engine holds because its context
// did not fit (ADR-0117); its subject is the seat, and Name the conversation.
const ClassConversationHeld = "conversation.held"

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
	// Alarm is the alarm kind that raised a finding's notice, so a reader
	// without the finding can still say what stands; empty otherwise.
	Alarm string `json:"alarm,omitempty"`
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
		out = append(out, Notice{Class: ClassConversationHeld, Since: h.At,
			Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: handle, Name: h.Conversation}})
	}
	return Merge(out)
}

// Read is every notice the advisor's findings, the fleet's alarms and the
// engine's held conversations raise at now.
func Read(findings []advisor.Finding, fleet []NodeAlarms, held []Notice, now time.Time) []Notice {
	return Merge(FromFindings(findings), FromAlarms(fleet, now), held)
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
