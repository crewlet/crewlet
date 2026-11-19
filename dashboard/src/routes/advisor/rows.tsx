/**
 * One finding as a row of the Advisor's lists, and the fix it offers.
 *
 * A ROW SAYS HOW FRESH IT IS. Each stored finding was last confirmed by a run
 * of its class; a row says when ("checked 4 minutes ago"), and one whose
 * class has not run for three of its intervals says it was not re-checked
 * since, so a reader never takes an old observation for the engine's view
 * now. Apply re-checks anyway (`apply.ts`).
 *
 * ONE ACTION, BY THE FIX'S KIND, and the rest behind the row's menu: Snooze
 * (or End the snooze, on a snoozed one), Dismiss with a reason, and Ask
 * Crewlet about it.
 *
 * A ROW'S TIMES ARE READ, NOT LOGGED: a past instant as how long ago, one a
 * person plans by as the time of day ("until Sat, Nov 21, 12:48", never its
 * seconds or this year). The exact instant is on the finding's page.
 *
 * ONE CONDITION ON MANY SUBJECTS IS ONE ROW. A class whose fix is a change to
 * one seat is found once per seat, each its own finding, applied and
 * dismissed on its own; listed as such, seven seats were seven rows of the
 * same sentence. Two or more findings of one class in a list are one row
 * ("7 seats"), each subject inside it with its own fix and menu ([listItems]).
 */

import { Suspense, type ReactNode } from "react";
import {
  Button,
  IconButton,
  InlineCode,
  Link,
  Menu,
  Text,
  type AttentionItem,
  type MenuEntry,
} from "@crewlethq/ui";
import { EllipsisGlyph } from "@crewlethq/icons/glyphs";
import type { AdvisorPage } from "~/protocol/advisor.ts";
import { ChunkRegion, lazyScreen } from "~/app/lazyScreen.ts";
import { href } from "~/app/router.tsx";
import { TAKE_BACKUP } from "~/lib/backups.ts";
import { ago } from "~/lib/ago.ts";
import { fmtAt, plural } from "~/lib/format.ts";
import { marked } from "~/ui/Problems.tsx";
import type { AdvisorKeptFinding, AdvisorRecordRef } from "~/protocol/advisorFindings.ts";
import type { DismissReason } from "./decisions.tsx";
import { impactWords, toneOf } from "./words.ts";

/** Where an `open_page` fix is taken, by the page the engine names and what it opens on. */
const PAGE_FIXES: Readonly<
  Record<
    AdvisorPage,
    (target: string) => { path: string[]; query?: Record<string, string>; action: string } | null
  >
> = {
  backups: () => ({
    path: ["settings", "backups"],
    query: { take: TAKE_BACKUP },
    action: "Take a backup",
  }),
  item: (key) => (key ? { path: ["worklet", key], action: `Open ${key}` } : null),
  seat: (handle) =>
    handle ? { path: ["company", "org", "seats", handle], action: "Open the agent" } : null,
};

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

/** How fresh a finding is, and what became of it, in its row's quiet line. */
export function freshness(f: AdvisorKeptFinding, now: number): string[] {
  const parts: string[] = [];
  const impact = impactWords(f);
  if (impact) parts.push(impact);
  if (snoozed(f, now)) parts.push(`snoozed until ${fmtAt(f.snoozed_until, now)}`);
  // A FINDING THE MODEL PASS FOUND is said to be the model's, as its page says.
  parts.push(
    `found ${f.source === "model" ? "by the Advisor's model " : ""}${ago(f.opened_at, now)}`,
  );
  if (f.state === "applied" && f.effect_due_at) {
    parts.push(`applied, checking its effect until ${fmtAt(f.effect_due_at, now)}`);
  } else if (f.confirmed === "stale") {
    parts.push(`not re-checked since ${fmtAt(f.last_confirmed_at, now)}`);
  } else {
    parts.push(`checked ${ago(f.last_confirmed_at, now)}`);
  }
  if (f.ineffective)
    parts.push(`a change applied ${ago(f.ineffective.applied_at, now)} did not clear it`);
  if (f.seen_before > 0) parts.push(`seen ${plural(f.seen_before, "time")} before`);
  return parts;
}

