# ADR-0116: An administrator notice is read off its condition, never written

- **Status:** accepted
- **Authority:** `internal/notice`
- **Enforced-by:** `internal/notice.TestAnAlarmPastItsThresholdReachesTheAdministratorsOnce`, `internal/notice.TestAPlatformNoticeClearsWithItsCondition`, `internal/notice.TestAPlatformNoticeOffersNoAnswer`, `internal/notice.TestTheBackupAlarmIsTheAdvisorsFinding`, `internal/notice.TestOnlyAnOwnersBlockingFindingIsANotice`, `internal/api/queries.TestTheAdvisorNoticesTheFleetsAlarms`
- **Cost-when-tried:** not tried here. Before this record an alarm reached a person only as a count in the dashboard's sidebar, and a seat only where its role carried `health_tools`; a live company ran a week with its backup alarm standing and nobody told, because no seat held that grant.
- **Tag-status:** unreleased

## The decision

**What the engine detects and only an administrator can fix reaches the
administrators as a platform notice, and a notice is read, never written.**
`GET /advisor` answers `notices` beside its findings: one per condition, keyed
by its class and subject, open exactly while the condition holds, with nothing
to answer. Two producers raise them today, and every later one (a failure
breaker's hold, a settled run) joins `internal/notice` rather than finding a
path of its own:

- **An alarm that has stood.** Every node's heartbeat already carries each
  alarm firing there and when it was raised, and a restart keeps that instant
  (ADR-0095). An alarm kind whose earliest raise on any live node is at least
  the deferral grace old is one notice naming every node it stands on. The
  threshold is borrowed (ADR-0015): the deferral grace is where the engine
  itself stops waiting for a condition to clear on its own.
- **A finding that blocks the company and is the owner's to fix.** A finding
  the engine is handling never reaches the owner as a to-do (ADR-0115).

A condition both raise is one notice under the finding's class and subject, so
the dashboard attaches the finding's fix: the backup alarm is the advisor's
no-verified-backup finding, never a second notice.

## Why the obvious alternative is wrong

A notice row per administrator, written when the condition starts and cleared
when it ends, is what an inbox usually is. It needs a writer that sees both
ends, and the conditions are seen by every node at once: each node would write
the backup notice, a node that died would leave its notice standing, a restart
would write it again (the re-raise ADR-0095 removed from wakes), and a person
made an administrator after the write would never see it. Read off the
condition, a notice cannot outlive it, two nodes give one notice, and whoever
administers the company when it is read is who it reaches.

A wake for each administrator, as an alarm wakes a seat holding `health_tools`,
tells a person nothing a seat does not already hear: a person has no turn to
start, and what they need is the condition while it holds, in Needs you.

## What this does not decide

Which alarms exist and where each fires (ADR-0015), the advisor's classes and
their fixes (ADR-0115), or what a seat does when an alarm wakes it (ADR-0095).
A notice has no read mark or snooze: it ends when its condition does, and a
finding the owner accepts is the advisor's to dismiss.
