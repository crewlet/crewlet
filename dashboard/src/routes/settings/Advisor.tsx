/**
 * Settings › Advisor: what the company's setup costs it, why, and the change
 * that fixes each (ADR-0115).
 *
 * THE ENGINE NEVER APPLIES A FIX. A change is one entity write the engine
 * built; this screen reads the entity, sets the change's fields, asks the
 * entity route's dry run what the company would become, and shows that answer
 * as the preview. Only Apply sends the write, from this person's session and
 * under the entity's own tag, so a seat somebody edited in between is refused
 * rather than overwritten.
 */

import { Suspense, useState } from "react";
import {
  AttentionList,
  groupNotices,
  Button,
  Callout,
  EmptyState,
  InlineCode,
  Link,
  PageNote,
  Skeleton,
  StepList,
  Text,
  type AttentionItem,
  type AttentionTone,
  type StepListStep,
} from "@crewlethq/ui";
import { FixDialog } from "~/components/advisorFix.tsx";
import { GoLiveReview } from "./GoLiveReview.tsx";
import { QueryState } from "~/components/common.tsx";
import { useAssistantControl, type AssistantControl } from "~/app/assistant/control.ts";
import { ChunkRegion, lazyScreen } from "~/app/lazyScreen.ts";
import { href } from "~/app/router.tsx";
import { unreadSentences } from "~/lib/advisorUnread.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type {
  AdvisorFinding,
  AdvisorPage,
  AdvisorRecord,
  AdvisorSeverity,
} from "~/protocol/advisor.ts";
import { marked } from "~/ui/Problems.tsx";
import { TAKE_BACKUP } from "~/lib/backups.ts";
import { plural } from "~/lib/format.ts";

/**
 * An atlas finding's fix (ADR-0141), from the atlas's own chunk: its pickers
 * and decisions are the atlas's, loaded when a reader first opens one.
 */
const AtlasFixDialog = lazyScreen("atlas", (m) => m.AdvisorAtlasFix);

/** How often the list is read again at rest: a setup changes by revisions. */
const POLL_MS = 60_000;

/** Each severity as the list's tone, and the word it is read as. */
const TONE: Record<AdvisorSeverity, AttentionTone> = {
  blocks: "danger",
  costs: "warning",
  note: "info",
};
const OWNER_LABELS = {
  title: "What to change",
  tones: { danger: "Blocks", warning: "Costs", info: "Worth knowing" },
};
const ENGINE_LABELS = {
  title: "What the engine is handling",
  tones: { danger: "Engine", warning: "Engine", info: "Engine" },
};

export function Advisor() {
  const { data, loading, error, refusal, detail, refetch } = useQuery("advisor", undefined, {
    pollMs: POLL_MS,
  });
  const [open, setOpen] = useState<AdvisorFinding | null>(null);
  const assistant = useAssistantControl();
  const owner = data?.findings.filter((f) => f.actor === "owner") ?? [];
  const engine = data?.findings.filter((f) => f.actor === "engine") ?? [];
  return (
    <>
      <PageNote>
        What the company&rsquo;s setup costs it and the change that fixes each; nothing changes
        until you review it.
      </PageNote>
      {/* THE READ IN FLIGHT IS A SKELETON, drawn beside `QueryState`, which
          draws nothing while it waits: the screen was its one sentence over
          an empty page, which reads as "nothing to change". */}
      {loading && !data && <Skeleton variant="text" rows={6} label="Reading the setup" />}
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && data.unread.length > 0 && (
          <Callout variant="warning" title="Some of the setup couldn't be read">
            {unreadSentences(data.unread).join(" ")}
          </Callout>
        )}
        {data && <GoLiveCheck owner={owner} unread={data.unread} />}
        {data && <GoLiveReview />}
        {/* NOTHING TO CHANGE ONLY WHEN EVERY SOURCE WAS READ: an unread one may hold the blocker. */}
        {data &&
          (owner.length === 0 ? (
            data.unread.length === 0 && (
              <EmptyState
                size="compact"
                title="Nothing to change"
                description="Your company's setup raises nothing worth changing right now."
              />
            )
          ) : (
            <AttentionList
              items={grouped(owner).map((g) => groupItem(g, (f) => setOpen(f), assistant))}
              labels={OWNER_LABELS}
            />
          ))}
        {engine.length > 0 && (
          <AttentionList
            items={grouped(engine).map((g) => groupItem(g, null, null))}
            labels={ENGINE_LABELS}
          />
        )}
      </QueryState>
      {open?.fix.kind === "atlas_act" && (
        <ChunkRegion resetKey={open.class + open.path}>
          <Suspense fallback={null}>
            <AtlasFixDialog
              finding={open}
              onClose={() => {
                setOpen(null);
                refetch();
              }}
            />
          </Suspense>
        </ChunkRegion>
      )}
      {open && open.fix.kind !== "atlas_act" && (
        <FixDialog
          finding={open}
          onClose={() => setOpen(null)}
          onApplied={() => {
            setOpen(null);
            refetch();
          }}
        />
      )}
    </>
  );
}

