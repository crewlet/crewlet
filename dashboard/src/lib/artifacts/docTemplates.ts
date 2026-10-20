/**
 * What a new doc can start from: the outline of a document a company writes
 * often, as Markdown the create page drops into its editor.
 *
 * A TEMPLATE FILLS THE PAGE, IT NEVER GATES IT: the page opens blank with the
 * cursor in the title, and these are offered beside it while the body is
 * empty. Each body is plain Markdown in the grammar the editor writes, so it
 * loads and saves byte for byte (`lib/richEditor.test.tsx` holds that), and
 * each section carries one line saying what goes there, which the writer
 * replaces.
 *
 * The doc's title is not part of a template: the engine writes it as the
 * tab's heading above the body.
 */

/** A doc template's key, as the create page's address may name it (`template=`). */
export type DocTemplateKey = "meeting" | "decision" | "brief" | "runbook" | "retro" | "howto";

export interface DocTemplate {
  key: DocTemplateKey;
  /** The chip's word. */
  label: string;
  /** What it is for, said on the chip's tooltip and to a screen reader. */
  description: string;
  /** The title field's example while this template is on the page. */
  titleExample: string;
  body: string;
}

export const DOC_TEMPLATES: readonly DocTemplate[] = [
  {
    key: "meeting",
    label: "Meeting notes",
    description: "Who came, what was discussed, what was decided and who does what next",
    titleExample: "Weekly planning, October 20",
    body: `**Date:** the day it met · **Attendees:** who came

## Agenda

1. The first topic.
2. The second topic.

## Notes

What was discussed, topic by topic.

## Decisions

- What was decided, and why.

## Action items

- [ ] Owner: what, by when.
`,
  },
  {
    key: "decision",
    label: "Decision record",
    description: "The context, the options weighed, what was decided and why",
    titleExample: "Move session storage to Postgres",
    body: `**Status:** proposed · **Deciders:** who decides

## Context

The problem, and the forces at play.

## Options

1. **Option A.** What it is, and what it costs.
2. **Option B.** What it is, and what it costs.

## Decision

What was chosen, and why it beats the alternatives.

## Consequences

What becomes easier, what becomes harder, and what to revisit.
`,
  },
  {
    key: "brief",
    label: "Project brief",
    description: "The problem, the goals, the scope and the milestones of a piece of work",
    titleExample: "Self-serve billing",
    body: `## Summary

The project in two sentences: the problem, and the outcome.

## Goals

- What success looks like, measurably.

## Non-goals

- What this project deliberately leaves out.

## Scope

What ships, and what is left for later.

## Milestones

| Milestone | Owner | Date |
| --- | --- | --- |
| The first milestone | Who | When |

## Risks

- What could go wrong, and how it is handled.
`,
  },
  {
    key: "runbook",
    label: "Runbook",
    description: "The steps to follow when something specific happens, and how to undo them",
    titleExample: "Database failover",
    body: `When to use this runbook, in one sentence.

## Before you start

- The access you need.
- The people to tell.

## Steps

1. The first step, and what you should see.
2. The next step.

## Check it worked

How to confirm the system is healthy again.

## Roll back

How to undo the steps if they make things worse.

## Escalate

Who to call, and when.
`,
  },
  {
    key: "retro",
    label: "Retrospective",
    description: "What went well, what did not and what the team will change",
    titleExample: "Q3 launch retrospective",
    body: `**Period:** the weeks it covers · **Team:** who took part

## What went well

- Something to keep doing.

## What did not go well

- Something that slowed the team down.

## What we will change

- [ ] Owner: the change, by when.
`,
  },
  {
    key: "howto",
    label: "How-to guide",
    description: "A task explained step by step for someone doing it for the first time",
    titleExample: "Set up a local environment",
    body: `What this guide helps you do, and who it is for.

## Before you begin

- What you need first.

## Steps

1. The first step, and what you should see.
2. The next step.

## Troubleshooting

**Symptom.** What to try.
`,
  },
];

/** The template a key names, or undefined. */
export function docTemplate(key: string): DocTemplate | undefined {
  return DOC_TEMPLATES.find((t) => t.key === key);
}
