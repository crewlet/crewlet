/**
 * Settings › Diagnostics: the company's choice to report the engine's own
 * defects, what a report says, and what was reported (ADR-0101).
 *
 * The switch is `diagnostics.issue_reports.enabled` on the company document,
 * written as any setting is, so the same refusals apply. A WRITE APPLIES NOTHING:
 * each node applies the revision on its own tick, so the answer can lag the
 * write, and the switch holds the wanted state until the answer agrees.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import {
  Button,
  Callout,
  Card,
  CodeBlock,
  DescriptionList,
  Disclosure,
  InlineCode,
  Link,
  Modal,
  PageNote,
  Switch,
  TableDateCell,
  TableKeyCell,
  TableStatusCell,
  Text,
  type DescriptionListEntry,
} from "@crewlethq/ui";
import { BugGlyph, FileTextGlyph, ListChecksGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { QueryState } from "~/components/common.tsx";
import type {
  IssueReportInert,
  IssueReportRow,
  IssueReportsAnswer,
} from "~/protocol/issueReports.ts";
import { refusalWords } from "~/lib/ceilings.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { getConfig, savePatch } from "~/protocol/configWrite.ts";
import { uiletTone } from "~/ui/primitives.tsx";

/** The guide this screen sums up. */
const GUIDE = "https://docs.crewlet.ai/engine/latest/guides/engine-issue-reports/";

/** How often a screen reads the record when it is at rest, and while a node applies. */
const POLL_MS = 60_000;
const APPLYING_POLL_MS = 3_000;

/** A build that sends nothing, in the words an administrator can act on. */
const INERT_WORDS: Record<IssueReportInert, string> = {
  development:
    "This is a development build, so it sends nothing while the switch is on. Only a release or a clean module build reports, because a development build's defect is often its developer's own unmerged change.",
  modified:
    "This build was made from a tree with uncommitted changes, so it sends nothing while the switch is on. Only a release or a clean module build reports.",
  no_commit:
    "This build recorded no commit, so it sends nothing while the switch is on: a report could not say what to look at. `go install` builds record none.",
};

/** What a report holds, as the disclosure says it. */
const SENT: readonly DescriptionListEntry[] = [
  ["Defect", "Which one, from a fixed catalogue, and the engine function it was detected in"],
  ["Build", "The engine's version, commit, Go toolchain, operating system and architecture"],
  ["Coding CLI", "Its name and release, for a defect that involves one"],
  ["Topology", "How many nodes (capped), and how the stream, coordination and sandbox run"],
  ["Occurrence", "How many times (capped), and when, rounded down to the hour"],
];

/** What a report's type has no field for. */
const NEVER_SENT: readonly DescriptionListEntry[] = [
  ["People", "Names and handles, of people, seats or units"],
  ["Work", "Item titles or keys, prompts, and anything a model or a tool said"],
  ["Failures", "Error messages, stacks, paths, hosts, addresses and URLs"],
  ["Records", "Ids of your seats, turns, items or messages, and config values"],
];

/** Lines the two lists up with each other. */
const TERM_WIDTH = "7rem";

const STATUS_WORDS: Record<
  IssueReportRow["status"],
  { label: string; glyph: string; tone: "positive" | "critical" | "neutral"; title: string }
> = {
  sending: {
    label: "Sending",
    glyph: "○",
    tone: "neutral",
    title: "A node is sending this report now",
  },
  filed: {
    label: "Filed",
    glyph: "●",
    tone: "positive",
    title: "The relay opened an issue for it",
  },
  duplicate: {
    label: "Duplicate",
    glyph: "●",
    tone: "neutral",
    title: "The issue was already open, so nothing new was filed",
  },
  failed: {
    label: "Failed",
    glyph: "✕",
    tone: "critical",
    title: "The relay never accepted it. It may go again after a day",
  },
};

/** The setting is the company's own, never a lead's. */
const COMPANY = { kind: "company" } as const;

function pretty(payload: unknown): string {
  return JSON.stringify(payload, null, 2);
}

