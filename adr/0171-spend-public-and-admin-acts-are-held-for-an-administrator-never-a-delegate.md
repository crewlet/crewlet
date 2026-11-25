# ADR-0171: Spend, public and admin acts are held for an administrator, never a delegate

- **Status:** accepted
- **Authority:** `internal/growth`
- **Enforced-by:**
  - `internal/delegated.TestTheGrowthActsAreNeverDelegated`
  - `internal/config.TestADelegateIsAnAgentThatCannotRequestWhatItDecides`
  - `internal/authz.TestAGrowthActIsAnsweredByAnAdministratorInPerson`
  - `internal/agent/builtin.TestAGrowthSpendOrAdminCallIsHeldForAnAdministrator`
  - `internal/agent/builtin.TestAnApprovedGrowthCallIsMadeOnlyOnWhatWasApproved`
  - `internal/agent/builtin.TestAGrowthWriteIsMadeOnlyWithinItsToolsClass`
  - `internal/engine.TestAGrowthActLapsesADayAfterItWasHeld`
  - `internal/api/mcpapi.TestALapsedGrowthActTakesDenyAlone`
  - `internal/sandbox.TestALapsedGrowthActIsNeverAllowed`
  - `internal/spendguard.TestTheTwinKeepsTheSpendContract`
  - `internal/spendguard.TestAnApplyReadsAndAppliesInsideTheLease`
  - `internal/agent/builtin.TestAHeldGrowthCallsAskIsRecordedOnceItIsHeld`
  - `internal/accessdomain.TestAGrowthActHoldsOneAnswerAndOneOutcome`
  - `internal/googlegrowth.TestTwoAppliesOnOneTargetMakeOneCall`
  - `internal/googlegrowth.TestAHeldCallsAnswerIsRecordedBeforeItActs`
- **Tag-status:** unreleased

## The decision

**A growth integration's call that starts or raises spend, publishes to
customers, or manages the vendor account (its users, access, keys, billing
setup and policies, or an irreversible change) is a held call, answered Allow
once or Deny by a person holding admin, present, at a fresh step-up.** It is
the ssh_operate held call (ADR-0077) with approvers `admin`: never the seat's
lead, a person a list names, a delegate, a machine credential or the
assistant. Its acts, `ad_spend`, `public_post` and `account_admin`, are never
delegable, and the document refuses a delegated-decisions rule naming one. A
block's `approvers` only route the question. The approval binds the digest of
the exact vendor operation and of the target's before-state; the call is made
once, under the account's apply lease, only while both hold, and lapses
unanswered after a day, after which it takes Deny alone. Every write is
classed against its target as it reads now before it is made or asked, and
one coming to more than its tool's class is refused, so no lesser tool
reaches a held act. Spend also passes `internal/spendguard` at the ask and
again at the apply; a cost the vendor names no amount for, which no cap could
hold (Google Analytics' BigQuery exports), is admin rather than spend. Pausing and lowering are operate: made at once and
recorded, since a brake must not wait on a step-up and its undo is a decision.
The access log records every step: an operate call made, and a held call's
ask, its one answer and its one outcome.

## Why the obvious alternative is wrong

A delegate, or a lead by position, deciding money or what customers read would
let one model approve another's spend, and the rubber-stamping ADR-0125
measured on merges is the same failure with money in it. Classing a pause as a
decision too would hold the brake on runaway spend behind a step-up.

## What this does not decide

The caps' values, which a company sets per account, or what a product shows
on its card; each product's docs page says that.
