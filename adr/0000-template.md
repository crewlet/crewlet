# ADR-0000 — The decision, as an imperative sentence

- **Status:** accepted
- **Authority:** `internal/somepackage`
- **Enforced-by:** `internal/somepackage.TestTheRuleHolds`
- **Measured:** the numbers this was settled on, or omit the line
- **Cost-when-tried:** what the obvious alternative cost *here*, or omit the line
- **Tag-status:** unreleased

<!--
Copy this file, take the next free number, name it after the decision, and
delete this comment.

Before writing anything: is this decision inside ONE package? Then it belongs
in that package's doc comment and not here. This directory is for a decision
that binds several packages, which is the one thing a package doc cannot hold.

Add the record's id to the authority's own doc. The gate in internal/adr checks
that anchor in both directions, so a record whose authority does not name it
back fails the build, and so does a doc naming a record that does not exist.
-->

## The decision

One or two paragraphs. State what is decided and what it forbids, in terms a
reader can act on. Name the seam, the type or the file that carries it.

Do not restate the authority. If the argument runs to three screens, that is
what the authority's doc is for, and the reader gets there from the line above.

## Why the obvious alternative is wrong

The part that earns the record. The obvious alternative is almost always the
shape somebody will propose again in a year, so name it and say what it costs —
preferably what it cost when it was tried, with the number.

## What this does not decide

The boundary. A record that reads as if it settles everything nearby is a
record somebody will cite for a decision it never made.
