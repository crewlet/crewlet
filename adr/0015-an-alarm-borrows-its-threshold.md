# ADR-0015 — An alarm fires at a threshold another decision already made

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/statelog.TestTheBackupAlarmFiresAtTheAgeThePolicyNames`
- **Measured:** the plan this replaces tinted a row `caution` past one apply linger — 250 ms, on a position refreshed every 15 seconds, so every row was lit always — and `critical` past `min_age`, seven days, which is 10 080 times the grace that actually does anything.
- **Tag-status:** unreleased

## The decision

An alarm never invents a number. It fires at the threshold some OTHER decision
already made and named, and where that constant belongs to another package it
is taken from there rather than copied.

A stall grace is what sheds a node. A deferral grace is what moves its seats. A
read budget is what a caller was promised. A backup age is what the retention
policy states. Each of those is a decision with consequences of its own, and
the alarm's job is to say that the system has reached it — not to hold a second
opinion about when reaching it matters.

The second half is that ONE evaluation feeds every surface. A gauge, a log line
and a screen read the same table, so they cannot disagree about whether
something is wrong. The table lives in `internal/statelog` rather than beside
any one subsystem because an alarm is the framework's answer to "is this node
doing its job", and everything above it asks the same question.

## Why the obvious alternative is wrong

The obvious alternative is that each alarm picks the threshold that suits it —
a dashboard wants to warn earlier than a pager does, and picking a number per
surface is easy and locally reasonable.

What it produces is two numbers for one event, and they drift in opposite
directions. The measured case is above: an alarm whose caution threshold was
one apply linger fired on every healthy node on every tick, so the row was
always lit and therefore said nothing; the same alarm's critical threshold was
`min_age`, four orders of magnitude past the grace that actually moves seats,
so it could not fire before the thing it was warning about had already
happened. Both numbers were defensible where they were written. Neither had any
relationship to a decision anybody had made about the system.

An alarm that borrows cannot drift, because there is nothing to drift from.
When the grace that sheds a node changes, the alarm about it changes with it,
in the same commit, without anybody remembering to.

## What this does not decide

It does not say an alarm may never have a constant of its own — a p95 TARGET
for interactive search is a promise nothing else makes, so it is named here and
is the threshold. What is forbidden is a SECOND number for an event that
already has one.

It does not decide the alarm table's contents, its severity vocabulary or how a
surface renders it. And it does not extend to metrics: a counter or a histogram
records what happened at whatever resolution it has, and the thresholds are
this record's subject only where something CONCLUDES that a system is unwell.