/** The go-live check's own words for a step's state. */
const GO_LIVE_LABELS = { done: "Done", waiting: "To do", pending: "Can't say yet" };

/**
 * THE GO-LIVE CHECK: the owner's findings counted by what ignoring them costs,
 * as the three things to do before the agents start, each done when none is
 * left. Read off the same answer as the list below, so the two cannot differ;
 * a source the answer could not read leaves its step unsaid rather than done.
 */
function GoLiveCheck({ owner, unread }: { owner: AdvisorFinding[]; unread: string[] }) {
  // ROWS, NOT FINDINGS: a class raised for nine seats is one thing to do.
  const rows = grouped(owner);
  const count = (severity: AdvisorSeverity) =>
    rows.filter((g) => g[0]!.severity === severity).length;
  const blocks = count("blocks");
  const costs = count("costs");
  const notes = count("note");
  const blind = unread.length > 0;
  const steps: StepListStep[] = [
    {
      id: "blocks",
      label: blocks
        ? `Fix what blocks the company (${blocks})`
        : blind
          ? "Nothing that could be read blocks the company"
          : "Nothing blocks the company",
      state: blocks ? "waiting" : blind ? "pending" : "done",
    },
    {
      id: "costs",
      label: costs ? `Look at what costs it (${costs})` : "Nothing costs it",
      state: costs ? "waiting" : blind ? "pending" : "done",
    },
    {
      id: "notes",
      label: notes ? `Read what is worth knowing (${notes})` : "Nothing else to read",
      state: notes ? "waiting" : "done",
    },
  ];
  const done = steps.filter((s) => s.state === "done").length;
  return (
    <StepList
      label="Go-live check"
      steps={steps}
      labels={GO_LIVE_LABELS}
      summary={`Go-live check: ${done} of ${steps.length} done`}
    />
  );
}

/** Where an `open_page` fix is taken, from its page and target. */
type PageFix = { path: string[]; query?: Record<string, string>; action: string };

/**
 * Where an `open_page` fix is taken, by the page the engine names and what it
 * opens on: the one gesture there, opened on arrival, so the row's action is the
 * step rather than a page to look for it on. The pages are the contract's
 * (`ADVISOR_PAGES`), held to the engine's.
 */
const PAGE_FIXES: Readonly<Record<AdvisorPage, (target: string) => PageFix | null>> = {
  backups: () => ({
    path: ["settings", "backups"],
    query: { take: TAKE_BACKUP },
    action: "Take a backup",
  }),
  item: (key) => (key ? { path: ["worklet", key], action: `Open ${key}` } : null),
  seat: (handle) =>
    handle ? { path: ["company", "org", "seats", handle], action: "Open the agent" } : null,
};

