/**
 * Settings › Delegated decisions (ADR-0125): the rules a company lets agent
 * seats decide requests by, the limits the engine holds every delegate to,
 * and what the delegates decided, read whole off `delegated_decisions`.
 *
 * # The rules are changed where they are read
 *
 * The rules are the company document's `delegated_decisions` block. This
 * screen draws them as the engine runs them, numbered as an escalation names
 * them ("rule 2"), with the paths a merge or deploy rule always puts to people
 * and the checks it requires on a repository's branch, and changes them as
 * any setting is changed (`lib/useSettingWrite.ts`): the switch turns
 * delegation on or off, Add a rule and each rule's pencil open a sheet filled
 * from the document (`DelegatedRuleDialog.tsx`), and each rule's bin removes
 * it after asking. Every change is a merge patch the engine checks against
 * the whole company before it is stored (`lib/delegatedRules.ts`), drawn for
 * every reader and disabled with the reason for one who cannot write the
 * document. The limits are the engine's own and no document changes them.
 *
 * # What was decided, in three lists
 *
 * The digest is one company day of verdicts that decided a request: the day
 * still running, so far, unless `day=` names an earlier one. The pager goes
 * back a day at a time and forward no further than today. The spot-checks and
 * the flagged
 * denials are the week before now, as the engine reads them: one decision in
 * a rule's `sample_one_in`, and every denial a delegate gave with high
 * confidence. One toolbar narrows all three, and each list's foot says how
 * many of its rows are shown and whether the log held more than the page.
 *
 * Admitted for an administrator and the people a rule escalates to; anybody
 * else is told so by the engine's refusal.
 */

