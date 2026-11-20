/**
 * One finding as a row of the Advisor's lists: its headline, one line of why,
 * the numbers behind it, ONE press that says what it does ("Let Agent SWE
 * watch the engine's health", "Take a backup", "Open ENG-12"), and a
 * menu with the rest (the fix's other changes, Snooze, Dismiss with a reason,
 * Ask Crewlet). Everything else is behind Details: the model's explanation,
 * the records, when it was found and checked (`view.ts` reads the row off
 * the finding).
 *
 * APPLY CONFIRMS IN PLACE: the press opens the re-check and the change's
 * preview under the row itself ([ApplyPanel]), where the person confirms.
 *
 * A ROW SAYS WHAT BECAME OF IT where that changes what to do: snoozed until a
 * time, applied and checking its effect, not re-checked for a while, a change
 * that did not clear it, seen before. A past instant is said as how long ago,
 * one a person plans by to the minute.
 *
 * ONE CONDITION ON MANY SUBJECTS IS ONE ROW. A class whose fix is a change to
 * one seat is found once per seat, each its own finding, applied and
 * dismissed on its own; two or more of one class in a list are one row
 * ("7 seats"), each subject inside it with its own press and menu
 * ([listItems]).
 */

import { Suspense, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  IconButton,
  Link,
  Menu,
  Text,
  type AttentionItem,
  type MenuEntry,
} from "@crewlethq/ui";
import { EllipsisGlyph } from "@crewlethq/icons/glyphs";
import { ChunkRegion, lazyScreen } from "~/app/lazyScreen.ts";
import { href } from "~/app/router.tsx";
import { ago } from "~/lib/ago.ts";
import { fmtAt, plural } from "~/lib/format.ts";
import { marked } from "~/ui/Problems.tsx";
import type { AdvisorKeptFinding, AdvisorRecordRef } from "~/protocol/advisorFindings.ts";
import type { DismissReason } from "./decisions.tsx";
import { figure } from "./Numbers.tsx";
import { findingView, type ViewAction, type ViewStat } from "./view.ts";
import { toneOf } from "./words.ts";

/** A cited record's page and how a list names it; null for one that has no page. */
export function recordLink(
  r: Pick<AdvisorRecordRef, "kind" | "id"> & { name?: string | null },
): { href: string; label: string; external?: boolean } | null {
  const named = (fallback: string) => r.name || fallback;
  switch (r.kind) {
    case "item":
      return { href: href(["worklet", r.id]), label: named(r.id) };
    case "turn":
      return { href: href(["live", "turns", r.id]), label: named(`turn ${r.id.slice(0, 8)}`) };
    case "seat":
      return { href: href(["company", "org", "seats", r.id]), label: named(r.id) };
    case "change": {
      const pull = /^https:\/\/github\.com\/([^/]+\/[^/]+)\/pull\/(\d+)/.exec(r.id);
      if (pull) return { href: r.id, label: `${pull[1]}#${pull[2]}`, external: true };
      const cut = r.id.lastIndexOf("!");
      if (cut <= 0) return null;
      return {
        href: href(["gitlet", "repos", r.id.slice(0, cut), "changes", r.id.slice(cut + 1)]),
        label: r.id,
      };
    }
  }
  return null;
}

/** The records a finding cites, as links; the rest counted. */
export function RecordLinks({ records }: { records: readonly AdvisorRecordRef[] }) {
  if (records.length === 0) return null;
  const links = records.flatMap((r) => {
    const link = recordLink(r);
    return link ? [{ ...link, key: `${r.kind}\u0000${r.id}` }] : [];
  });
  const others = records.length - links.length;
  return (
    <Text as="p" tone="secondary">
      Records:{" "}
      {links.map((l, i) => (
        <span key={l.key}>
          {i > 0 && ", "}
          <Link href={l.href} external={l.external} size="caption">
            {l.label}
          </Link>
        </span>
      ))}
      {others > 0 && `${links.length > 0 ? ", and " : ""}${plural(others, "other record")}`}
    </Text>
  );
}

