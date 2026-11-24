package advisor

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// EVERY FINDING LEADS WITH ITS NUMBER (ADR-0166). A class's title says what
// kind of thing is wrong and is the same on every finding of it; the headline
// says how much, on this subject, this week, from the facts the finding
// already states. A class that writes its own headline keeps it; every other
// class has one here. Only a document's own warnings, which measure nothing,
// lead with their title.

// stateHeadlines gives each finding of fs that states no headline its
// class's, naming its seat as c does.
func stateHeadlines(c *config.Company, fs []Finding) {
	for i := range fs {
		if fs[i].Headline != "" {
			continue
		}
		who := subjectSaid(fs[i])
		if s := fs[i].Subject; s.Seat != "" && c != nil && (s.Name == "" || s.Kind == SubjectGroup) {
			who = seatName(c, s.Seat)
		}
		fs[i].Headline = headlineOf(fs[i], who)
	}
}

// HeadlineOf is f's headline: its own, else its class's from its facts, else
// its title where its class measures nothing. A finding kept before its class
// wrote one reads this way too.
func HeadlineOf(f Finding) string {
	if f.Headline != "" {
		return f.Headline
	}
	return headlineOf(f, subjectSaid(f))
}

// subjectSaid is what a finding is about, as plainly as the finding names it.
func subjectSaid(f Finding) string {
	s := f.Subject
	switch {
	case s.Kind == SubjectGroup && s.Seat != "":
		return s.Seat
	case s.Name != "":
		return s.Name
	case s.Seat != "":
		return s.Seat
	case s.Schedule != "":
		return s.Schedule
	}
	return "The company"
}

// headlineOf is f's class's headline, who naming its subject.
func headlineOf(f Finding, who string) string {
	say, ok := headlines()[f.Class]
	if !ok {
		return f.Title
	}
	n := func(key string) int64 {
		for _, fact := range f.Evidence {
			if fact.Key == key {
				return fact.Value
			}
		}
		return 0
	}
	return say(f, who, n)
}

// headlineFunc is one class's headline, from its finding, its subject's name
// and its facts by key.
type headlineFunc func(f Finding, who string, n func(string) int64) string

