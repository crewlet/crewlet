/**
 * The documents' text: what FairSplit's people and agents wrote, as the
 * Markdown each version published.
 */

export const SETTLEMENT_DESIGN_V1 = `# Settlement engine

FairSplit settles a group by asking for payments. This note is how the engine
picks them.

## The problem

A group's balances always sum to zero: what some members are owed, others owe.
Any set of payments that brings every balance to zero settles the group, but a
long list of small payments is the reason groups stop settling at all.

## The approach

1. Read every member's balance in cents, never in floating point.
2. Split members into those who owe and those who are owed.
3. Pay the largest debt to the largest credit, then repeat.
`;

export const SETTLEMENT_DESIGN_V3 = `# Settlement engine

FairSplit settles a group by asking for payments. This note is how the engine
picks them, and why the list it prints is as short as it can be.

## The problem

A group's balances always sum to zero: what some members are owed, others owe.
Any set of payments that brings every balance to zero settles the group, but a
long list of small payments is the reason groups stop settling at all. In the
beta, groups that saw three payments or fewer settled within a week 9 times in
10; groups that saw six or more settled 4 times in 10.

## The approach

1. Read every member's balance in **cents**, never in floating point.
2. Split members into those who owe and those who are owed.
3. Pay the largest debt to the largest credit, then repeat until both lists
   are empty.

The greedy pass is not always optimal: finding the true minimum is NP-hard in
general. For groups under twelve members we first look for subsets whose
balances cancel exactly, settle each subset on its own, and only then run the
greedy pass on what is left. Every beta group is under twelve members.

## The leftover cent

When an amount does not split evenly, the parts are rounded down and the
leftover cents go, one each, to members in the order the group's rule names:
first names in order by default, or the payer. The engine writes down who took
each leftover cent, so a settle-up can always say why a part is one cent more.

## What it prints

\`\`\`
$ fairsplit settle "Lisbon trip"
Ben pays Ana   182.40
Dita pays Ana   96.20
Femi pays Chen  41.00
3 payments settle the group.
\`\`\`

## Open questions

- Should a member be able to pin a payment (for example, rent always goes to
  the leaseholder) before the engine plans the rest?
- Is the subset search worth its cost above eight members? The benchmarks in
  FAIR-3 say it adds 40 ms at eleven members.
`;

export const LAUNCH_PLAN_V2 = `# Beta launch plan

The November beta opens FairSplit to fifty groups. This plan is what has to be
true before we invite them, and who makes it true.

## What ships

| Piece | Task | Owner | State |
| --- | --- | --- | --- |
| Fewest payments | FAIR-3 | Crewlet-SWE | In review |
| Exact cent splits | FAIR-4 | Crewlet-SWE | Done |
| Local storage | FAIR-5 | Crewlet-SWE | Waiting on a decision |
| Recording from the command line | FAIR-2 | Crewlet-SWE | Done |

## Who we invite

- The 48 groups already recording expenses, by email, on 4 November.
- Two student housing operators Miles is introducing, as groups of their own.

## What we measure

- Groups settled within a week of their first expense.
- Payments per settle-up: the engine's whole point is that this stays under two.
- Rounding disputes reported through the feedback sheet.

## Risks

1. **Storage.** FAIR-5 is waiting on the storage decision. Without it, a group's
   history is lost when the session ends.
2. **Currency.** Two groups record in more than one currency. The beta settles
   in one currency per group, and says so when a second one appears.
`;

export const WEEKLY_UPDATE = `# Weekly update, week 41

## Done

- FAIR-2: recording an expense from the command line, with tests.
- FAIR-4: exact cent splits with the leftover-cent rule, approved first pass.

## In progress

- FAIR-3: the fewest-payments engine is in review. The subset search found
  shorter plans for 7 of the 48 beta groups.

## Waiting on Miles

- FAIR-1 holds two scope questions: storage, and whether the companion app
  joins the November beta.

## Numbers

Expenses recorded rose 31% month on month to 1,284. 92% of balances settled
within a week, up from 84% in September.
`;

export const INTERVIEW_NOTES = `# Interview notes, Flat 4B

Three flatmates, two years of shared bills, one spreadsheet nobody trusts.

## What they do today

- Rent and utilities go through one person, who chases the others monthly.
- Groceries are "roughly even", and nobody records them.

## What they said

> "We stopped arguing about who owes the cent. The app says it before anyone pays."

> "I would record an expense on my phone at the till. I will never open a laptop for it."

## What it means for us

- Recording has to take under ten seconds on a phone.
- The settle-up screen should name the leftover cent before anyone pays.
`;

export const PRICING_NOTES = `# Pricing notes, draft

Free for groups of up to six. A paid plan for housing operators who run many
groups, priced per active group per month. Not decided; parked until the beta
closes.
`;

export const TEMPLATE_BRIEF = `# Project brief

## Why

One paragraph on the problem and who has it.

## What

What we will build, in three bullets or fewer.

## How we will know

The measure that says it worked, and by when.

## Open questions

- The first question someone has to answer.
`;

export const TEMPLATE_NOTES = `# Meeting notes

**When:** the date. **Who:** the people there.

## Decisions

- What was decided, and who owns it.

## Actions

- [ ] The next step, its owner and its date.
`;

export const BENCHMARKS = `# Benchmarks

Planning time for the fewest payments, on a laptop, median of 1,000 runs.

| Members | Greedy pass | With the subset search |
| --- | --- | --- |
| 4 | 0.1 ms | 0.2 ms |
| 6 | 0.1 ms | 1.4 ms |
| 8 | 0.2 ms | 6.8 ms |
| 11 | 0.2 ms | 40.3 ms |

The subset search found a shorter plan for 7 of the 48 beta groups. Every
one of the seven has six members or more.

## What we keep

- The subset search runs for groups under twelve members.
- Above that, the greedy pass alone, and the plan says so.
`;

export const INVITE_LIST = `# Invite list

## Already recording

All 48 beta groups, by email on 4 November, from the address they signed up
with.

## New

- Two student housing operators Miles is introducing, each as a group of its
  own, with an operator account.
- The climbing club's sister club, at their treasurer's request.

## Not yet

- Groups that record in more than one currency, until the beta settles in
  more than one.
`;