/** Whether a person's snooze holds the finding off the list at `now`. */
export function snoozed(f: AdvisorKeptFinding, now: number): boolean {
  return f.snoozed_until !== null && Date.parse(f.snoozed_until) > now;
}

/**
 * What became of a finding, where that changes what to do: snoozed, applied
 * and waiting on its effect, not re-checked for a while, a change that did
 * not clear it, seen before. A finding as found has none.
 */
export function marks(f: AdvisorKeptFinding, now: number): string[] {
  const parts: string[] = [];
  if (snoozed(f, now)) parts.push(`snoozed until ${fmtAt(f.snoozed_until, now)}`);
  if (f.state === "applied" && f.effect_due_at) {
    parts.push(`applied, checking its effect until ${fmtAt(f.effect_due_at, now)}`);
  } else if (f.confirmed === "stale") {
    parts.push(`not re-checked since ${fmtAt(f.last_confirmed_at, now)}`);
  }
  if (f.ineffective)
    parts.push(`a change applied ${ago(f.ineffective.applied_at, now)} did not clear it`);
  if (f.seen_before > 0) parts.push(`seen ${plural(f.seen_before, "time")} before`);
  return parts;
}

/** When it was found, by whom, and when its check last confirmed it: Details' line. */
export function freshness(f: AdvisorKeptFinding, now: number): string {
  const found = `Found ${f.source === "model" ? "by the Advisor's model " : ""}${ago(f.opened_at, now)}`;
  return f.confirmed === "stale" ? found : `${found} · checked ${ago(f.last_confirmed_at, now)}`;
}

/** A stat as the row says it: what it counts, then its number in its unit ("Hours held: 31 h"). */
export function statWords(s: ViewStat): string {
  const shown = figure(s.value ?? 0, s.unit);
  const number = `${shown.value}${shown.unit && (s.unit === "hours" || s.unit === "minutes") ? ` ${shown.unit}` : ""}`;
  return `${s.label}: ${number}`;
}

/** What a row's presses do. */
export interface RowActions {
  /** Open the in-place Apply on a finding's option. */
  apply: (f: AdvisorKeptFinding, option: number) => void;
  /** Open an atlas finding's fix on the entries it names. */
  review: (f: AdvisorKeptFinding) => void;
  /** Ask Crewlet about the finding: its page, with `question` (or a plain one) waiting in Ask Crewlet's box. */
  ask: ((f: AdvisorKeptFinding, question?: string) => void) | null;
  snooze: (f: AdvisorKeptFinding) => void;
  /** End a person's snooze now: the finding is back on the list. */
  wake: (f: AdvisorKeptFinding) => void;
  dismiss: (f: AdvisorKeptFinding, reason: DismissReason) => void;
}

/**
 * The question Ask Crewlet is offered about a finding, for the person to send
 * or change: the engine's own where its fix is to ask (`ask_crewlet`), else
 * a plain one. NEVER THE FINDING'S ID OR TITLE: the question is asked from
 * the finding's own page, whose address the page context already names
 * (`app/assistant/context.ts`), and an id in it was a string of hex in the
 * person's own words.
 */
export function questionAbout(f: AdvisorKeptFinding): string {
  if (f.fix.kind === "ask_crewlet" && f.fix.prompt) return f.fix.prompt;
  return "What does this finding rest on, does it still hold, and what would its fix change?";
}

/** Whether the engine builds a change for the finding that Apply re-checks and sends. */
export function appliable(f: AdvisorKeptFinding): boolean {
  return (
    f.state === "open" &&
    ((f.fix.kind === "config_change" && f.fix.options.length > 0) || f.fix.kind === "api_change")
  );
}

/**
 * A finding's one press, labelled with what it does. `subject` names whom it
 * is for where its words alone are the same on every line beside it (the
 * subjects inside a row of one class).
 */