/** A record's page and how a list names it; null for one that has no page. */
export function recordLink(
  r: AdvisorRecord,
): { href: string; label: string; external?: boolean } | null {
  switch (r.kind) {
    case "item":
      return { href: href(["worklet", r.id]), label: r.id };
    case "turn":
      return { href: href(["live", "turns", r.id]), label: `turn ${r.id.slice(0, 8)}` };
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

/** The records behind a row's findings, each once, as links; the rest counted. */
function RecordLinks({ findings }: { findings: readonly AdvisorFinding[] }) {
  const seen = new Set<string>();
  const records = findings
    .flatMap((f) => f.records ?? [])
    .filter((r) => {
      const key = `${r.kind}\u0000${r.id}`;
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
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
      {others > 0 && `${links.length > 0 ? ", and " : ""}${plural(others, "comment")}`}
    </Text>
  );
}

/**
 * A finding's quieter line under its title.
 *
 * WHAT TO DO, where it is a step rather than a change to send (outside the
 * engine, or on a page of this dashboard), is read without opening the row.
 * Elsewhere the row names what it is about, UNLESS THE TITLE ALREADY DOES: a
 * finding whose title names its subject would say it twice.
 */
export function findingMeta(f: AdvisorFinding): string | undefined {
  if (f.fix.kind === "external_step" || f.fix.kind === "open_page") return f.fix.sentence;
  const subject = f.subject.name ?? "";
  if (!subject || f.title.includes(subject)) return undefined;
  return subject;
}

/**
 * Findings that say the same thing (one class, one title, one why, one cost,
 * one kind of fix) are one row, in the order the first of each came.
 */
export function grouped(findings: readonly AdvisorFinding[]): AdvisorFinding[][] {
  const groups = new Map<string, AdvisorFinding[]>();
  for (const f of findings) {
    const key = [f.class, f.title, f.why, f.severity, f.fix.kind].join("\u0000");
    const group = groups.get(key);
    if (group) group.push(f);
    else groups.set(key, [f]);
  }
  return [...groups.values()];
}

/** How many subjects of a kind a row is about, in words. */
function subjectCount(kind: AdvisorFinding["subject"]["kind"], n: number): string {
  if (kind === "repository") return plural(n, "repository", "repositories");
  if (kind === "schedule") return plural(n, "schedule");
  return plural(n, "agent");
}

/**
 * A group as one row: its subjects named once (a count where several carry no
 * item), each one's items under it, and the first finding's action, so a
 * change reviewed one at a time leaves the rest in the row.
 */
function groupItem(
  group: readonly AdvisorFinding[],
  review: ((f: AdvisorFinding) => void) | null,
  assistant: AssistantControl | null,
): AttentionItem {
  const first = group[0]!;
  const item = findingItem(first, review && (() => review(first)), assistant, group);
  const names = group.flatMap((f) => f.names ?? []);
  if (group.length === 1 && names.length === 0 && !first.item) return item;
  // TWO SEATS SHARING A NAME stay two, told apart by their handles.
  const shared = new Set(
    group.map((f) => f.subject.name ?? "").filter((n, i, all) => n !== "" && all.indexOf(n) !== i),
  );
  const nameOf = (f: AdvisorFinding) =>
    shared.has(f.subject.name ?? "") && f.subject.seat
      ? `${f.subject.name} (${f.subject.seat})`
      : (f.subject.name ?? "");
  const notice = groupNotices(
    group.flatMap((f) => {
      const subjects = f.names?.length ? f.names : [nameOf(f)];
      return subjects.map((subject) => ({
        kind: "group",
        subject: subject || undefined,
        item: f.item,
        title: item.title,
      }));
    }),
    { count: (s) => subjectCount(first.subject.kind, s.length) },
  )[0]!;
  // THE META NAMES THE SUBJECT on a lone finding; with the subject before the title it would say it twice.
  const said =
    group.length > 1 || (notice.subject !== undefined && findingMeta(first) === first.subject.name);
  return {
    ...item,
    key: `${item.key}\u0000${group.length}`,
    subject: notice.subject,
    lines: notice.lines,
    meta: said ? undefined : item.meta,
  };
}

/** One finding as a row of the list: what is wrong, why, and its one action. */
function findingItem(
  f: AdvisorFinding,
  review: (() => void) | null,
  assistant: AssistantControl | null,
  group: readonly AdvisorFinding[] = [f],
): AttentionItem {
  const subject = f.subject.name ?? "";
  const meta = findingMeta(f);
  let action;
  if (review && assistant && f.fix.kind === "draft_with_assistant" && f.fix.prompt) {
    // A DRAFT IS ASKED OF ASK CREWLET, whose own preview binds the approval.
    const prompt = f.fix.prompt;
    action = assistant.unavailable ? (
      <Text as="span" tone="secondary">
        {assistant.unavailable.short}
      </Text>
    ) : (
      <Button size="small" onClick={() => assistant.ask(prompt)}>
        Draft with Ask Crewlet
      </Button>
    );
  } else if (review && f.fix.kind === "config_change" && f.fix.options.length > 0) {
    action = (
      <Button size="small" onClick={review}>
        Review the change
      </Button>
    );
  } else if (review && f.fix.kind === "atlas_act" && f.fix.act) {
    // AN ATLAS FIX IS SENT WITH THE ATLAS'S OWN TOOLS, one call per entry it names.
    action = (
      <Button size="small" onClick={review}>
        Review the fix
      </Button>
    );
  } else if (review && f.fix.kind === "external_step" && f.fix.url) {
    action = (
      <Link href={f.fix.url} external size="caption">
        {subject ? `Open ${subject}` : "Open it"}
      </Link>
    );
  } else if (review && f.fix.kind === "open_page" && f.fix.page && f.fix.page in PAGE_FIXES) {
    const page = PAGE_FIXES[f.fix.page as AdvisorPage](f.fix.target ?? "");
    action = page && (
      <Link href={href(page.path, page.query)} size="caption">
        {page.action}
      </Link>
    );
  } else if (review && f.fix.kind === "open_setting") {
    action = (
      <Link href={href(["settings", "config"])} size="caption">
        Open the settings
      </Link>
    );
  } else if (review && f.fix.kind === "external_step" && f.subject.kind === "integration") {
    // AN INTEGRATION'S STEP WITH NO PAGE NAMED is taken from its own screen.
    action = (
      <Link href={href(["company", "integrations"])} size="caption">
        Open integrations
      </Link>
    );
  }
  return {
    // BY HANDLE, never by name: two seats may share a display name.
    key: [f.class, f.path, f.subject.seat, f.subject.unit, f.subject.schedule].join("\u0000"),
    tone: TONE[f.severity],
    // THE ENGINE'S SENTENCES, drawn as every engine sentence is ([marked]): a
    // value in backticks is code and a recorded instant a time, never the
    // punctuation it travelled in.
    title: marked(f.title),
    meta: meta === undefined ? undefined : marked(meta),
    action,
    details: (
      <div className="col gap-1">
        <Text as="p">{marked(f.why)}</Text>
        {review && meta !== f.fix.sentence && <Text as="p">{marked(f.fix.sentence)}</Text>}
        {f.fix.kind === "open_setting" && f.fix.setting && (
          <Text as="p" tone="secondary">
            The setting: <InlineCode>{f.fix.setting}</InlineCode>
          </Text>
        )}
        <RecordLinks findings={group} />
      </div>
    ),
  };
}