export function Diagnostics() {
  const [wanted, setWanted] = useState<boolean | null>(null);
  const { data, loading, error, refusal, detail, refetch } = useQuery("issue_reports", undefined, {
    pollMs: wanted === null ? POLL_MS : APPLYING_POLL_MS,
  });
  useEffect(() => {
    if (data && wanted === data.enabled) setWanted(null);
  }, [data, wanted]);
  return (
    <>
      <PageNote>
        Opt in to tell the project when this engine detects a defect in itself. Reports are
        anonymous and hold nothing of your company&rsquo;s.{" "}
        <Link href={GUIDE} target="_blank" rel="noreferrer" size="inherit">
          How it works
        </Link>
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && (
          <>
            <ReportsSwitch answer={data} wanted={wanted} onWanted={setWanted} onSaved={refetch} />
            <ReportsList answer={data} />
            <ReportPreview answer={data} />
          </>
        )}
      </QueryState>
    </>
  );
}

/** The setting, what it sends and never sends, and a build that will not send. */
function ReportsSwitch({
  answer,
  wanted,
  onWanted,
  onSaved,
}: {
  answer: IssueReportsAnswer;
  wanted: boolean | null;
  onWanted: (on: boolean | null) => void;
  onSaved: () => void;
}) {
  const access = useConfigWriteAccess();
  const [saving, setSaving] = useState(false);
  const [refused, setRefused] = useState("");
  const inflight = useRef<AbortController | null>(null);
  useEffect(() => () => inflight.current?.abort(), []);

  const write = useCallback(
    (on: boolean) => {
      inflight.current?.abort();
      const controller = new AbortController();
      inflight.current = controller;
      const { signal } = controller;
      setSaving(true);
      setRefused("");
      void (async () => {
        try {
          const read = await getConfig(signal);
          if (read.kind !== "document") {
            setRefused(refusalWords(read, COMPANY).message);
            return;
          }
          const outcome = await savePatch(
            { diagnostics: { issue_reports: { enabled: on } } },
            read.etag,
            on ? "Turned engine issue reports on" : "Turned engine issue reports off",
            signal,
          );
          if (outcome.kind !== "saved") {
            setRefused(
              outcome.kind === "valid"
                ? "The engine checked the change but stored nothing. Try again."
                : refusalWords(outcome, COMPANY).message,
            );
            return;
          }
          onWanted(on);
          onSaved();
        } catch (err) {
          if (!signal.aborted) {
            setRefused(err instanceof Error ? err.message : "The engine could not be reached.");
          }
        } finally {
          if (!signal.aborted) setSaving(false);
        }
      })();
    },
    [onSaved, onWanted],
  );

  const shown = wanted ?? answer.enabled;
  return (
    <Card padding="none">
      <Card.Header icon={<BugGlyph size="sm" />}>
        <Card.Title>Issue reports</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-2">
        <Switch
          framed
          label="Report engine defects to the project"
          description="Off by default. Each report is filed as a public, anonymous issue on crewlet/crewlet, at most once a week per defect."
          checked={shown}
          disabled={saving}
          disabledReason={access.can ? undefined : access.reason}
          onCheckedChange={write}
        />
        {refused && (
          <Callout variant="danger" title="The change was not saved">
            {refused}
          </Callout>
        )}
        {shown && answer.inert && (
          <Callout variant="warning" title="This build sends no reports">
            {INERT_WORDS[answer.inert]}
          </Callout>
        )}
        <Disclosure title="What is sent, and what never is" defaultOpen={!answer.enabled}>
          <div className="col gap-2">
            <Text as="p">
              A report is a typed record, sent to <InlineCode>{answer.relay}</InlineCode>, which
              files the issue. Your engine holds no GitHub credential.
            </Text>
            <Text as="h3" variant="label">
              What is sent
            </Text>
            <DescriptionList dense termWidth={TERM_WIDTH} items={SENT} />
            <Text as="h3" variant="label">
              What never is
            </Text>
            <DescriptionList dense termWidth={TERM_WIDTH} items={NEVER_SENT} />
          </div>
        </Disclosure>
      </Card.Body>
    </Card>
  );
}