/** What a row's presses do. */
export interface RowActions {
  /** Open the Apply dialog on a finding whose change the engine builds. */
  apply: (f: AdvisorKeptFinding) => void;
  /** Open an atlas finding's fix on the entries it names. */
  review: (f: AdvisorKeptFinding) => void;
  /** Ask Crewlet about the finding: its page, with a question waiting in Ask Crewlet's box. */
  ask: ((f: AdvisorKeptFinding) => void) | null;
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
 * The one press a finding's fix offers, by its kind. `subject` names whom the
 * press is for where its words alone are the same on every line beside it
 * (the subjects inside a row of one class).
 */
export function fixAction(f: AdvisorKeptFinding, actions: RowActions, subject?: string): ReactNode {
  if (appliable(f)) {
    return (
      <Button
        size="small"
        onClick={() => actions.apply(f)}
        {...(subject ? { "aria-label": `Apply for ${subject}` } : {})}
      >
        Apply
      </Button>
    );
  }
  if (f.state !== "open") return null;
  if (f.fix.kind === "ask_crewlet" && actions.ask) {
    const ask = actions.ask;
    return (
      <Button size="small" onClick={() => ask(f)}>
        Ask Crewlet about this
      </Button>
    );
  }
  if (f.fix.kind === "external_step" && f.fix.url) {
    return (
      <Link href={f.fix.url} external size="caption">
        {f.subject.name ? `Open ${f.subject.name}` : "Open it"}
      </Link>
    );
  }
  if (f.fix.kind === "open_page" && f.fix.page && f.fix.page in PAGE_FIXES) {
    const page = PAGE_FIXES[f.fix.page as AdvisorPage](f.fix.target ?? "");
    return page ? (
      <Link href={href(page.path, page.query)} size="caption">
        {page.action}
      </Link>
    ) : null;
  }
  if (f.fix.kind === "open_setting") {
    return (
      <Link href={href(["settings", "config"])} size="caption">
        Open the settings
      </Link>
    );
  }
  // AN ATLAS FIX IS SENT WITH THE ATLAS'S OWN TOOLS, on the entries the
  // finding names; a reader the engine names none (no atlas app) is sent to
  // the atlas instead.
  if (f.fix.kind === "atlas_act" && f.fix.act && f.entries !== null) {
    return (
      <Button size="small" onClick={() => actions.review(f)}>
        Review the fix
      </Button>
    );
  }
  if (f.fix.kind === "atlas_act") {
    return (
      <Link href={href(["company", "atlas"])} size="caption">
        Open the atlas
      </Link>
    );
  }
  return null;
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
  const items: MenuEntry[] = [
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

/** One finding as a row of a list: what is wrong, how fresh it is, its fix and its menu. */
export function findingItem(
  f: AdvisorKeptFinding,
  now: number,
  actions: RowActions,
): AttentionItem {
  const action = fixAction(f, actions);
  return {
    key: f.id,
    tone: toneOf(f.severity),
    subject: f.subject.name || undefined,
    // THE ENGINE'S SENTENCES, drawn as every engine sentence is ([marked]).
    title: (
      <Link href={href(["advisor", "findings", f.id])} size="body">
        {marked(f.title)}
      </Link>
    ),
    meta: freshness(f, now).join(" · "),
    action: (
      <span className="advisor-row-actions">
        {action}
        <FindingMenu f={f} actions={actions} now={now} />
      </span>
    ),
    details: (
      <div className="col gap-1">
        <Text as="p">{marked(f.why)}</Text>
        {f.explanation ? (
          <Text as="p">
            <span className="advisor-said">The Advisor's model explains: </span>
            {marked(f.explanation)}
          </Text>
        ) : null}
        <Text as="p">{marked(f.fix.sentence)}</Text>
        {f.fix.kind === "open_setting" && f.fix.setting ? (
          <Text as="p" tone="secondary">
            The setting: <InlineCode>{f.fix.setting}</InlineCode>
          </Text>
        ) : null}
        <RecordLinks records={f.evidence.records} />
      </div>
    ),
  };
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
): AttentionItem[] {
  const byClass = new Map<string, AdvisorKeptFinding[]>();
  for (const f of findings) byClass.set(f.class, [...(byClass.get(f.class) ?? []), f]);
  const drawn = new Set<string>();
  const items: AttentionItem[] = [];
  for (const f of findings) {
    const same = byClass.get(f.class)!;
    if (same.length < 2) {
      items.push(findingItem(f, now, actions));
    } else if (!drawn.has(f.class)) {
      drawn.add(f.class);
      items.push(groupItem(same, now, actions));
    }
  }
  return items;
}

/**
 * Findings of one class as one row: the class's sentence (the engine writes
 * one per class), how many subjects it holds and since when, and inside it
 * each subject with its own fix, its menu and how fresh it is.
 */
export function groupItem(
  findings: readonly AdvisorKeptFinding[],
  now: number,
  actions: RowActions,
): AttentionItem {
  const first = findings[0]!;
  const oldest = findings.reduce((a, b) =>
    Date.parse(a.opened_at) <= Date.parse(b.opened_at) ? a : b,
  );
  return {
    key: `class:${first.class}`,
    tone: toneOf(first.severity),
    subject: subjectsWords(findings),
    title: marked(first.title),
    meta: `found ${ago(oldest.opened_at, now)}`,
    details: (
      <div className="col gap-2">
        <Text as="p">{marked(first.why)}</Text>
        <ul className="advisor-group" aria-label={`Each of the ${subjectsWords(findings)}`}>
          {findings.map((f) => {
            const name = f.subject.name || f.subject.id;
            return (
              <li key={f.id} className="advisor-group__row">
                <span className="col">
                  <Link href={href(["advisor", "findings", f.id])} size="body">
                    {name}
                  </Link>
                  <Text as="span" variant="caption" tone="secondary">
                    {freshness(f, now).join(" · ")}
                  </Text>
                </span>
                <span className="advisor-row-actions">
                  {fixAction(f, actions, name)}
                  <FindingMenu f={f} actions={actions} now={now} name={`${name}: ${f.title}`} />
                </span>
              </li>
            );
          })}
        </ul>
      </div>
    ),
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
