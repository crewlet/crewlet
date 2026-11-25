# ADR-0168: A busy seat's other wakes about its work are triaged, and the engine gates the choice

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:**
  - `internal/engine.TestOnlyAWakeAboutTheRunsWorkOrFromItsLeadIsTriaged`
  - `internal/engine.TestTheEngineGatesTheTriagesChoice`
  - `internal/engine.TestTheTriagesDecisionsAreClosedAndItsPromptNamesEach`
  - `internal/engine.TestATriagePlaysOverTheRealToolSessionAndFencesTheWake`
  - `internal/engine.TestALeadsBurstIsOneNoteAndPastTheCapItStays`
  - `internal/engine.TestAnInputWakeFromAnotherWriterJoinsAsInformationAndBindsTheTurn`
  - `internal/engine.TestNoiseIsAbsorbedOnlyOwingNothingAndARequestOrAFailureStays`
  - `internal/engine.TestASteerNoteHoldsWholeWakesWithinANotesBound`
  - `internal/engine.TestAWaitingPersonIsToldWhyOncePerWake`
  - `internal/notify.TestAWaitingMessageIsToldWhyItWaits`
  - `internal/agent/prompts.TestATriageReadsTheWakeAndTheTaskInTheirFences`
  - `internal/agent/prompts.TestAnotherWritersWakeIsToldAsInformation`
  - `internal/engine.TestTheRequestersWakeAboutWhatTheRunWroteJoinsAndAnyoneElsesIsTriaged`
  - `internal/engine.TestAWakeAboutARecordTheRunWroteReachesTheRunningRun`
- **Cost-when-tried:** under ADR-0167 alone a colleague's fact about a running run's work ("the customer is on the EU region"), a lead's "stop, roll back" in chat and a "thanks!" in the run's room all waited for the run to end: the run worked on without the fact or the correction, and each message then started a turn of its own over a finished result, the thanks included.
- **Tag-status:** unreleased

## The decision

**A wake the requester rule (ADR-0167) leaves on a busy seat's inbox is weighed only where it is a candidate**: written in the run's conversation, about the run's work item, or by somebody the chart says leads the seat (`Chart.Leads`, as `steer_turn` decides it; a chart that cannot tell is no). A type a run never takes, a schedule's fire, a wake already worked and one with no words stay excluded, and every other wake waits with no model call.

**A candidate is triaged once per wake and job** by one call on the role's `triage` chain (`llm_triage`, else `llm`, else `default`): a one-tool pass whose `decide_wake` takes a closed `decision` (`input`, `steer`, `request`, `noise`) and a reason, reading the running turn's task and the wake each in its writer's fence. Two rounds, ninety seconds, at most sixteen triages a job and four a read; spent on the running turn's seat meter as `wake_triage`.

**The engine gates the description; the model never decides whom anything is for.** `input` joins as a wake item from anybody, told as information about the work from somebody the turn does not work for, never a request. `steer` joins only from a person who leads the seat (the chart asked again at the gate, a human seat), filed as their steer note through the person's path, under its seal and bounds: a burst of their wakes is one note, oldest first within a note's length, and a burst arriving while a note of theirs waits unread waits to become the next one, since a sealed note is never rewritten; past the turn's note cap it stays. Anybody else's `steer` is a `request`, and a request waits for a turn of its own. `noise` is absorbed, recorded worked by the running turn and skipped `absorbed` naming it, only when the wake owes nothing (no answer owed, not addressed to the seat, no question put to it; ADR-0120); one that owes anything waits. A triage that decides nothing (no chain, a refusal, a timeout, an invalid answer) leaves the wake waiting, said once and counted.

**A lead's note claims its messages at the filing**, unlike a wake item: a note on a running turn's record is read by its run or, at the latest, its review, so its delivery is dropped `joined` naming the turn, and an answer the messages were owed binds the turn (`steer.Note.Owes`). **An input wake is claimed at its read**, as ADR-0167's are, and its owed answer binds the turn ([ADR-0109](0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)'s amendment "a turn may read another writer's input").

## Why the obvious alternatives are wrong

**A model deciding whether the writer may steer, or whom a call is for.** The words are the writer's, and a model reading them can be told anything; whose authority a message carries is the chart's to say and whose request a turn works for is ADR-0109's, so the model's answer only ever picks among the actions the engine already allows that writer.

**Triaging every wake.** A chatty room would buy a call per message for words that cannot touch the run; the candidate rule spends one only where the wake can change the work.

**Rewriting the lead's waiting note with a later message.** A note a harness may already hold cannot be taken back, and a reseal would hand the run two versions of one note.

**Claiming a lead's note at its read.** A note is read on more paths than a wake (a box hand-over, a brief, a hook, a poll, the review), and a park between filing and read would let the message run again beside its note.

## Amendment: a request is put to who may let it join

The gate's `request` from a person or an agent seat other than the run's requester (a non-lead's `steer`
included) is put to the run's requester and the seat's leads as a fold-in before it waits, and
joins as a steer note once one of them approves it, under [ADR-0109](0109-a-turn-somebody-woke-is-held-to-the-apps-they-use.md)'s
amendment "another writer's request joins only on approval", which states the rule. Until an answer it is
left undecided, followed from the run's record at each read with no second triage; refused, unanswered when
the job ends, past a job's four fold-ins, or with nobody to put it to, it waits for a turn of its own. A
person whose wake waits is told why, once per wake and reason, on the conversation's queued line.

## Amendment: a wake about a record the run wrote is a candidate

A wake about a record the run created or wrote (ADR-0167's amendment, the same exact identity) is a candidate whoever wrote it, beside one in the run's conversation, about its item or a lead's, and the triage's message says so in the engine's line before the wake's fence. The gate is unchanged: such a wake's input joins as information, another writer's request is put as a fold-in (the amendment above), and noise is absorbed only owing nothing.
