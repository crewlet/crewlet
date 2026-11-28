/**
 * Settings › Diagnostics: the company's choice to report the engine's own
 * defects, what a report says, and what was reported (ADR-0101), and its
 * usage insights ([Insights], ADR-0119).
 *
 * The switch is `diagnostics.issue_reports.enabled` on the company document,
 * written through the one setting write (`lib/useSettingWrite.ts`): checked by
 * the engine first, stopped on a warning it introduces, and watched until this
 * node applies it. A WRITE APPLIES NOTHING by itself: each node applies the
 * revision on its own tick, so the answer can lag the write, and the switch
 * holds the wanted state until the answer agrees.
 */

import { useEffect, useState } from "react";
import {
  Button,
  Callout,
  Card,
  CodeBlock,
  CopyButton,
  DescriptionList,
  Disclosure,
  EmptyValue,
  InlineCode,
  Link,
  PageNote,
  Switch,
  TableDateCell,
  TableKeyCell,
  TableStatusCell,
  Text,
  type DescriptionListEntry,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { QueryState } from "~/components/common.tsx";
import { Insights } from "./Insights.tsx";
import type {
  IssueReportInert,
  IssueReportRow,
  IssueReportsAnswer,
} from "~/protocol/issueReports.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { SettingWriteStatus } from "~/components/SettingWriteStatus.tsx";
import { issueReportsPlan } from "~/lib/diagnosticsSettings.ts";
import { useSettingWrite } from "~/lib/useSettingWrite.ts";
import { marked } from "~/ui/Problems.tsx";
import { uiletTone } from "~/ui/primitives.tsx";

/** The guide this screen sums up. */
const GUIDE = "https://docs.crewlet.ai/engine/latest/guides/engine-issue-reports/";
/** Where a kept report goes: the project's private vulnerability report (SECURITY.md). */
const PRIVATE_REPORT = "https://github.com/crewlet/crewlet/security/advisories/new";

/** How often a screen reads the record when it is at rest, and while a node applies. */
const POLL_MS = 60_000;
const APPLYING_POLL_MS = 3_000;

/** Why this build sends nothing, in plain words. */
const INERT_WORDS: Record<IssueReportInert, string> = {
  development:
    "You're running a development build, so nothing is sent even with this on. Only official releases, the published images and clean builds send reports, because a bug in a development build is often just unfinished code.",
  modified:
    "This build has local code changes, so nothing is sent even with this on. Only official releases, the published images and clean builds send reports.",
  no_commit:
    "This build doesn't know which version of the code it came from, so nothing is sent even with this on: the Crewlet team couldn't tell where to look.",
};

/** What a report holds, in plain words. */
const SENT: readonly DescriptionListEntry[] = [
  [
    "Which bug",
    "Picked from a fixed list, plus the place in Crewlet's own code where it happened and, for a crash, up to five lines of Crewlet's code that led there",
  ],
  ["Your version", "Which version of the engine you run, and on what operating system"],
  ["Coding tool", "Its name and version, only when the bug involves it (for example, Claude Code)"],
  ["Your setup", "Roughly how the engine is set up, such as how many servers it runs on"],
  ["How often", "How many times it happened, and roughly when (to the hour)"],
];

/** What a report never holds, in plain words. */
const NEVER_SENT: readonly DescriptionListEntry[] = [
  ["People", "Anyone's name or handle, including your agents' and teams'"],
  ["Your work", "Task titles, prompts, or anything an agent or tool wrote"],
  [
    "Error details",
    "Error messages, logs, paths on your servers, server names, IP addresses or links",
  ],
  ["Your data", "Any ID from your workspace, and any of your settings"],
];

/** Lines the two lists up with each other. */
const TERM_WIDTH = "7.5rem";

const STATUS_WORDS: Record<
  IssueReportRow["status"],
  { label: string; glyph: string; tone: "positive" | "critical" | "neutral"; title: string }
> = {
  sending: {
    label: "Sending",
    glyph: "○",
    tone: "neutral",
    title: "This report is being sent right now",
  },
  filed: {
    label: "Reported",
    glyph: "●",
    tone: "positive",
    title: "A new GitHub issue was opened for this bug",
  },
  duplicate: {
    label: "Already reported",
    glyph: "●",
    tone: "neutral",
    title: "This bug already has an open issue, so no new one was opened",
  },
  failed: {
    label: "Didn't send",
    glyph: "✕",
    tone: "critical",
    title: "The report couldn't be delivered. The engine tries again after a day",
  },
  held: {
    label: "Kept private",
    glyph: "◆",
    tone: "neutral",
    title:
      "This crash was in code that people outside your company can reach, so it was kept here and not sent. You can report it privately",
  },
};

/** How a change that met a newer document ends. */
export const REREAD = "Reload to see the setting as it is now.";

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
        Anonymous reports the engine can send the Crewlet team, off until an admin turns them on.{" "}
        {/* A LINK INSIDE THE SENTENCE carries the underline register: the
            kit's link marks itself by its ink alone until hovered, which in
            the dark theme is 1.18:1 against the words around it. */}
        <Link href={GUIDE} external size="inherit" className="prose-link">
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
      <Insights />
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
  // THE ONE SETTING WRITE: checked by the engine first, stopped on a warning
  // the change introduces, and watched until this node applies it.
  const write = useSettingWrite({ reread: REREAD, onApplied: onSaved });
  const { state } = write;
  // A CHANGE THAT CAME TO NOTHING lets go of the wanted state, so the switch
  // goes back to what the engine holds.
  useEffect(() => {
    if (state.kind === "refused" || state.kind === "idle") onWanted(null);
  }, [state.kind, onWanted]);
  const press = (on: boolean) => {
    // A SAVE IN FLIGHT IGNORES A PRESS RATHER THAN DISABLING THE SWITCH: a
    // disabled input drops keyboard focus, and the next Space would go nowhere.
    if (write.busy) return;
    onWanted(on);
    write.submit(issueReportsPlan(on));
  };

  const shown = wanted ?? answer.enabled;
  return (
    <Card padding="none">
      <Card.Header>
        <Card.Title>Bug reports to Crewlet</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-2">
        <Switch
          framed
          label="Send anonymous bug reports to the Crewlet team"
          description="When the engine hits a bug in its own code, it opens a public issue on GitHub (crewlet/crewlet). At most one report a week for each bug."
          checked={shown}
          disabledReason={access.can ? undefined : access.reason}
          onCheckedChange={press}
        />
        <SettingWriteStatus
          write={write}
          unchanged="Already so."
          onReload={onSaved}
          answersWarning
        />
        {shown && answer.inert && (
          <Callout variant="warning" title="This build won't send reports">
            {INERT_WORDS[answer.inert]}
          </Callout>
        )}
        <Disclosure title="What's sent, and what never is" defaultOpen={!answer.enabled}>
          <div className="col gap-2">
            <Text as="p">
              Every report is a short, fixed form with no free text. It goes to{" "}
              <InlineCode close=",">{answer.relay}</InlineCode> which opens the GitHub issue, so
              your engine never needs a GitHub login.
            </Text>
            <Text as="h3" variant="label">
              What&rsquo;s sent
            </Text>
            <DescriptionList dense termWidth={TERM_WIDTH} items={SENT} />
            <Text as="h3" variant="label">
              What&rsquo;s never sent
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
      <Card.Header count={answer.reports.length}>
        <Card.Title>Reports you&rsquo;ve sent</Card.Title>
      </Card.Header>
      <DataGrid<IssueReportRow>
        name="reports"
        aria-label="Bug reports you've sent"
        rows={answer.reports}
        rowKey={(r) => r.fingerprint}
        defaultSort="-first_sent"
        isFailed={(r) => r.status === "failed"}
        empty={{
          size: "inline",
          title: "Nothing sent yet",
          hint: "With this on, a bug shows up here once the engine runs into it a few times.",
        }}
        columns={[
          {
            key: "title",
            header: "Bug",
            sortValue: (r) => r.title,
            cell: (r) => (
              <span className="truncate" title={r.title}>
                {r.title}
              </span>
            ),
          },
          {
            key: "fingerprint",
            header: "Bug ID",
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
            header: "GitHub issue",
            shrink: true,
            sortValue: (r) => r.issue,
            cell: (r) =>
              r.issue ? (
                <Link href={r.issue} external size="inherit">
                  #{r.issue.split("/").at(-1)}
                </Link>
              ) : (
                <EmptyValue label="No issue link yet" />
              ),
          },
          {
            key: "sent",
            header: "Details",
            shrink: true,
            cell: (r) => (
              <Button
                variant="ghost"
                size="small"
                onClick={() => setOpened(r)}
                aria-label={`See what was ${r.status === "held" ? "kept" : "sent"} for: ${r.title}`}
              >
                View
              </Button>
            ),
          },
        ]}
      />
      <Modal
        open={opened !== null}
        onClose={() => setOpened(null)}
        size="lg"
        title={opened?.status === "held" ? "What was kept" : "What was sent"}
        subtitle={opened?.title}
      >
        {opened?.status === "held" && <KeptReport payload={opened.payload} />}
        {opened && opened.status !== "held" && (
          <CodeBlock
            plain
            selectable
            language="json"
            label="The report, exactly as sent"
            code={pretty(opened.payload)}
          />
        )}
      </Modal>
    </Card>
  );
}