import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Button,
  Card,
  EmptyValue,
  IconButton,
  Input,
  Modal,
  PageNote,
  Select,
  SettingsTable,
  Switch,
  Tag,
  TableDateCell,
  TableEntityCell,
  TableTextCell,
  VisuallyHidden,
  type DataGridColumn,
} from "@crewlethq/ui";
import {
  BadgeCheckGlyph,
  ChevronLeftGlyph,
  ChevronRightGlyph,
  FlagGlyph,
  GaugeGlyph,
  ListChecksGlyph,
  PencilGlyph,
  PlusGlyph,
  SearchGlyph,
  ShieldCheckGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { SettingWriteStatus, standingWords } from "~/components/SettingWriteStatus.tsx";
import { enabledPlan, removeRulePlan } from "~/lib/delegatedRules.ts";
import { useSettingWrite, type SettingWrite } from "~/lib/useSettingWrite.ts";
import { useConfigWriteAccess, type ConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { DelegatedRuleDialog, RULES_REREAD, type RuleTarget } from "./DelegatedRuleDialog.tsx";
import { useParam } from "~/app/router.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { plainSeat } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, nameOfIn, useSeatBadgeOf, type NameOf } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, plural, tsKey } from "~/lib/format.ts";
import { andList } from "~/lib/words.ts";
import {
  confidenceWords,
  giverWords,
  isReleaseAct,
  kindWords,
  overruledWords,
  verdictWords,
} from "~/lib/delegation.ts";
import type {
  DelegatedDecisionsAnswer,
  DelegatedRequiredChecks,
  DelegatedRule,
  DelegatedVerdictRow,
} from "~/contract/delegated.ts";

/** How often the read is asked again: the digest moves once a day, the week's lists slowly. */
const DELEGATED_POLL_MS = 300_000;

/**
 * How often today, so far, is asked again: a delegate answers within the
 * engine's 15-minute deadline, and a person watching the day sees its verdict
 * within a minute of it, for one read of three short pages.
 */
const RUNNING_POLL_MS = 60_000;

/** The verdict filter's choices: every verdict, and none. */
const VERDICT_OPTIONS = [
  { value: "", label: "Any verdict" },
  { value: "allow", label: "Allowed" },
  { value: "escalate", label: "Escalated" },
  { value: "deny", label: "Denied" },
];

/** The giver filter's choices (`accessdomain.VerdictBy`). */
const GIVER_OPTIONS = [
  { value: "", label: "Anybody" },
  { value: "agent", label: "Delegates" },
  { value: "person", label: "People" },
  { value: "engine", label: "Crewlet" },
];

/** How many of a rule's own paths its row shows before it counts the rest. */
const PATHS_SHOWN = 4;

/** How many of a rule's required-check entries its row shows before it counts the rest. */
const CHECKS_SHOWN = 2;

/** One required-check entry as a line: `acme/api main: test, lint`. */
const checksLine = (c: DelegatedRequiredChecks) => `${c.repo} ${c.branch}: ${c.checks.join(", ")}`;

/** A company day label moved by `days`: `2026-11-09` back one is `2026-11-08`. */
export function shiftDay(label: string, days: number): string {
  const at = Date.parse(`${label}T00:00:00Z`);
  if (!Number.isFinite(at)) return label;
  return new Date(at + days * 86_400_000).toISOString().slice(0, 10);
}

/** Whether a verdict row matches the toolbar: the search, the verdict and who gave it. */
export function rowMatches(
  row: DelegatedVerdictRow,
  q: string,
  verdict: string,
  giver: string,
  name: NameOf,
): boolean {
  if (verdict && row.verdict !== verdict) return false;
  if (giver && row.by_kind !== giver) return false;
  const needle = q.trim().toLowerCase();
  if (!needle) return true;
  const hay = [
    name(row.seat),
    row.seat,
    giverWords(row, name),
    row.subject ?? "",
    row.host,
    row.reason ?? "",
    row.item,
  ];
  return hay.some((s) => s.toLowerCase().includes(needle));
}

export function DelegatedDecisions() {
  const [day, setDay] = useParam("day", "");
  const [q, setQ] = useParam("q", "");
  const [verdict, setVerdict] = useParam("verdict", "");
  const [giver, setGiver] = useParam("by", "");
  // TODAY, SO FAR, unless the reader went back: the engine's own default is the
  // last whole day, which Home's digest reads.
  const params = useMemo(() => ({ day: day || "today" }), [day]);
  const { data, loading, error, refusal, detail, refetch } = useQuery(
    "delegated_decisions",
    params,
    { pollMs: day === "" ? RUNNING_POLL_MS : DELEGATED_POLL_MS },
  );
  const org = useOrg();
  const nameOf = useMemo(() => nameOfIn(indexOrg(org)), [org]);
  const shown = (rows: readonly DelegatedVerdictRow[]) =>
    rows.filter((r) => rowMatches(r, q, verdict, giver, nameOf));
  const filtered = q.trim() !== "" || verdict !== "" || giver !== "";
  return (
    <>
      <PageNote>
        What agent seats decide in a person&rsquo;s place, the rules they decide by and what they
        decided. The rules are a setting of the company document: change them here, and the engine
        checks each change against the whole company first.
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && (
          <>
            <RulesCard answer={data} nameOf={nameOf} onApplied={refetch} />
            <LimitsCard answer={data} />
            <PageToolbar>
              <Input
                inputSize="sm"
                type="search"
                value={q}
                onChange={(e) => setQ(e.target.value)}
                onClear={() => setQ("")}
                clearLabel="Clear the search"
                leading={<SearchGlyph size="sm" />}
                placeholder="Seat, request, server or reason"
                aria-label="Search decisions"
                width="md"
              />
              <Select
                size="sm"
                width="auto"
                value={verdict}
                onChange={(value) => setVerdict(String(value))}
                ariaLabel="Verdict"
                active={verdict !== ""}
                options={VERDICT_OPTIONS}
              />
              <Select
                size="sm"
                width="auto"
                value={giver}
                onChange={(value) => setGiver(String(value))}
                ariaLabel="Given by"
                active={giver !== ""}
                options={GIVER_OPTIONS}
              />
              <span className="spacer" />
              {/* THE DAY AND ITS PAGER WRAP AS ONE, never a chevron left behind. */}
              <span className="row gap-1">
                <IconButton
                  size="sm"
                  variant="ghost"
                  label="The day before"
                  icon={<ChevronLeftGlyph size="sm" />}
                  onClick={() => setDay(shiftDay(data.day, -1))}
                />
                <span className="t-caption t-num">{data.day}</span>
                <IconButton
                  size="sm"
                  variant="ghost"
                  label="The day after"
                  icon={<ChevronRightGlyph size="sm" />}
                  // TODAY IS THE LATEST DAY: past it is one that has not begun.
                  disabled={!data.complete}
                  onClick={() => setDay(shiftDay(data.day, 1))}
                />
              </span>
            </PageToolbar>
            <VerdictsCard
              name="decided"
              icon={<ListChecksGlyph size="sm" />}
              title={data.complete ? `Decided on ${data.day}` : "Decided today, so far"}
              subtitle={
                data.complete
                  ? "Every request a verdict decided on that company day"
                  : "The company day is still running: requests decided later today join this list."
              }
              all={data.decided}
              rows={shown(data.decided)}
              more={data.decided_more}
              filtered={filtered}
              empty={
                data.complete
                  ? "No request was decided that day."
                  : "No request has been decided yet today."
              }
            />
            <VerdictsCard
              name="sampled"
              icon={<BadgeCheckGlyph size="sm" />}
              title="Spot-checks"
              subtitle="Decisions picked for a look after the fact, over the last 7 days"
              all={data.spot_checks}
              rows={shown(data.spot_checks)}
              more={data.spot_checks_more}
              filtered={filtered}
              empty="No decision was picked for a spot-check this week."
            />
            <VerdictsCard
              name="flagged"
              icon={<FlagGlyph size="sm" />}
              title="Flagged denials"
              subtitle="Requests a delegate denied with high confidence, over the last 7 days"
              all={data.flagged}
              rows={shown(data.flagged)}
              more={data.flagged_more}
              filtered={filtered}
              empty="No delegate flagged a request as harmful this week."
            />
          </>
        )}
      </QueryState>
    </>
  );
}

/** Which of the card's controls the one write in flight came from. */
type RulesGesture =
  | { readonly kind: "none" }
  | { readonly kind: "switch" }
  | { readonly kind: "sheet"; readonly target: RuleTarget }
  | { readonly kind: "remove"; readonly rule: DelegatedRule };

/**
 * Whether delegation is on, and each rule as the engine numbers it, with the
 * controls that change them.
 *
 * ONE WRITE FOR THE CARD, which the switch, the sheet and a removal each
 * drive: the rules are one list in the document, so two changes of it in
 * flight at once would each be built on a list the other replaces. What a
 * saved change came to (applying, refused by this node) is said beside the
 * card's state until this node applied it and the read carries it.
 */
function RulesCard({
  answer,
  nameOf,
  onApplied,
}: {
  answer: DelegatedDecisionsAnswer;
  nameOf: NameOf;
  onApplied: () => void;
}) {
  const access = useConfigWriteAccess();
  const write = useSettingWrite({ reread: RULES_REREAD, onApplied });
  const [gesture, setGesture] = useState<RulesGesture>({ kind: "none" });
  const saved = write.state.kind === "saved" ? write.state : null;
  // A SAVE CLOSES THE SHEET OR THE QUESTION: the mark beside the card's state
  // says the change is applying, which is the answer the person waits for.
  useEffect(() => {
    if (saved && (gesture.kind === "sheet" || gesture.kind === "remove")) {
      setGesture({ kind: "none" });
    }
  }, [saved, gesture.kind]);
  const begin = (next: RulesGesture) => {
    write.reset();
    setGesture(next);
  };
  const mark =
    saved && (saved.standing === "applying" || saved.standing === "refused")
      ? `Saved, ${standingWords(saved.standing)}`
      : "";
  const columns = ruleColumns(nameOf, access, (next) => begin(next));
  return (
    <Card padding="none">
      <Card.Header
        icon={<ShieldCheckGlyph size="sm" />}
        count={answer.rules.length}
        subtitle={
          answer.enabled
            ? "Requests of these kinds go to their delegates first."
            : "Off: every request goes to people at once. The rules are kept."
        }
        actions={
          <span className="row gap-2">
            {mark && (
              <span className="t-caption" role="status">
                {mark}
              </span>
            )}
            <Switch
              size="sm"
              label={answer.enabled ? "On" : "Off"}
              checked={answer.enabled}
              disabledReason={access.can ? (write.busy ? "Saving…" : undefined) : access.reason}
              title={access.can ? undefined : access.reason}
              onCheckedChange={(on) => {
                begin({ kind: "switch" });
                write.submit(enabledPlan(on));
              }}
            />
            <Button
              size="small"
              variant="secondary"
              leadingIcon={<PlusGlyph size="sm" />}
              disabledReason={access.can ? undefined : access.reason}
              title={access.can ? undefined : access.reason}
              onClick={() => begin({ kind: "sheet", target: { kind: "add" } })}
            >
              Add a rule
            </Button>
          </span>
        }
      >
        <Card.Title id="delegated-rules">Rules</Card.Title>
      </Card.Header>
      {gesture.kind === "switch" && write.state.kind !== "idle" && !saved && (
        <div className="delegated-rules-status">
          <SettingWriteStatus write={write} unchanged="Already so." onReload={onApplied} />
        </div>
      )}
      <DataGrid<DelegatedRule>
        name="rules"
        aria-labelledby="delegated-rules"
        rows={answer.rules}
        rowKey={(r) => String(r.rule)}
        columns={columns}
        empty={{
          title: "No rules",
          hint: "The company document names none, so every request goes to people. Add a rule to delegate one kind of request.",
          icon: "shield-check",
        }}
      />
      {gesture.kind === "sheet" && (
        <DelegatedRuleDialog
          target={gesture.target}
          write={write}
          onReload={onApplied}
          onClose={() => setGesture({ kind: "none" })}
        />
      )}
      {gesture.kind === "remove" && (
        <RemoveRuleDialog
          rule={gesture.rule}
          last={answer.rules.length}
          write={write}
          onReload={onApplied}
          onClose={() => {
            write.reset();
            setGesture({ kind: "none" });
          }}
        />
      )}
    </Card>
  );
}

/**
 * Removing one rule, asked first: requests of its kinds go to people at once
 * from then on, and every later rule's number moves up one, which an
 * escalation names it by.
 */
function RemoveRuleDialog({
  rule,
  last,
  write,
  onReload,
  onClose,
}: {
  rule: DelegatedRule;
  /** The number of the last rule, to say which move up. */
  last: number;
  write: SettingWrite;
  onReload: () => void;
  onClose: () => void;
}) {
  const kinds = andList(rule.kinds.map(kindWords));
  return (
    <Modal
      open
      size="sm"
      role="alertdialog"
      title={`Remove rule ${rule.rule}?`}
      icon={<TrashGlyph size="sm" />}
      onClose={onClose}
      dismissable={!write.busy}
      onSubmit={() => {
        if (write.busy) return;
        if (write.state.kind === "warned") write.confirm();
        else write.submit(removeRulePlan(rule.rule, rule.kinds));
      }}
      stackBody
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" loading={write.busy}>
            {write.state.kind === "warned" ? "Remove anyway" : "Remove rule"}
          </Button>
        </>
      }
    >
      <p className="t-body" style={{ margin: 0 }}>
        {`Requests for ${kinds.toLowerCase()} go to people at once.`}
        {rule.rule < last
          ? ` Rules after it move up one: rule ${rule.rule + 1} becomes rule ${rule.rule}.`
          : ""}
      </p>
      <SettingWriteStatus write={write} unchanged="Already removed." onReload={onReload} />
    </Modal>
  );
}

