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
 * document. The limits every delegate is held to (the deadline and the two
 * hourly caps) are the document's too, within ranges that cannot switch one
 * off: each row is the field Settings › Limits changes a cap with
 * (`components/LimitSetting.tsx`), its value beside the engine's default.
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

import { useEffect, useMemo, useState } from "react";
import {
  Button,
  Card,
  EmptyState,
  EmptyValue,
  IconButton,
  InlineCode,
  InlineRef,
  Input,
  List,
  ListItem,
  PageNote,
  Select,
  SettingsTable,
  Switch,
  Tag,
  TableDateCell,
  TableEntityCell,
  TableTextCell,
  type DataGridColumn,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import {
  ArrowRightGlyph,
  ChevronLeftGlyph,
  ChevronRightGlyph,
  GaugeGlyph,
  PencilGlyph,
  PlusGlyph,
  SearchGlyph,
  TrashGlyph,
} from "@crewlethq/icons/glyphs";
import { LimitSetting } from "~/components/LimitSetting.tsx";
import { saysSomething, SettingWriteStatus } from "~/components/SettingWriteStatus.tsx";
import {
  DELEGABLE_KINDS,
  enabledPlan,
  problemWords,
  removeRulePlan,
} from "~/lib/delegatedRules.ts";
import { useSettingWrite, type SettingWrite } from "~/lib/useSettingWrite.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { DelegatedRuleDialog, RULES_REREAD, type RuleTarget } from "./DelegatedRuleDialog.tsx";
import { useParam } from "~/app/router.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageToolbar } from "~/app/frame/PageToolbar.tsx";
import { plainSeat } from "~/app/frame/cells.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import {
  handleLabel,
  indexOrg,
  nameOfIn,
  useOrgIndex,
  useSeatBadgeOf,
  type NameOf,
} from "~/lib/seats.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { useNow } from "~/lib/clock.ts";
import { companyDateLabel, fmtCount, tsKey } from "~/lib/format.ts";
import {
  confidenceWords,
  giverWords,
  isReleaseAct,
  kindsPhrase,
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
const PATHS_SHOWN = 3;

/** How many of a rule's required-check entries its row shows before it counts the rest. */
const CHECKS_SHOWN = 2;

/** One required-check entry as a line: `acme/api main: test, lint`. */
const checksLine = (c: DelegatedRequiredChecks) => `${c.repo} ${c.branch}: ${c.checks.join(", ")}`;

/** How often a rule's decisions are reviewed, as its row says it. */
export const reviewWords = (rate: number) =>
  rate <= 1 ? "Reviews every decision" : `Reviews 1 in ${fmtCount(rate)}`;

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
        Let agents decide some requests in a person&rsquo;s place; anything risky or unclear still
        goes to people.
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && (
          <>
            <RulesCard answer={data} onApplied={refetch} />
            <LimitsCard answer={data} onApplied={refetch} />
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
                {/* THE DAY AS EVERY OTHER DATE IS SAID, never the engine's
                    `2026-11-15` label: today by its name, another by its date. */}
                <span className="t-caption t-num" title={data.day}>
                  {data.complete ? companyDateLabel(data.day) : "Today"}
                </span>
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
              title={
                data.complete ? `Decided on ${companyDateLabel(data.day)}` : "Decided today, so far"
              }
              subtitle={
                data.complete
                  ? "Every request decided that day."
                  : "Requests decided later today appear here too."
              }
              all={data.decided}
              rows={shown(data.decided)}
              more={data.decided_more}
              filtered={filtered}
              empty={{
                title: data.complete ? "No decisions that day" : "No decisions yet today",
                hint: "A request an agent decides shows here with its verdict.",
              }}
            />
            <VerdictsCard
              name="sampled"
              title="Spot-checks"
              subtitle="Decisions picked for people to review, last 7 days"
              all={data.spot_checks}
              rows={shown(data.spot_checks)}
              more={data.spot_checks_more}
              filtered={filtered}
              empty={{
                title: "No spot-checks this week",
                hint: "Some decisions are picked for people to review after the fact.",
              }}
            />
            <VerdictsCard
              name="flagged"
              title="Flagged denials"
              subtitle="Requests an agent refused with high confidence, last 7 days"
              all={data.flagged}
              rows={shown(data.flagged)}
              more={data.flagged_more}
              filtered={filtered}
              empty={{
                title: "No flagged denials this week",
                hint: "A request an agent refuses as harmful shows here.",
              }}
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
  onApplied,
}: {
  answer: DelegatedDecisionsAnswer;
  onApplied: () => void;
}) {
  const access = useConfigWriteAccess();
  const write = useSettingWrite({
    reread: RULES_REREAD,
    onApplied,
    problemWords: (problems) => problems.map(problemWords).join(" "),
  });
  const [gesture, setGesture] = useState<RulesGesture>({ kind: "none" });
  const saved = write.state.kind === "saved" ? write.state : null;
  // A SAVE CLOSES THE SHEET OR THE QUESTION: its toast says it is applying.
  useEffect(() => {
    if (saved && (gesture.kind === "sheet" || gesture.kind === "remove")) {
      setGesture({ kind: "none" });
    }
  }, [saved, gesture.kind]);
  const begin = (next: RulesGesture) => {
    write.reset();
    setGesture(next);
  };
  const held = access.can ? undefined : access.reason;
  // A kind is in one rule only, so with every kind taken there is nothing to add.
  const full = DELEGABLE_KINDS.every((k) => answer.rules.some((r) => r.kinds.includes(k)));
  const addHeld =
    held ?? (full ? "Every kind of request has a rule. Edit one instead." : undefined);
  return (
    <Card padding="none">
      <Card.Header
        count={answer.rules.length}
        subtitle={
          answer.enabled
            ? "Requests of these kinds go to an agent first."
            : "Off. Every request goes to people, and the rules are kept."
        }
        actions={
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
        }
      >
        <Card.Title id="delegated-rules">Rules</Card.Title>
      </Card.Header>
      <PageActions>
        <Button
          size="small"
          variant="primary"
          leadingIcon={<PlusGlyph size="sm" />}
          disabledReason={addHeld}
          title={addHeld}
          onClick={() => begin({ kind: "sheet", target: { kind: "add" } })}
        >
          Add rule
        </Button>
      </PageActions>
      {saysSomething(write.state) && (gesture.kind === "switch" || saved) && (
        <div className="delegated-rules-status">
          <SettingWriteStatus write={write} unchanged="Already so." onReload={onApplied} />
        </div>
      )}
      {answer.rules.length === 0 ? (
        <EmptyState
          size="inline"
          title="No rules yet"
          description="Until a rule names a kind of request, people decide every one."
        />
      ) : (
        <List variant="numbered-divided" aria-labelledby="delegated-rules">
          {answer.rules.map((rule) => (
            <RuleRow
              key={rule.rule}
              rule={rule}
              held={held}
              onChange={() => begin({ kind: "sheet", target: { kind: "edit", number: rule.rule } })}
              onRemove={() => begin({ kind: "remove", rule })}
            />
          ))}
        </List>
      )}
      {gesture.kind === "sheet" && (
        <DelegatedRuleDialog
          target={gesture.target}
          write={write}
          sample={answer.sample}
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
  const kinds = kindsPhrase(rule.kinds);
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
        {`People will decide ${kinds.toLowerCase()} again.`}
        {rule.rule < last
          ? ` Rules after it move up one: rule ${rule.rule + 1} becomes rule ${rule.rule}.`
          : ""}
      </p>
      <SettingWriteStatus write={write} unchanged="Already removed." onReload={onReload} />
    </Modal>
  );
}

/**
 * One rule as a sentence: what, who decides, who hears about it, then a merge
 * or deploy rule's conditions. Numbered as the engine numbers it.
 */
function RuleRow({
  rule,
  held,
  onChange,
  onRemove,
}: {
  rule: DelegatedRule;
  /** Why this reader cannot change the rules, or undefined. */
  held: string | undefined;
  onChange: () => void;
  onRemove: () => void;
}) {
  const release = rule.kinds.some(isReleaseAct);
  const paths = rule.always_escalate_paths;
  const checks = rule.required_checks;
  return (
    <ListItem
      action={
        <span className="row gap-1">
          <IconButton
            size="sm"
            variant="ghost"
            icon={<PencilGlyph size="sm" />}
            label={`Edit rule ${rule.rule}`}
            disabledReason={held}
            title={held}
            onClick={onChange}
          />
          <IconButton
            size="sm"
            variant="ghost"
            icon={<TrashGlyph size="sm" />}
            label={`Remove rule ${rule.rule}`}
            disabledReason={held}
            title={held}
            onClick={onRemove}
          />
        </span>
      }
    >
      <div className="delegated-rule">
        <p className="delegated-rule-head">
          <strong>{kindsPhrase(rule.kinds)}</strong>
          <ArrowRightGlyph size="sm" className="delegated-rule-arrow" aria-hidden />
          <span>
            <SeatNames handles={rule.delegates} />{" "}
            {rule.delegates.length > 1 ? "decide together" : "decides"}
          </span>
        </p>
        <p className="delegated-rule-meta">
          <span>
            Escalates to <SeatNames handles={rule.escalate_to} />
          </span>
          <span className="t-num">{reviewWords(rule.sample_one_in)}</span>
        </p>
        {release && (
          <p className="delegated-rule-meta">
            <span title={checks.length > 0 ? checks.map(checksLine).join("\n") : undefined}>
              {"Checks: "}
              {checks.length === 0
                ? "what each branch requires"
                : checks.slice(0, CHECKS_SHOWN).map((c, i) => (
                    <span key={`${c.repo} ${c.branch}`}>
                      {i > 0 ? ", " : ""}
                      <InlineCode>{`${c.repo} ${c.branch}`}</InlineCode>
                      {` (${c.checks.join(", ")})`}
                    </span>
                  ))}
              {checks.length > CHECKS_SHOWN
                ? ` and ${fmtCount(checks.length - CHECKS_SHOWN)} more`
                : ""}
            </span>
            {paths.length > 0 && (
              <span title={paths.join("\n")}>
                {"Always to people: "}
                {paths.slice(0, PATHS_SHOWN).map((p, i) => (
                  <span key={p}>
                    {i > 0 ? ", " : ""}
                    <InlineCode>{p}</InlineCode>
                  </span>
                ))}
                {paths.length > PATHS_SHOWN
                  ? ` and ${fmtCount(paths.length - PATHS_SHOWN)} more`
                  : ""}
              </span>
            )}
          </p>
        )}
      </div>
    </ListItem>
  );
}

/**
 * Seats named in a sentence, each with its face before its name and its
 * handle on hover; a handle the chart does not hold is said as written.
 */
export function SeatNames({ handles }: { handles: readonly string[] }) {
  const index = useOrgIndex();
  return (
    <>
      {handles.map((handle, i) => {
        const seat = index.byHandle.get(handle);
        const name = seat?.name || handle;
        const kind = seat?.kind === "human" ? "human" : "agent";
        return (
          <span key={handle}>
            {i === 0 ? "" : i === handles.length - 1 ? " and " : ", "}
            <InlineRef
              markShape="face"
              mark={
                <SeatAvatar name={name} kind={kind} avatar={seat?.avatar} size="text" decorative />
              }
              title={name === handle ? handleLabel(handle) : `${name} · ${handleLabel(handle)}`}
            >
              {name}
            </InlineRef>
          </span>
        );
      })}
    </>
  );
}

/**
 * What each limit is called, by its key in the company document. A key this
 * build has no words for is drawn by the key itself.
 */
const DELEGATED_LIMIT_LABELS: Readonly<Record<string, string>> = {
  "delegated_decisions.deadline_seconds": "An agent must answer within",
  "delegated_decisions.allows_per_hour": "Most allows per agent and kind, each hour",
  "delegated_decisions.server_allows_per_hour": "Most commands allowed per server, each hour",
};

/**
 * The limits the engine holds every delegate to: the document's, within
 * ranges that cannot switch one off, each changed where it is read by the
 * field Settings › Limits changes a cap with.
 */
function LimitsCard({
  answer,
  onApplied,
}: {
  answer: DelegatedDecisionsAnswer;
  onApplied: () => void;
}) {
  return (
    <Card padding="none">
      <Card.Header
        icon={<GaugeGlyph size="sm" />}
        count={answer.limits.length}
        subtitle="Past any of these, a request goes to people, whatever the agent decides."
      >
        <Card.Title id="delegated-limits">What Crewlet enforces</Card.Title>
      </Card.Header>
      <SettingsTable aria-labelledby="delegated-limits">
        {answer.limits.map((cap) => (
          <LimitSetting
            key={cap.key}
            cap={cap}
            label={DELEGATED_LIMIT_LABELS[cap.key] ?? cap.key}
            area="Delegated decisions"
            reread="Reload to see the limits as they are now."
            onApplied={onApplied}
          />
        ))}
      </SettingsTable>
    </Card>
  );
}

/** One list of verdicts, its foot saying how much of it is shown. */
function VerdictsCard({
  name,
  title,
  subtitle,
  all,
  rows,
  more,
  filtered,
  empty,
}: {
  name: string;
  title: string;
  subtitle: string;
  all: readonly DelegatedVerdictRow[];
  rows: DelegatedVerdictRow[];
  more: boolean;
  filtered: boolean;
  empty: { title: string; hint: string };
}) {
  const columns = useVerdictColumns();
  const titleID = `delegated-${name}`;
  const foot = [
    all.length > 0 ? `Showing ${fmtCount(rows.length)} of ${fmtCount(all.length)}` : "",
    more ? "The log holds more than this page" : "",
  ].filter(Boolean);
  return (
    <Card padding="none">
      <Card.Header count={all.length} subtitle={subtitle}>
        <Card.Title id={titleID}>{title}</Card.Title>
      </Card.Header>
      <DataGrid<DelegatedVerdictRow>
        name={name}
        aria-labelledby={titleID}
        rows={rows}
        rowKey={(r) => `${r.request}:${r.by}:${r.at}`}
        columns={columns}
        defaultSort="-at"
        empty={
          filtered && all.length > 0
            ? { title: "Nothing matches", hint: "Clear the search or the filters.", size: "inline" }
            : { ...empty, size: "inline" }
        }
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