export function ActionButton({
  f,
  action,
  actions,
  subject,
}: {
  f: AdvisorKeptFinding;
  action: ViewAction;
  actions: RowActions;
  subject?: string | undefined;
}): ReactNode {
  const named = subject ? { "aria-label": `${action.label} for ${subject}` } : {};
  switch (action.kind) {
    case "apply":
      return (
        <Button
          size="small"
          variant="secondary"
          onClick={() => actions.apply(f, action.option)}
          {...named}
        >
          {action.label}
        </Button>
      );
    case "open":
      return (
        <ButtonLink
          size="small"
          variant="secondary"
          href={action.href}
          {...(action.external ? { target: "_blank", rel: "noopener noreferrer" } : {})}
          {...named}
        >
          {action.label}
        </ButtonLink>
      );
    case "ask":
      return actions.ask ? (
        <Button
          size="small"
          variant="secondary"
          onClick={() => actions.ask!(f, action.question)}
          {...named}
        >
          {action.label}
        </Button>
      ) : null;
    case "review":
      return (
        <Button size="small" variant="secondary" onClick={() => actions.review(f)} {...named}>
          {action.label}
        </Button>
      );
  }
}

/** A secondary press, as a menu entry. */
function menuEntry(
  f: AdvisorKeptFinding,
  a: ViewAction,
  actions: RowActions,
  i: number,
): MenuEntry | null {
  switch (a.kind) {
    case "apply":
      return { key: `apply-${i}`, label: a.label, onSelect: () => actions.apply(f, a.option) };
    case "open":
      return {
        key: `open-${i}`,
        label: a.label,
        onSelect: () => window.open(a.href, a.external ? "_blank" : "_self"),
      };
    case "ask":
      return actions.ask
        ? { key: `ask-${i}`, label: a.label, onSelect: () => actions.ask!(f, a.question) }
        : null;
    case "review":
      return { key: `review-${i}`, label: a.label, onSelect: () => actions.review(f) };
  }
}

/**
 * The row's menu: what a person answers a finding with besides its fix. `name`
 * is what the menu is for, where the finding's title is not enough to tell it
 * from its neighbours (a subject inside a row of one class).
 */
export function FindingMenu({
  f,
  actions,
  now,
  name = f.title,
}: {
  f: AdvisorKeptFinding;
  actions: RowActions;
  now: number;
  name?: string;
}) {
  if (f.state !== "open") return null;
  const others = findingView(f)
    .actions.secondary.map((a, i) => menuEntry(f, a, actions, i))
    .filter((e): e is MenuEntry => e !== null);
  const items: MenuEntry[] = [
    ...others,
    ...(others.length > 0 ? [{ kind: "separator" as const, key: "sep-fix" }] : []),
    snoozed(f, now)
      ? { key: "wake", label: "End the snooze", onSelect: () => actions.wake(f) }
      : { key: "snooze", label: "Snooze for 7 days", onSelect: () => actions.snooze(f) },
    { kind: "separator", key: "sep-dismiss" },
    {
      key: "wont_fix",
      label: "Dismiss: won't fix",
      onSelect: () => actions.dismiss(f, "wont_fix"),
    },
    { key: "known", label: "Dismiss: already known", onSelect: () => actions.dismiss(f, "known") },
    { key: "wrong", label: "Dismiss: it's wrong", onSelect: () => actions.dismiss(f, "wrong") },
    ...(actions.ask && f.fix.kind !== "ask_crewlet"
      ? [
          { kind: "separator" as const, key: "sep-ask" },
          {
            key: "ask",
            label: "Ask Crewlet about this",
            onSelect: () => actions.ask!(f),
          },
        ]
      : []),
  ];
  return (
    <Menu
      label={`More for ${name}`}
      align="end"
      items={items}
      renderTrigger={(trigger) => (
        <IconButton
          {...trigger}
          size="sm"
          variant="ghost"
          icon={<EllipsisGlyph size="sm" />}
          label={`More for ${name}`}
        />
      )}
    />
  );
}