/** The rule grid's columns, ending in each rule's change and removal. */
function ruleColumns(
  nameOf: NameOf,
  access: ConfigWriteAccess,
  begin: (gesture: RulesGesture) => void,
): DataGridColumn<DelegatedRule>[] {
  const names = (handles: readonly string[]) => andList(handles.map(nameOf));
  const held = access.can ? undefined : access.reason;
  return [
    {
      key: "rule",
      header: "Rule",
      shrink: true,
      cell: (r) => <span className="t-num">{r.rule}</span>,
    },
    {
      key: "kinds",
      header: "Decides",
      floor: "9rem",
      cell: (r) => (
        <span className="row gap-1 wrap">
          {r.kinds.map((k) => (
            <Tag key={k} appearance="outline" title={k}>
              {kindWords(k)}
            </Tag>
          ))}
        </span>
      ),
    },
    {
      key: "delegates",
      header: "Delegates",
      floor: "9rem",
      // A PANEL READS WHOLE, its two names and that both must allow, beside
      // the paths column that narrows it.
      overflow: "wrap",
      cell: (r) =>
        `${names(r.delegates)}${r.delegates.length > 1 ? " (a panel: both must allow)" : ""}`,
    },
    {
      key: "escalate",
      header: "Escalates to",
      floor: "8rem",
      cell: (r) => <TableTextCell>{names(r.escalate_to)}</TableTextCell>,
    },
    {
      key: "paths",
      header: "Always escalates",
      floor: "9rem",
      drop: 2,
      // A RULE'S OWN PATHS, beside the built-in list every merge and deploy
      // rule carries; a command rule has none to name.
      title: (r) =>
        r.always_escalate_paths.length > 0 ? r.always_escalate_paths.join("\n") : undefined,
      cell: (r) => {
        const paths = r.always_escalate_paths;
        if (paths.length === 0) {
          return (
            <EmptyValue
              label={r.kinds.some(isReleaseAct) ? "The built-in paths only" : "Not for commands"}
            />
          );
        }
        const rest = paths.length - PATHS_SHOWN;
        return (
          <span className="row gap-1 wrap">
            {paths.slice(0, PATHS_SHOWN).map((p) => (
              <Tag key={p} appearance="outline" monospace>
                {p}
              </Tag>
            ))}
            {rest > 0 ? <Tag appearance="outline">{`${fmtCount(rest)} more`}</Tag> : null}
          </span>
        );
      },
    },
    {
      key: "checks",
      header: "Requires checks",
      floor: "10rem",
      drop: 3,
      // A RULE'S OWN CHECKS per repository and branch, beside whatever the
      // branch's protection requires; a command rule has none to name.
      title: (r) =>
        r.required_checks.length > 0 ? r.required_checks.map(checksLine).join("\n") : undefined,
      cell: (r) => {
        const entries = r.required_checks;
        if (entries.length === 0) {
          return (
            <EmptyValue
              label={r.kinds.some(isReleaseAct) ? "What the branch requires" : "Not for commands"}
            />
          );
        }
        const rest = entries.length - CHECKS_SHOWN;
        return (
          <span className="col gap-1">
            {entries.slice(0, CHECKS_SHOWN).map((c) => (
              <span key={`${c.repo} ${c.branch}`} className="row gap-1 wrap">
                <Tag appearance="outline" monospace>{`${c.repo} ${c.branch}`}</Tag>
                <TableTextCell>{c.checks.join(", ")}</TableTextCell>
              </span>
            ))}
            {rest > 0 ? <Tag appearance="outline">{`${fmtCount(rest)} more`}</Tag> : null}
          </span>
        );
      },
    },
    {
      key: "sample",
      header: "Spot-checks",
      shrink: true,
      align: "right",
      drop: 1,
      cell: (r) => <span className="t-num">{`1 in ${fmtCount(r.sample_one_in)}`}</span>,
    },
    {
      key: "actions",
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      action: true,
      shrink: true,
      cell: (r) => (
        <span className="row gap-1">
          <IconButton
            size="sm"
            variant="ghost"
            icon={<PencilGlyph size="sm" />}
            label={`Change rule ${r.rule}`}
            disabledReason={held}
            title={held}
            onClick={() => begin({ kind: "sheet", target: { kind: "edit", number: r.rule } })}
          />
          <IconButton
            size="sm"
            variant="ghost"
            icon={<TrashGlyph size="sm" />}
            label={`Remove rule ${r.rule}`}
            disabledReason={held}
            title={held}
            onClick={() => begin({ kind: "remove", rule: r })}
          />
        </span>
      ),
    },
  ];
}