// headlines is every class's headline but those a detector writes itself.
// TestEveryClassLeadsWithItsNumber holds every class a detector raises to one.
func headlines() map[Class]headlineFunc {
	return map[Class]headlineFunc{
		ClassHealthUnheld: func(_ Finding, _ string, n func(string) int64) string {
			return fmt.Sprintf("None of the company's %s may read the engine's health.",
				countSaid(int(n("agents")), "agent", "agents"))
		},
		ClassAgentMentionsWithoutAsk: func(_ Finding, _ string, n func(string) int64) string {
			return fmt.Sprintf("%d of %d agent comments this week woke %s without asking anything.",
				n("woken"), n("agent_comments"), countSaid(int(n("agents")), "agent", "agents"))
		},
		ClassPersonRequestsWithoutAsk: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s was named in %s this week that asked them nothing.", who,
				countSaid(int(n("named")), "comment", "comments"))
		},
		ClassAskOvertaken: func(_ Finding, _ string, n func(string) int64) string {
			return fmt.Sprintf("%d of %d answers this week came too late: %d asked again, %d after the work finished.",
				n("superseded")+n("moot"), n("answers"), n("superseded"), n("moot"))
		},
		ClassReviewIdle: func(f Finding, who string, n func(string) int64) string {
			items := countSaid(int(n("idle")), "item", "items")
			switch f.Focus {
			case "asked":
				return fmt.Sprintf("%s has been asked to review %s for a day or more.", who, items)
			case "nobody":
				return fmt.Sprintf("%s put %s in review and asked nobody to review them.", who, items)
			}
			return fmt.Sprintf("%s holds %s in review it has not touched for a day.", who, items)
		},
		ClassNoticeLoad: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s got %d notices this week, %d of them asking something.", who, n("notices"),
				n("addressed"))
		},
		ClassPickupStillWaiting: func(_ Finding, _ string, n func(string) int64) string {
			return fmt.Sprintf("%d of %d pickups this week ended in words only, the work waiting on something else.",
				n("words"), n("pickups"))
		},
		ClassAuxiliaryShare: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s spent %s of its %s tokens on learning this week (%d%%).", who,
				tokensSaid(n("auxiliary")), tokensSaid(n("tokens")), percent(n("auxiliary"), n("tokens")))
		},
		ClassRedeliveredSeen: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%d of %s's %d wakes this week came for changes it had already read.",
				n("absorbed"), who, n("woke"))
		},
		ClassNoVerifiedBackup: func(_ Finding, _ string, n func(string) int64) string {
			return fmt.Sprintf("No verified backup is recorded, so the engine keeps %s until one is.",
				countSaid(int(n("logs")), "log", "logs"))
		},
		ClassSandboxOutOfMemory: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s's sandbox ran out of memory in %s this week.", who,
				countSaid(int(n("runs")), "run", "runs"))
		},
		ClassInboundSilent: func(_ Finding, _ string, n func(string) int64) string {
			return fmt.Sprintf("The poll found %s your work waited on, and the webhook has delivered %d since.",
				countSaid(int(n("polled")), "pull request", "pull requests"), n("delivered"))
		},
		ClassWorkBottleneck: func(f Finding, who string, n func(string) int64) string {
			holder := "nobody holds"
			if f.Subject.Kind == SubjectSeat {
				holder = who + " holds"
			}
			return fmt.Sprintf("%s wait on %s, which %s; %d of them are ready to start.",
				countSaid(int(n("waiting")), "item", "items"), itemOf(f), holder, n("ready"))
		},
		ClassWorkHeldByPerson: func(f Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s wait on %s, which waits on %s, with %s.",
				countSaid(int(n("ready")), "agent item", "agent items"), itemOf(f), who,
				countSaid(int(n("questions")), "open question", "open questions"))
		},
		ClassMergeNoChecks: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s into %s went to people this week; its branch requires no check.",
				countSaid(int(n("merges")), "merge", "merges"), who)
		},
		ClassPoolWaits: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s waited for a database connection %s in the last day, %s this week.", who,
				countSaid(int(n("waits_day")), "time", "times"), countSaid(int(n("minutes")), "minute", "minutes"))
		},
		ClassFalseAlarm: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s on %s cleared by themselves within ten minutes this week.",
				countSaid(int(n("quick_clears")), "alarm", "alarms"), who)
		},
		ClassAlarmNoop: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%d of %s's %d alarm turns this week changed nothing, at %s tokens.", n("nothing"),
				who, n("alarm_turns"), tokensSaid(n("tokens")))
		},
		ClassRoundCap: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s's runs stopped at the cap of %d rounds %s this week, after %s tokens.", who,
				n("cap"), countSaid(int(n("runs")), "time", "times"), tokensSaid(n("tokens")))
		},
		ClassBudgetStopped: func(f Finding, who string, n func(string) int64) string {
			open := countSaid(int(n("open")), "open item", "open items")
			if f.Subject.Kind == SubjectCompany {
				return fmt.Sprintf("The company's budget stopped its agents %d hours this week while they held %s.",
					n("hours"), open)
			}
			return fmt.Sprintf("%s's budget stopped it %d hours this week while it held %s.", who, n("hours"), open)
		},
		ClassWorkConcentrated: func(_ Finding, who string, n func(string) int64) string {
			others := fmt.Sprintf("%d other agents hold", n("idle"))
			if n("idle") == 1 {
				others = "1 other agent holds"
			}
			return fmt.Sprintf("%s holds %d of the %d open items agents hold, while %s two or fewer.", who,
				n("open"), n("agents_open"), others)
		},
		ClassModelHeld: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s's work was held %d item-hours on its model's limit this week, of %d it stood.",
				who, n("limit_minutes")/60, n("standing_minutes")/60)
		},
		ClassScheduleUnproductive: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("Schedule %s ran %d times this week and moved work in %d.", who, n("fires"), n("moved"))
		},
		ClassAddSeat: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s waited %d item-hours for %s's %s, busy %d%% of their capacity.",
				countSaid(int(n("items")), "item", "items"), n("wait_minutes")/60, who,
				countSaid(int(n("seats")), "seat", "seats"), n("busy_percent"))
		},
		ClassRemoveSeat: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s's %s sat idle %d%% of their capacity over two weeks.", who,
				countSaid(int(n("seats")), "seat", "seats"), n("idle_percent"))
		},
		ClassSandboxUnstartable: paused,
		ClassModelsCredential:   paused,
		ClassAtlasOwnerGone:     atlasCounted,
		ClassAtlasUnmonitored:   atlasCounted,
		ClassAtlasUnowned:       atlasCounted,
		ClassAtlasDisagreement:  atlasCounted,
		ClassAtlasSourceStale: func(_ Finding, who string, n func(string) int64) string {
			return fmt.Sprintf("%s has told the atlas nothing for %s.", who,
				countSaid(int(n("hours")), "hour", "hours"))
		},
	}
}

// paused is a pause class's headline: how many agents it holds, and why.
func paused(f Finding, _ string, n func(string) int64) string {
	because := strings.TrimSuffix(strings.TrimPrefix(f.Title, "Paused because "), ".")
	return fmt.Sprintf("%s paused because %s.", countSaid(int(n("agents")), "agent is", "agents are"), because)
}

// atlasCounted is an atlas class's headline: its title, which counts the
// entries it is about.
func atlasCounted(f Finding, _ string, _ func(string) int64) string { return f.Title }

// itemOf is the item a finding about one item is about.
func itemOf(f Finding) string {
	if key, ok := strings.CutPrefix(f.Focus, "item:"); ok {
		return key
	}
	for _, r := range f.Records {
		if r.Kind == RecordItem {
			return r.ID
		}
	}
	return "one item"
}