/** The Apply a page has open: on which finding, and which of its fix's options. */
export interface OpenApply {
  id: string;
  option: number;
}

/** What draws the open Apply: the page's own [ApplyPanel]. */
export type ApplyFor = (f: AdvisorKeptFinding, option: number) => ReactNode;

/** The row's one line of numbers and what became of it. */
function statLine(f: AdvisorKeptFinding, now: number) {
  const items = [...findingView(f).stats.map(statWords), ...marks(f, now)];
  return items.length > 0 ? [{ items }] : undefined;
}

/** What Details opens: the model's words, the records, and when it was found and checked. */
function Details({ f, now }: { f: AdvisorKeptFinding; now: number }) {
  const view = findingView(f);
  return (
    <div className="col gap-1">
      {view.why !== f.why ? <Text as="p">{marked(f.why)}</Text> : null}
      {f.explanation ? (
        <Text as="p">
          <span className="advisor-said">The Advisor's model explains: </span>
          {marked(f.explanation)}
        </Text>
      ) : null}
      <Text as="p">{marked(f.fix.sentence)}</Text>
      <RecordLinks records={f.evidence.records} />
      <Text as="p" variant="caption" tone="secondary">
        {freshness(f, now)} ·{" "}
        <Link href={href(["advisor", "findings", f.id])} size="caption">
          Its page
        </Link>
      </Text>
    </div>
  );
}

/** One finding as a row of a list: its headline, why, numbers, one press, its menu, and its open Apply. */
export function findingItem(
  f: AdvisorKeptFinding,
  now: number,
  actions: RowActions,
  open: OpenApply | null = null,
  applyFor: ApplyFor | null = null,
): AttentionItem {
  const view = findingView(f);
  const primary = view.actions.primary;
  const applying = open !== null && open.id === f.id && applyFor !== null;
  const label = view.actions.primary?.kind === "apply" ? view.actions.primary.label : "the fix";
  return {
    key: f.id,
    tone: toneOf(f.severity),
    subject: f.subject.name || undefined,
    // THE ENGINE'S SENTENCES, drawn as every engine sentence is ([marked]).
    title: (
      <Link href={href(["advisor", "findings", f.id])} size="body">
        {marked(view.headline)}
      </Link>
    ),
    lines: statLine(f, now),
    meta: marked(view.why),
    action: (
      <span className="advisor-row-actions">
        {primary ? <ActionButton f={f} action={primary} actions={actions} /> : null}
        <FindingMenu f={f} actions={actions} now={now} />
      </span>
    ),
    details: <Details f={f} now={now} />,
    ...(applying
      ? {
          panel: applyFor(f, open.option),
          panelLabel: `Apply: ${optionLabel(f, open.option) ?? label}`,
        }
      : {}),
  };
}

/** The label of a fix's option, as its press says it. */
function optionLabel(f: AdvisorKeptFinding, option: number): string | null {
  const v = findingView(f).actions;
  const all = [v.primary, ...v.secondary];
  const a = all.find((x) => x?.kind === "apply" && x.option === option);
  return a ? a.label : null;
}

/** What a subject of each kind is called, one and many, counted in a row of one class: "7 seats". */
const SUBJECT_WORDS: Readonly<Record<string, readonly [string, string]>> = {
  seat: ["seat", "seats"],
  group: ["group of seats", "groups of seats"],
  unit: ["unit", "units"],
  schedule: ["schedule", "schedules"],
  item: ["item", "items"],
  model: ["model", "models"],
  node: ["node", "nodes"],
  objective: ["objective", "objectives"],
  repository: ["repository", "repositories"],
  integration: ["integration", "integrations"],
  atlas: ["atlas entry", "atlas entries"],
};