/**
 * A crash kept rather than sent, and how to report it privately: anybody who could
 * reach the code it crashed in might crash every server the same way, so it goes
 * to the Crewlet team through GitHub's private report, never a public issue.
 */
function KeptReport({ payload }: { payload: unknown }) {
  return (
    <div className="col gap-2">
      <Callout variant="warning" title="This crash wasn't sent">
        It happened in code that people outside your company can reach, so a public issue could show
        them how to crash your engine. Copy the report and paste it into a private report to the
        Crewlet team.
      </Callout>
      <div className="row gap-2">
        <CopyButton
          text={() => pretty(payload)}
          label="Copy the report"
          copiedLabel="Copied"
          copiedMessage="The report is on your clipboard"
          failedMessage="Your browser didn't allow copying. Select the report below and copy it"
        />
        <Link href={PRIVATE_REPORT} external size="inherit">
          Report privately on GitHub
        </Link>
      </div>
      <CodeBlock
        plain
        selectable
        language="json"
        label="The report, as it was kept"
        code={pretty(payload)}
      />
    </div>
  );
}

/** The exact record a report of each class would send. */
function ReportPreview({ answer }: { answer: IssueReportsAnswer }) {
  return (
    <Card padding="none">
      <Card.Header>
        <Card.Title>See a sample report</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-2">
        <Text as="p" tone="secondary">
          This is exactly what a report from this engine would look like. Only the numbers are made
          up.
        </Text>
        {answer.previews.map((preview) => (
          <Disclosure key={preview.class} title={preview.title}>
            <div className="col gap-2">
              {/* THE CATALOGUE'S SENTENCE, drawn as the engine's sentences
                  are: it names a fact of the report below in backticks
                  (`fatal_kind`), which is code, never punctuation. */}
              <Text as="p">{marked(preview.summary)}</Text>
              <CodeBlock
                plain
                selectable
                language="json"
                label={`Sample report: ${preview.title}`}
                code={pretty(preview.payload)}
              />
            </div>
          </Disclosure>
        ))}
      </Card.Body>
    </Card>
  );
}
