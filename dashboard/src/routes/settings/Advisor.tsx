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

import { useState } from "react";
import {
  AttentionList,
  Button,
  Callout,
  EmptyState,
  InlineCode,
  Link,
  PageNote,
  StepList,
  Text,
  type AttentionItem,
  type AttentionTone,
  type StepListStep,
} from "@crewlethq/ui";
import { FixDialog } from "~/components/advisorFix.tsx";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import type { AdvisorFinding, AdvisorSeverity } from "~/protocol/advisor.ts";

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
  const owner = data?.findings.filter((f) => f.actor === "owner") ?? [];
  const engine = data?.findings.filter((f) => f.actor === "engine") ?? [];
  return (
    <>
      <PageNote>
        What your company&rsquo;s setup costs it, why, and the change that fixes each. You review
        every change before it is made; the engine never makes one on its own.
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && data.unread.includes("integrations") && (
          <Callout variant="warning" title="Some of the setup couldn't be read">
            This server couldn&rsquo;t read the integrations&rsquo; last check, so anything they
            need from you is missing below.
          </Callout>
        )}
        {data && <GoLiveCheck owner={owner} unread={data.unread} />}
        {data &&
          (owner.length === 0 ? (
            <EmptyState
              size="compact"
              title="Nothing to change"
              description="Your company's setup raises nothing worth changing right now."
            />
          ) : (
            <AttentionList
              items={owner.map((f) => findingItem(f, () => setOpen(f)))}
              labels={OWNER_LABELS}
            />
          ))}
        {engine.length > 0 && (
          <AttentionList items={engine.map((f) => findingItem(f, null))} labels={ENGINE_LABELS} />
        )}
      </QueryState>
      {open && (
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
  const count = (severity: AdvisorSeverity) => owner.filter((f) => f.severity === severity).length;
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

/** One finding as a row of the list: what is wrong, why, and its one action. */
function findingItem(f: AdvisorFinding, review: (() => void) | null): AttentionItem {
  const subject = f.subject.name ?? "";
  let action;
  if (review && f.fix.kind === "config_change" && f.fix.options.length > 0) {
    action = (
      <Button size="small" onClick={review}>
        Review the change
      </Button>
    );
  } else if (review && f.fix.kind === "external_step" && f.fix.url) {
    action = (
      <Link href={f.fix.url} external size="caption">
        {subject ? `Open ${subject}` : "Open it"}
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
    title: f.title,
    // WHAT TO DO, where it happens outside the engine, is read without opening
    // the row; elsewhere the row names what it is about.
    meta: f.fix.kind === "external_step" ? f.fix.sentence : subject || undefined,
    action,
    details: (
      <div className="col gap-1">
        <Text as="p">{f.why}</Text>
        {review && f.fix.kind !== "external_step" && <Text as="p">{f.fix.sentence}</Text>}
        {f.fix.kind === "open_setting" && f.fix.setting && (
          <Text as="p" tone="secondary">
            The setting: <InlineCode>{f.fix.setting}</InlineCode>
          </Text>
        )}
      </div>
    ),
  };
}