/** The limits the engine holds every delegate to: its own, never the document's. */
function LimitsCard({ answer }: { answer: DelegatedDecisionsAnswer }) {
  const { limits } = answer;
  return (
    <Card padding="none">
      <Card.Header
        icon={<GaugeGlyph size="sm" />}
        subtitle="Whatever a delegate answers, past these a request goes to people."
      >
        <Card.Title id="delegated-limits">What Crewlet enforces</Card.Title>
      </Card.Header>
      <SettingsTable aria-labelledby="delegated-limits">
        <SettingsTable.Row label="A request no delegate answered goes to people after">
          {plural(limits.deadline_minutes, "minute")}
        </SettingsTable.Row>
        <SettingsTable.Row label="Allows per kind and delegate, each clock hour">
          {fmtCount(limits.decisions_per_hour)}
        </SettingsTable.Row>
        <SettingsTable.Row label="Commands allowed per server, each clock hour">
          {fmtCount(limits.server_allows_per_hour)}
        </SettingsTable.Row>
      </SettingsTable>
    </Card>
  );
}

/** One list of verdicts, its foot saying how much of it is shown. */
function VerdictsCard({
  name,
  icon,
  title,
  subtitle,
  all,
  rows,
  more,
  filtered,
  empty,
}: {
  name: string;
  icon: ReactNode;
  title: string;
  subtitle: string;
  all: readonly DelegatedVerdictRow[];
  rows: DelegatedVerdictRow[];
  more: boolean;
  filtered: boolean;
  empty: string;
}) {
  const columns = useVerdictColumns();
  const titleID = `delegated-${name}`;
  const foot = [
    all.length > 0 ? `Showing ${fmtCount(rows.length)} of ${fmtCount(all.length)}` : "",
    more ? "The log holds more than this page" : "",
  ].filter(Boolean);
  return (
    <Card padding="none">
      <Card.Header icon={icon} count={all.length} subtitle={subtitle}>
        <Card.Title id={titleID}>{title}</Card.Title>
      </Card.Header>
      <DataGrid<DelegatedVerdictRow>
        name={name}
        aria-labelledby={titleID}
        rows={rows}
        rowKey={(r) => `${r.request}:${r.by}:${r.at}`}
        columns={columns}
        defaultSort="-at"
        empty={{
          title: filtered && all.length > 0 ? "Nothing matches" : empty,
          hint: filtered && all.length > 0 ? "Clear the search or the filters." : undefined,
          icon: "list-checks",
        }}
        footer={foot.length > 0 ? <span className="t-caption">{foot.join(". ")}.</span> : null}
      />
    </Card>
  );
}

