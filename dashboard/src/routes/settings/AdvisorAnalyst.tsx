/** Settings › Advisor's analyst (ADR-0157): problems no rule names, found by a run on the
 *  company's own models, listed only once the engine has checked every record and number each
 *  cites. Kept apart from the rule-based list. */

import { useState } from "react";
import {
  AttentionList,
  Button,
  Callout,
  EmptyState,
  InlineCode,
  Link,
  Text,
  type AttentionItem,
} from "@crewlethq/ui";
import { AgoTime } from "~/components/AgoTime.tsx";
import { FixDialog } from "~/components/advisorFix.tsx";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { unreadSentences } from "~/lib/advisorUnread.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type {
  AdvisorAnalysisAnswer,
  AdvisorFinding,
  AnalystFinding,
  AnalystVerdict,
} from "~/protocol/advisor.ts";
import { RestError, rest } from "~/protocol/rest.ts";
import { findingItem } from "./Advisor.tsx";

/** How often the record is read: often enough to see a run end. */
const POLL_MS = 15_000;

/** Each answer, in the words its button says. */
const VERDICTS: readonly { verdict: AnalystVerdict; label: string }[] = [
  { verdict: "useful", label: "Useful" },
  { verdict: "wrong", label: "Wrong" },
  { verdict: "known", label: "Already known" },
];

/** What each way a run ended says, where it did not end clean. */
const ENDED: Readonly<Record<string, { title: string; tone: "warning" | "danger" }>> = {
  ceiling: { title: "The last run stopped at its token ceiling", tone: "warning" },
  refused: { title: "The analyst's model declined the last run", tone: "warning" },
  failed: { title: "The last run did not finish", tone: "danger" },
};

export function AdvisorAnalyst() {
  const { data, loading, error, refusal, detail, refetch } = useQuery(
    "advisor_analysis",
    undefined,
    { pollMs: POLL_MS },
  );
  return (
    <section aria-label="Found by the analyst" className="col gap-2">
      <Text as="h3" variant="label">
        Found by the analyst
      </Text>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && <AnalystBody answer={data} refetch={refetch} />}
      </QueryState>
    </section>
  );
}

function AnalystBody({ answer, refetch }: { answer: AdvisorAnalysisAnswer; refetch: () => void }) {
  const [refused, setRefused] = useState("");
  const [busy, setBusy] = useState(false);
  const [open, setOpen] = useState<AdvisorFinding | null>(null);
  const last = answer.last;
  const running = last?.status === "running";

  const send = async (call: () => Promise<unknown>) => {
    setBusy(true);
    setRefused("");
    try {
      await call();
      refetch();
    } catch (err) {
      setRefused(
        err instanceof RestError
          ? err.detail || err.hint || err.code || "The engine refused that."
          : String(err),
      );
    } finally {
      setBusy(false);
    }
  };

  if (!answer.on) {
    return (
      <Callout
        variant="neutral"
        title="The analyst is off"
        action={
          <Link href={href(["settings", "config"])} size="caption">
            Open the settings
          </Link>
        }
      >
        <Text as="p">
          It reads the week&rsquo;s records on your own models, under a token ceiling of its own,
          and lists only what it can cite. Name a model under{" "}
          <InlineCode>advisor.analyst.llm</InlineCode> to turn it on
          {answer.suggest ? (
            <>
              , such as Ask Crewlet&rsquo;s <InlineCode close=".">{answer.suggest}</InlineCode>
            </>
          ) : (
            "."
          )}
        </Text>
      </Callout>
    );
  }

  const runNow = (
    <Button
      size="small"
      disabled={busy || running}
      onClick={() => void send(() => rest.post("/advisor/analysis", {}))}
    >
      {running ? "Running" : "Run now"}
    </Button>
  );
  const schedule =
    answer.cadence === "off"
      ? "It runs only when you press Run now."
      : `It runs ${answer.cadence} at ${String(answer.hour).padStart(2, "0")}:00.`;

  return (
    <div className="col gap-2">
      <div className="row gap-2">
        <Text as="p" tone="secondary">
          {last ? (
            <>
              Last run <AgoTime value={last.started} /> on{" "}
              <InlineCode close=",">{last.model}</InlineCode>{" "}
              {plural(last.input_tokens + last.output_tokens, "token")} of{" "}
              {last.ceiling.toLocaleString()}. {schedule}
            </>
          ) : (
            <>No run yet. {schedule}</>
          )}
        </Text>
        {runNow}
      </div>
      {refused && (
        <Callout variant="danger" title="Couldn't do that">
          {refused}
        </Callout>
      )}
      {last && ENDED[last.status] && (
        <Callout variant={ENDED[last.status]!.tone} title={ENDED[last.status]!.title}>
          {last.reason}
        </Callout>
      )}
      {last && answer.stale && !running && (
        <Callout variant="warning" title="The last run is out of date">
          Its findings are from more than two of its periods ago; run it again to read this week.
        </Callout>
      )}
      {last && last.unread.length > 0 && (
        <Callout variant="warning" title="The last run couldn't read everything">
          {unreadSentences(last.unread).join(" ")}
        </Callout>
      )}
      {answer.findings.length > 0 ? (
        <AttentionList
          items={answer.findings.map((f) =>
            analystItem(
              f,
              setOpen,
              (verdict) =>
                void send(() =>
                  rest.post("/advisor/analysis/answers", { fingerprint: f.fingerprint, verdict }),
                ),
            ),
          )}
          labels={{
            title: "What the analyst found",
            tones: { danger: "Blocks", warning: "Costs", info: "Worth knowing" },
          }}
        />
      ) : (
        last?.status === "done" &&
        !answer.stale &&
        last.unread.length === 0 && (
          <EmptyState
            size="compact"
            title="Nothing found"
            description="The last run found nothing the rules above do not already list."
          />
        )
      )}
      {last && <Dropped dropped={last.dropped} setAside={answer.set_aside} />}
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
    </div>
  );
}

/** What the last run submitted that is not listed, counted. */
function Dropped({ dropped, setAside }: { dropped: Record<string, number>; setAside: number }) {
  const n = Object.values(dropped).reduce((a, b) => a + b, 0);
  if (n === 0 && setAside === 0) return null;
  const parts = [];
  if (n > 0) parts.push(`${plural(n, "finding")} left out because the engine could not verify it`);
  if (setAside > 0)
    parts.push(`${plural(setAside, "finding")} you answered wrong or already known`);
  return (
    <Text as="p" tone="secondary">
      Not listed: {parts.join("; ")}.
    </Text>
  );
}

/** One analyst finding as a row: the rule list's row, with its answer. */
function analystItem(
  f: AnalystFinding,
  review: (f: AdvisorFinding) => void,
  answer: (v: AnalystVerdict) => void,
): AttentionItem {
  const item = findingItem(f, () => review(f), null);
  return {
    ...item,
    key: f.fingerprint,
    details: (
      <div className="col gap-1">
        {item.details}
        <div className="row gap-1" role="group" aria-label="Your answer">
          {VERDICTS.map((v) => (
            <Button
              key={v.verdict}
              size="small"
              variant={f.answer === v.verdict ? "primary" : "ghost"}
              aria-pressed={f.answer === v.verdict}
              onClick={() => answer(v.verdict)}
            >
              {v.label}
            </Button>
          ))}
        </div>
      </div>
    ),
  };
}
