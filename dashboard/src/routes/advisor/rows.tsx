/**
 * One finding as a row of the Advisor's lists, and the fix it offers.
 *
 * A ROW SAYS HOW FRESH IT IS. Each stored finding was last confirmed by a run
 * of its class; a row says when ("checked 4 minutes ago"), and one whose
 * class has not run for three of its intervals says it was not re-checked
 * since, so a reader never takes an old observation for the engine's view
 * now. Apply re-checks anyway (`apply.ts`).
 *
 * ONE ACTION, BY THE FIX'S KIND, and the rest behind the row's menu: Snooze,
 * Dismiss with a reason, and Ask Crewlet about it.
 */

import type { ReactNode } from "react";
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
import { href } from "~/app/router.tsx";
import { TAKE_BACKUP } from "~/lib/backups.ts";
import { ago } from "~/lib/ago.ts";
import { fmtDateTime, plural } from "~/lib/format.ts";
import { marked } from "~/ui/Problems.tsx";
import type { AdvisorDismissReason, AdvisorEvidenceRecord, AdvisorFinding } from "./types.ts";
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
  r: Pick<AdvisorEvidenceRecord, "kind" | "id"> & { name?: string | null },
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
export function RecordLinks({ records }: { records: readonly AdvisorEvidenceRecord[] }) {
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

/** How fresh a finding is, and what became of it, in its row's quiet line. */
export function freshness(f: AdvisorFinding, now: number): string[] {
  const parts: string[] = [];
  const impact = impactWords(f);
  if (impact) parts.push(impact);
  parts.push(`found ${ago(f.opened_at, now)}`);
  if (f.state === "applied" && f.effect_due_at) {
    parts.push(`applied, checking its effect until ${fmtDateTime(f.effect_due_at)}`);
  } else if (f.confirmed === "stale") {
    parts.push(`not re-checked since ${fmtDateTime(f.last_confirmed_at)}`);
  } else {
    parts.push(`checked ${ago(f.last_confirmed_at, now)}`);
  }
  if (f.ineffective)
    parts.push(`a change applied ${fmtDateTime(f.ineffective.applied_at)} did not clear it`);
  if (f.seen_before > 0) parts.push(`seen ${plural(f.seen_before, "time")} before`);
  return parts;
}

/** What a row's presses do. */
export interface RowActions {
  /** Open the Apply dialog on a finding whose change the engine builds. */
  apply: (f: AdvisorFinding) => void;
  /** Ask Crewlet about the finding, beside the page. */
  ask: ((question: string) => void) | null;
  snooze: (f: AdvisorFinding) => void;
  dismiss: (f: AdvisorFinding, reason: AdvisorDismissReason) => void;
}

/** What Ask Crewlet is asked about a finding: the finding by its id, which it reads itself. */
export function askAbout(f: AdvisorFinding): string {
  const prompt = f.fix.kind === "ask_crewlet" && f.fix.prompt ? `${f.fix.prompt} ` : "";
  return `${prompt}Explain the Advisor's finding "${f.title}" (finding ${f.id}): what it rests on, whether it still holds, and what its fix would change.`;
}

/** Whether the engine builds a change for the finding that Apply re-checks and sends. */
export function appliable(f: AdvisorFinding): boolean {
  return (
    f.state === "open" &&
    ((f.fix.kind === "config_change" && f.fix.options.length > 0) || f.fix.kind === "api_change")
  );
}

/** The one press a finding's fix offers, by its kind. */
export function fixAction(f: AdvisorFinding, actions: RowActions): ReactNode {
  if (appliable(f)) {
    return (
      <Button size="small" onClick={() => actions.apply(f)}>
        Apply
      </Button>
    );
  }
  if (f.state !== "open") return null;
  if (f.fix.kind === "ask_crewlet" && actions.ask) {
    const ask = actions.ask;
    return (
      <Button size="small" onClick={() => ask(askAbout(f))}>
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
  if (f.fix.kind === "atlas_act") {
    return (
      <Link href={href(["company", "atlas"])} size="caption">
        Open the atlas
      </Link>
    );
  }
  return null;
}

/** The row's menu: what a person answers a finding with besides its fix. */
export function FindingMenu({ f, actions }: { f: AdvisorFinding; actions: RowActions }) {
  if (f.state !== "open") return null;
  const items: MenuEntry[] = [
    { key: "snooze", label: "Snooze for 7 days", onSelect: () => actions.snooze(f) },
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
            onSelect: () => actions.ask!(askAbout(f)),
          },
        ]
      : []),
  ];
  return (
    <Menu
      label={`More for ${f.title}`}
      align="end"
      items={items}
      renderTrigger={(trigger) => (
        <IconButton
          {...trigger}
          size="sm"
          variant="ghost"
          icon={<EllipsisGlyph size="sm" />}
          label={`More for ${f.title}`}
        />
      )}
    />
  );
}

/** One finding as a row of a list: what is wrong, how fresh it is, its fix and its menu. */
export function findingItem(f: AdvisorFinding, now: number, actions: RowActions): AttentionItem {
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
        <FindingMenu f={f} actions={actions} />
      </span>
    ),
    details: (
      <div className="col gap-1">
        <Text as="p">{marked(f.why)}</Text>
        {f.explanation ? <Text as="p">{marked(f.explanation)}</Text> : null}
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