/**
 * The columns every list of verdicts shares. What gives way first is what
 * the row says elsewhere: the server (in the command's hover), the seat, who
 * gave it, and the reason last, since a digest is read for why.
 */
function useVerdictColumns(): DataGridColumn<DelegatedVerdictRow>[] {
  const now = useNow();
  const seatBadge = useSeatBadgeOf();
  const org = useOrg();
  const nameOf = useMemo(() => nameOfIn(indexOrg(org)), [org]);
  return [
    {
      key: "at",
      header: "When",
      align: "right",
      shrink: true,
      sortValue: (r) => tsKey(r.at),
      cell: (r) => <TableDateCell value={r.at} now={now} />,
    },
    {
      key: "seat",
      header: "Seat",
      // SIZED TO THE NAME, so the command and the reason share what is left.
      shrink: true,
      drop: 2,
      sortValue: (r) => nameOf(r.seat),
      cell: (r) => <TableEntityCell {...plainSeat({ ...seatBadge(r.seat) })} />,
    },
    {
      key: "what",
      header: "What",
      floor: "12rem",
      overflow: "truncate",
      title: (r) =>
        r.subject ? `${r.subject}${r.host ? ` on ${r.host}` : ""}` : r.subject_unopened,
      cell: (r) =>
        r.subject ? (
          <span className="mono">{r.subject}</span>
        ) : (
          <EmptyValue label={r.subject_unopened ?? "Not shown"} />
        ),
    },
    {
      key: "host",
      header: "Server",
      shrink: true,
      drop: 1,
      sortValue: (r) => r.host,
      cell: (r) =>
        r.host ? (
          <Tag appearance="outline" monospace>
            {r.host}
          </Tag>
        ) : (
          <EmptyValue label={r.kind === "ssh_operate" ? "No server" : "Not on a server"} />
        ),
    },
    {
      key: "verdict",
      header: "Verdict",
      shrink: true,
      sortValue: (r) => r.verdict,
      // WHY THE ENGINE CHANGED OR MADE IT, in its closed words, on hover.
      title: (r) =>
        r.overruled.length > 0
          ? `By Crewlet: ${r.overruled.map(overruledWords).join(", ")}`
          : undefined,
      cell: (r) => {
        const v = verdictWords(r.verdict);
        return (
          <Tag size="xs" variant={v.tone}>
            {v.label}
          </Tag>
        );
      },
    },
    {
      key: "by",
      header: "By",
      shrink: true,
      drop: 3,
      sortValue: (r) => giverWords(r, nameOf),
      title: (r) => confidenceWords(r.confidence) || undefined,
      cell: (r) => (
        <TableTextCell>
          {giverWords(r, nameOf)}
          {r.rule > 0 ? ` · rule ${r.rule}` : r.through === "decision" ? " · a decision" : ""}
        </TableTextCell>
      ),
    },
    {
      key: "reason",
      header: "Reason",
      floor: "10rem",
      overflow: "truncate",
      drop: 4,
      title: (r) => r.reason || undefined,
      cell: (r) =>
        r.reason ? (
          <TableTextCell>{r.reason}</TableTextCell>
        ) : (
          <EmptyValue label={r.by_kind === "engine" ? "Crewlet decided it" : "No reason given"} />
        ),
    },
  ];
}
