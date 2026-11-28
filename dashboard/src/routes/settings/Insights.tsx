/**
 * Settings › Diagnostics' usage insights (ADR-0119): the company's consent to
 * share weekly records of how its agents work, the records waiting for review,
 * and every record kept, each with its exact bytes.
 *
 * The consent is `diagnostics.insights` on the company document, written
 * through the one setting write (`lib/useSettingWrite.ts`), as any setting
 * is; a review answer is `POST /diagnostics/insights/decisions`.
 */

import { useCallback, useEffect, useState } from "react";
import {
  Button,
  Callout,
  Card,
  ChoiceList,
  CodeBlock,
  DescriptionList,
  Disclosure,
  InlineCode,
  TableStatusCell,
  Text,
  type DescriptionListEntry,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { QueryState } from "~/components/common.tsx";
import { fmtDateTime } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { saysSomething, SettingWriteStatus } from "~/components/SettingWriteStatus.tsx";
import { insightsPlan, type InsightsChange } from "~/lib/diagnosticsSettings.ts";
import { useSettingWrite } from "~/lib/useSettingWrite.ts";
import type {
  InsightClass,
  InsightRow,
  InsightsAnswer,
  InsightsShare,
  InsightStatus,
} from "~/protocol/insights.ts";
import { RestError, rest } from "~/protocol/rest.ts";
import { uiletTone } from "~/ui/primitives.tsx";

/** How often the record is read at rest, and while a node applies a change. */
const POLL_MS = 60_000;
const APPLYING_POLL_MS = 3_000;

/** How a change that met a newer document ends. */
const REREAD = "Reload to see the setting as it is now.";
const TERM_WIDTH = "7.5rem";

/** The three answers to "share?", in the order a person weighs them. */
const SHARE_WORDS: Record<"off" | "review" | "auto", { label: string; detail: string }> = {
  off: { label: "Don't share", detail: "Nothing is kept or sent." },
  review: {
    label: "Review each record first",
    detail: "Each week's record waits here until you send it or decline it.",
  },
  auto: {
    label: "Send automatically",
    detail: "Every eligible record is sent, each at its own random time within a day.",
  },
};

/** What a record holds, in plain words. */
const SENT: readonly DescriptionListEntry[] = [
  [
    "Which pattern",
    "One class from a fixed list, such as agents woken for turns that changed nothing",
  ],
  ["Which week", "The calendar week, and nothing finer"],
  ["Version", "The engine's release, or main for any other public build"],
  ["How much", "At most four facts, each a range such as 35-49% or 2-3 agents, never a raw count"],
];

/** What a record never holds, in plain words. */
const NEVER_SENT: readonly DescriptionListEntry[] = [
  ["Names", "Any role, agent, person, team, server or host"],
  ["Work content", "Item keys, titles, or any text an agent, a tool or a person wrote"],
  ["Exact numbers", "Raw counts, durations, or any time finer than the week"],
  ["Setup", "The commit, how the engine is deployed, or any of your settings"],
];

const STATUS_WORDS: Record<
  InsightStatus,
  { label: string; glyph: string; tone: "positive" | "critical" | "neutral"; title: string }
> = {
  observed: {
    label: "Watching",
    glyph: "○",
    tone: "neutral",
    title: "The pattern was seen this week; it is eligible once it holds two days in a row",
  },
  queued: { label: "Waiting for you", glyph: "◆", tone: "neutral", title: "Send it or decline it" },
  scheduled: { label: "Will send", glyph: "○", tone: "neutral", title: "Sent at its own time" },
  sending: { label: "Sending", glyph: "○", tone: "neutral", title: "Being sent right now" },
  sent: { label: "Sent", glyph: "●", tone: "positive", title: "The relay took it" },
  failed: { label: "Didn't send", glyph: "✕", tone: "critical", title: "The relay never took it" },
  declined: { label: "Not sent", glyph: "●", tone: "neutral", title: "An admin declined it" },
};

function pretty(value: unknown): string {
  return JSON.stringify(value, null, 2);
}

/** A record a class would send, with each fact at its first range. */
export function sampleRecord(answer: InsightsAnswer, c: InsightClass): Record<string, unknown> {
  return {
    schema: answer.schema,
    class: c.class,
    week: "2030-W02",
    version: "v1.4.0",
    facts: Object.fromEntries(c.facts.map((f) => [f.key, f.values[0] ?? ""])),
  };
}

export function Insights() {
  const [wanted, setWanted] = useState<InsightsShare | null>(null);
  const { data, loading, error, refusal, detail, refetch } = useQuery(
    "diagnostics_insights",
    undefined,
    { pollMs: wanted === null ? POLL_MS : APPLYING_POLL_MS },
  );
  useEffect(() => {
    if (data && wanted !== null && shareOf(data.share) === wanted) setWanted(null);
  }, [data, wanted]);
  return (
    <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
      {data && (
        <>
          <Consent answer={data} wanted={wanted} onWanted={setWanted} onSaved={refetch} />
          {shareOf(data.share) === "review" && <ReviewQueue answer={data} onDecided={refetch} />}
          <Records answer={data} />
        </>
      )}
    </QueryState>
  );
}

/** An unset consent is off. */
function shareOf(share: InsightsShare): "off" | "review" | "auto" {
  return share === "" ? "off" : share;
}

/** The consent, what a record sends and never sends, and a sample of each class. */
function Consent({
  answer,
  wanted,
  onWanted,
  onSaved,
}: {
  answer: InsightsAnswer;
  wanted: InsightsShare | null;
  onWanted: (share: InsightsShare | null) => void;
  onSaved: () => void;
}) {
  const access = useConfigWriteAccess();
  // THE ONE SETTING WRITE: checked by the engine first, stopped on a warning
  // the change introduces, and watched until this node applies it.
  const write = useSettingWrite({ reread: REREAD, onApplied: onSaved });
  const { state } = write;
  // A CHANGE THAT CAME TO NOTHING lets go of the wanted choice, so the list
  // goes back to what the engine holds.
  useEffect(() => {
    if (state.kind === "refused" || state.kind === "idle") onWanted(null);
  }, [state.kind, onWanted]);
  const shown = wanted ?? shareOf(answer.share);
  // CHOOSING TO SHARE ACCEPTS THIS BUILD'S CATALOGUE, the one listed below.
  const choose = (value: string) => {
    if (write.busy) return;
    const share = value as "off" | "review" | "auto";
    const change: InsightsChange = share === "off" ? { share } : { share, accepted: answer.schema };
    onWanted(share);
    write.submit(
      insightsPlan(change, `Set usage insights to ${SHARE_WORDS[share].label.toLowerCase()}`),
    );
  };
  // THE ENGINE SHARES ONLY ON THE LIST ACCEPTED, so an older one, or a newer
  // one accepted on another build, shares nothing.
  const stale = shareOf(answer.share) !== "off" && answer.accepted !== answer.schema;
  return (
    <Card padding="none">
      <Card.Header>
        <Card.Title>Usage insights to Crewlet</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-2">
        <Text as="p">
          Share small weekly records of the patterns the advisor finds in how your agents work, so
          the Crewlet team learns which ones are common. It&rsquo;s separate from bug reports.
        </Text>
        <ChoiceList
          label="Sharing"
          value={shown}
          disabled={!access.can}
          onChange={choose}
          options={(["off", "review", "auto"] as const).map((v) => ({
            value: v,
            label: SHARE_WORDS[v].label,
            detail: SHARE_WORDS[v].detail,
          }))}
        />
        {!access.can && access.reason && (
          <Text as="p" tone="secondary">
            {access.reason}
          </Text>
        )}
        {stale && (
          <Callout
            variant="warning"
            title="Nothing is shared until you accept this list"
            action={
              <Button
                size="small"
                disabled={!access.can}
                onClick={() =>
                  write.submit(
                    insightsPlan({ accepted: answer.schema }, "Accepted the usage insights list"),
                  )
                }
              >
                Accept this list
              </Button>
            }
          >
            This engine&rsquo;s list of patterns differs from the one you accepted.
          </Callout>
        )}
        <SettingWriteStatus
          write={write}
          unchanged="Already so."
          onReload={onSaved}
          answersWarning
        />
        <Disclosure title="What a record holds, and never holds" defaultOpen={shown === "off"}>
          <div className="col gap-2">
            <Text as="p">
              Every record is a fixed form with no free text, sent to{" "}
              <InlineCode close=",">{answer.relay}</InlineCode> and only from a public release or
              image.
            </Text>
            <Text as="h3" variant="label">
              A record holds
            </Text>
            <DescriptionList dense termWidth={TERM_WIDTH} items={SENT} />
            <Text as="h3" variant="label">
              A record never holds
            </Text>
            <DescriptionList dense termWidth={TERM_WIDTH} items={NEVER_SENT} />
          </div>
        </Disclosure>
        <Disclosure title={`See a sample record (${answer.catalogue.classes.length} patterns)`}>
          <div className="col gap-2">
            {answer.catalogue.classes.map((c) => (
              <Disclosure key={c.class} title={c.title}>
                <div className="col gap-2">
                  <Text as="p">{c.summary}</Text>
                  <CodeBlock
                    plain
                    selectable
                    language="json"
                    label={`Sample record: ${c.title}`}
                    code={pretty(sampleRecord(answer, c))}
                  />
                </div>
              </Disclosure>
            ))}
          </div>
        </Disclosure>
      </Card.Body>
    </Card>
  );
}

/** The records waiting for an administrator, each sent or declined here. */
function ReviewQueue({ answer, onDecided }: { answer: InsightsAnswer; onDecided: () => void }) {
  const access = useConfigWriteAccess();
  const queued = answer.records.filter((r) => r.status === "queued");
  const [busy, setBusy] = useState("");
  const [refused, setRefused] = useState("");
  const [opened, setOpened] = useState<InsightRow | null>(null);
  // ALWAYS SENDING A PATTERN is a change to the company document, made
  // through the one setting write like the consent above it.
  const write = useSettingWrite({ reread: REREAD, onApplied: onDecided });
  const [alwaysFor, setAlwaysFor] = useState<InsightRow | null>(null);
  const titleOf = useTitles(answer);

  const decide = async (r: InsightRow, choice: "send" | "decline"): Promise<boolean> => {
    setBusy(`${r.class}/${r.week}`);
    setRefused("");
    try {
      await rest.post("/diagnostics/insights/decisions", { class: r.class, week: r.week, choice });
      onDecided();
      return true;
    } catch (err) {
      setRefused(
        err instanceof RestError
          ? err.detail || err.hint || err.code || "The engine refused that."
          : String(err),
      );
      return false;
    } finally {
      setBusy("");
    }
  };
  // ALWAYS SENDING A CLASS SENDS THIS RECORD TOO: the person has said yes to
  // it, once the document holds the pattern (saved, or already so).
  const always = (r: InsightRow) => {
    if (write.busy) return;
    const classes = [...new Set([...answer.always, r.class])];
    setAlwaysFor(r);
    write.submit(
      insightsPlan({ always: classes }, `Always send the ${titleOf(r.class)} usage insight`),
    );
  };
  const written = write.state.kind;
  useEffect(() => {
    if (!alwaysFor) return;
    if (written === "saved" || written === "unchanged") {
      setAlwaysFor(null);
      void decide(alwaysFor, "send");
    } else if (written === "refused" || written === "idle") {
      setAlwaysFor(null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [written, alwaysFor]);

  return (
    <Card padding="none">
      <Card.Header count={queued.length}>
        <Card.Title>Waiting for your review</Card.Title>
      </Card.Header>
      {refused && (
        <Card.Body padding="md">
          <Callout variant="danger" title="Couldn't do that">
            {refused}
          </Callout>
        </Card.Body>
      )}
      {saysSomething(write.state) && (
        <Card.Body padding="md">
          <SettingWriteStatus
            write={write}
            unchanged="Already so."
            onReload={onDecided}
            answersWarning
          />
        </Card.Body>
      )}
      <DataGrid<InsightRow>
        name="insights-review"
        aria-label="Usage insights waiting for your review"
        rows={queued}
        rowKey={(r) => `${r.class}/${r.week}`}
        empty={{
          size: "inline",
          title: "Nothing to review",
          hint: "A record waits here once a pattern holds two days in a row of one week.",
        }}
        columns={[
          {
            key: "class",
            header: "Pattern",
            overflow: "truncate",
            floor: "12rem",
            cell: (r) => titleOf(r.class),
          },
          { key: "week", header: "Week", shrink: true, cell: (r) => r.week },
          {
            // FOUR CONTROLS NEED THEIR WHOLE WIDTH: a shrink column is capped at a
            // fifth of the grid and clipped the last two. Where they cannot fit, the
            // grid stacks into cards and the controls wrap.
            key: "decide",
            header: "Your answer",
            width: "max-content",
            cell: (r) => {
              const pending = busy === `${r.class}/${r.week}`;
              const blocked = !access.can || busy !== "";
              return (
                <div className="row wrap gap-1">
                  <Button variant="ghost" size="small" onClick={() => setOpened(r)}>
                    View
                  </Button>
                  <Button
                    size="small"
                    variant="primary"
                    loading={pending}
                    disabled={blocked}
                    onClick={() => void decide(r, "send")}
                  >
                    Send
                  </Button>
                  <Button size="small" disabled={blocked} onClick={() => void decide(r, "decline")}>
                    Don&rsquo;t send
                  </Button>
                  <Button
                    size="small"
                    variant="ghost"
                    disabled={blocked}
                    title="Send this record, and send this pattern's records without review from now on"
                    onClick={() => always(r)}
                  >
                    Always send this pattern
                  </Button>
                </div>
              );
            },
          },
        ]}
      />
      <RecordModal
        row={opened}
        title={opened ? titleOf(opened.class) : ""}
        onClose={() => setOpened(null)}
      />
    </Card>
  );
}

/** Every record the company keeps, each with its exact bytes. */
function Records({ answer }: { answer: InsightsAnswer }) {
  const [opened, setOpened] = useState<InsightRow | null>(null);
  const titleOf = useTitles(answer);
  return (
    <Card padding="none">
      <Card.Header count={answer.records.length}>
        <Card.Title>Usage insights kept</Card.Title>
      </Card.Header>
      <DataGrid<InsightRow>
        name="insights"
        aria-label="Usage insight records"
        rows={answer.records}
        rowKey={(r) => `${r.class}/${r.week}`}
        defaultSort="-week"
        isFailed={(r) => r.status === "failed"}
        empty={{
          size: "inline",
          title: "No records yet",
          hint: "With sharing on, a record shows up here once the advisor finds a pattern.",
        }}
        columns={[
          {
            key: "class",
            header: "Pattern",
            sortValue: (r) => titleOf(r.class),
            cell: (r) => <span className="truncate">{titleOf(r.class)}</span>,
          },
          {
            key: "week",
            header: "Week",
            shrink: true,
            sortValue: (r) => r.week,
            cell: (r) => r.week,
          },
          {
            key: "status",
            header: "Status",
            shrink: true,
            phoneLead: true,
            sortValue: (r) => r.status,
            cell: (r) => {
              const words = STATUS_WORDS[r.status];
              const when = r.send_at ? `. Sends ${fmtDateTime(r.send_at)}` : "";
              const relay = r.answer ? `. The relay answered ${r.answer}` : "";
              return (
                <TableStatusCell
                  glyph={words.glyph}
                  label={words.label}
                  title={`${words.title}${when}${relay}`}
                  tone={uiletTone(words.tone)}
                />
              );
            },
          },
          {
            key: "record",
            header: "Details",
            shrink: true,
            cell: (r) =>
              r.record === undefined ? null : (
                <Button
                  variant="ghost"
                  size="small"
                  onClick={() => setOpened(r)}
                  aria-label={`See the record for ${titleOf(r.class)}, ${r.week}`}
                >
                  View
                </Button>
              ),
          },
        ]}
      />
      <RecordModal
        row={opened}
        title={opened ? titleOf(opened.class) : ""}
        onClose={() => setOpened(null)}
      />
    </Card>
  );
}

/** One record, exactly as it is or would be sent. */
function RecordModal({
  row,
  title,
  onClose,
}: {
  row: InsightRow | null;
  title: string;
  onClose: () => void;
}) {
  return (
    <Modal open={row !== null} onClose={onClose} size="lg" title="The record" subtitle={title}>
      {row && (
        <CodeBlock
          plain
          selectable
          language="json"
          label={`The record for ${row.week}, exactly as it is sent`}
          code={pretty(row.record ?? {})}
        />
      )}
    </Modal>
  );
}

/** A class's title from the catalogue, its id where this build does not name it. */
function useTitles(answer: InsightsAnswer): (cls: string) => string {
  return useCallback(
    (cls: string) => answer.catalogue.classes.find((c) => c.class === cls)?.title ?? cls,
    [answer.catalogue.classes],
  );
}