/** How many subjects a row of one class holds, in words: "7 seats", or "3 findings" for mixed or unknown kinds. */
export function subjectsWords(findings: readonly AdvisorKeptFinding[]): string {
  const kinds = new Set(findings.map((f) => f.subject.kind));
  const words = kinds.size === 1 ? SUBJECT_WORDS[findings[0]!.subject.kind] : undefined;
  return words ? plural(findings.length, words[0], words[1]) : plural(findings.length, "finding");
}

/**
 * A list's findings as its rows, in the order given: a finding alone is its
 * own row, and two or more of one class are one row where the first of them
 * stands, each subject inside it ([groupItem]).
 */
export function listItems(
  findings: readonly AdvisorKeptFinding[],
  now: number,
  actions: RowActions,
  open: OpenApply | null = null,
  applyFor: ApplyFor | null = null,
): AttentionItem[] {
  const byClass = new Map<string, AdvisorKeptFinding[]>();
  for (const f of findings) byClass.set(f.class, [...(byClass.get(f.class) ?? []), f]);
  const drawn = new Set<string>();
  const items: AttentionItem[] = [];
  for (const f of findings) {
    const same = byClass.get(f.class)!;
    if (same.length < 2) {
      items.push(findingItem(f, now, actions, open, applyFor));
    } else if (!drawn.has(f.class)) {
      drawn.add(f.class);
      items.push(groupItem(same, now, actions, open, applyFor));
    }
  }
  return items;
}

/**
 * Findings of one class as one row: the class's sentence (the engine writes
 * one per class), how many subjects it holds, and inside it each subject with
 * its own press and menu. An Apply opened on one of them opens under the row.
 */
export function groupItem(
  findings: readonly AdvisorKeptFinding[],
  now: number,
  actions: RowActions,
  open: OpenApply | null = null,
  applyFor: ApplyFor | null = null,
): AttentionItem {
  const first = findings[0]!;
  const view = findingView(first);
  const target = open ? findings.find((f) => f.id === open.id) : undefined;
  return {
    key: `class:${first.class}`,
    tone: toneOf(first.severity),
    subject: subjectsWords(findings),
    title: marked(view.headline),
    meta: marked(view.why),
    details: (
      <ul className="advisor-group" aria-label={`Each of the ${subjectsWords(findings)}`}>
        {findings.map((f) => {
          const name = f.subject.name || f.subject.id;
          const primary = findingView(f).actions.primary;
          const line = [...findingView(f).stats.map(statWords), ...marks(f, now)].join(" · ");
          return (
            <li key={f.id} className="advisor-group__row">
              <span className="col">
                <Link href={href(["advisor", "findings", f.id])} size="body">
                  {name}
                </Link>
                {line ? (
                  <Text as="span" variant="caption" tone="secondary">
                    {line}
                  </Text>
                ) : null}
              </span>
              <span className="advisor-row-actions">
                {primary ? (
                  <ActionButton f={f} action={primary} actions={actions} subject={name} />
                ) : null}
                <FindingMenu f={f} actions={actions} now={now} name={`${name}: ${f.title}`} />
              </span>
            </li>
          );
        })}
      </ul>
    ),
    ...(target && open && applyFor
      ? {
          panel: applyFor(target, open.option),
          panelLabel: `Apply for ${target.subject.name || target.subject.id}: ${optionLabel(target, open.option) ?? "the fix"}`,
        }
      : {}),
  };
}

/**
 * An atlas finding's fix (ADR-0141), from the atlas's own chunk: its pickers
 * and decisions are the atlas's, loaded when a reader first opens one.
 */
const AtlasFixDialog = lazyScreen("atlas", (m) => m.AdvisorAtlasFix);

/** The atlas fix dialog on `finding`, or nothing. */
export function AtlasFix({
  finding,
  onClose,
}: {
  finding: AdvisorKeptFinding | null;
  onClose: () => void;
}) {
  if (finding === null) return null;
  return (
    <ChunkRegion resetKey={finding.id}>
      <Suspense fallback={null}>
        <AtlasFixDialog finding={finding} onClose={onClose} />
      </Suspense>
    </ChunkRegion>
  );
}