/** What this company reported, each with the exact bytes it sent. */
function ReportsList({ answer }: { answer: IssueReportsAnswer }) {
  const now = useNow();
  const [opened, setOpened] = useState<IssueReportRow | null>(null);
  return (
    <Card padding="none">
      <Card.Header icon={<ListChecksGlyph size="sm" />} count={answer.reports.length}>
        <Card.Title>Reports</Card.Title>
      </Card.Header>
      <DataGrid<IssueReportRow>
        name="reports"
        aria-label="Reports this company has sent"
        rows={answer.reports}
        rowKey={(r) => r.fingerprint}
        defaultSort="-first_sent"
        isFailed={(r) => r.status === "failed"}
        empty={{
          title: "No reports yet",
          hint: "A report appears here when a defect crosses its threshold with reporting on.",
          icon: "list-checks",
        }}
        columns={[
          {
            key: "title",
            header: "Defect",
            sortValue: (r) => r.title,
            cell: (r) => (
              <span className="truncate" title={r.title}>
                {r.title}
              </span>
            ),
          },
          {
            key: "fingerprint",
            header: "Fingerprint",
            shrink: true,
            drop: 1,
            sortValue: (r) => r.fingerprint,
            cell: (r) => <TableKeyCell value={r.fingerprint} text={r.fingerprint.slice(0, 12)} />,
          },
          {
            key: "first_sent",
            align: "right",
            header: "First sent",
            shrink: true,
            sortValue: (r) => r.first_sent,
            cell: (r) => <TableDateCell value={r.first_sent} now={now} />,
          },
          {
            key: "status",
            header: "Status",
            shrink: true,
            phoneLead: true,
            sortValue: (r) => r.status,
            cell: (r) => {
              const words = STATUS_WORDS[r.status];
              return (
                <TableStatusCell
                  glyph={words.glyph}
                  label={words.label}
                  title={`${words.title}. Last sent ${fmtDateTime(r.last_sent)}`}
                  tone={uiletTone(words.tone)}
                />
              );
            },
          },
          {
            key: "issue",
            header: "Issue",
            shrink: true,
            sortValue: (r) => r.issue,
            cell: (r) => <IssueCell issue={r.issue} title={r.title} onOpen={() => setOpened(r)} />,
          },
        ]}
      />
      <Modal
        open={opened !== null}
        onClose={() => setOpened(null)}
        title="What this report sent"
        subtitle={opened?.title}
      >
        {opened && (
          <CodeBlock plain selectable label="The report, as JSON" code={pretty(opened.payload)} />
        )}
      </Modal>
    </Card>
  );
}

/** The relay's address for a report where it named one, and the report's own bytes. */
function IssueCell({ issue, title, onOpen }: { issue: string; title: string; onOpen: () => void }) {
  const number = issue.split("/").at(-1);
  return (
    <span className="row gap-2">
      {issue && (
        <Link href={issue} target="_blank" rel="noreferrer">
          #{number}
        </Link>
      )}
      <Button
        variant="ghost"
        size="small"
        onClick={onOpen}
        aria-label={`What was sent for: ${title}`}
      >
        View
      </Button>
    </span>
  );
}

/** The exact record a report of each class would send. */
function ReportPreview({ answer }: { answer: IssueReportsAnswer }) {
  return (
    <Card padding="none">
      <Card.Header icon={<FileTextGlyph size="sm" />}>
        <Card.Title>What a report looks like</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-2">
        <Text as="p" tone="secondary">
          Built exactly as a real report is, with this engine&rsquo;s build and topology. The facts
          are examples.
        </Text>
        {answer.previews.map((preview) => (
          <Disclosure key={preview.class} title={preview.title}>
            <div className="col gap-2">
              <Text as="p">{preview.summary}</Text>
              <CodeBlock
                plain
                selectable
                label={`A report of ${preview.class}, as JSON`}
                code={pretty(preview.payload)}
              />
            </div>
          </Disclosure>
        ))}
      </Card.Body>
    </Card>
  );
}
