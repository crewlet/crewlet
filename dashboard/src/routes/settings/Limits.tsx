/**
 * Settings › Limits: the caps the engine holds the company's work to, as it
 * ENFORCES them, and the one place they are drawn.
 *
 * # What the engine enforces, never a copy of it
 *
 * Every number is the engine's own answer (`limits`, an administrator's):
 * the value it holds a turn, a delegation call or a wake-up to, with its
 * default applied where the company document writes none. The page keeps no
 * default of its own, so it can never say a number the engine does not hold.
 * The applied company does not remember whether a value was written, so the
 * page says only what is true: whether the value in force is the engine's
 * default. A default says so as a quiet word; a value that differs carries a
 * mark and the default it replaced, which is the one row a reader scanning
 * the page came to find. A value that switches its limit off says Off rather
 * than 0.
 *
 * # One table, drawn as one
 *
 * Each group is a card holding a [SettingsTable]: the cap's name with its key
 * in the company document under it, the value in tabular figures, and its
 * source. The table's tracks are fractions of the card, so every card on the
 * page puts its values and sources on the same two lines, and each group
 * takes a glyph of its own so the cards are told apart at a glance.
 *
 * # Engine settings, not the company's policies
 *
 * These caps were drawn beside the standing policies once. They are not
 * policies the company writes for its agents to read; they are what the engine
 * stops a turn at, so they live here, in Settings' Runtime group, and Company
 * › Policies says where they are.
 *
 * # Changed where they are read
 *
 * Each cap the company document carries (`settable`, the engine's own word
 * for it) has a pencil beside its value, drawn for every reader and disabled
 * with the reason for one who cannot write the document. It opens a field
 * for that one cap: the value is checked against the whole company by the
 * engine before it is stored (`lib/useSettingWrite.ts`), so a number the
 * engine cannot run is refused in the engine's words and nothing is saved.
 * An empty field is the default, which removes the key (`lib/limits.ts`).
 * The value a save wrote is drawn, marked as applying, until this node has
 * applied the revision and the answer carries it. The schedule timeout is
 * the engine's constant and has no pencil: each schedule sets its own.
 */

import {
  Button,
  Card,
  EmptyState,
  FormField,
  IconButton,
  Input,
  PageNote,
  Popover,
  returnFocus,
  SettingsTable,
} from "@crewlethq/ui";
import {
  BellGlyph,
  CalendarClockGlyph,
  GaugeGlyph,
  NetworkGlyph,
  PencilGlyph,
} from "@crewlethq/icons/glyphs";
import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { QueryState } from "~/components/common.tsx";
import { SettingWriteStatus, standingWords } from "~/components/SettingWriteStatus.tsx";
import { fmtSetting } from "~/lib/format.ts";
import { limitPlan, limitText, limitTyping, readLimit, type LimitRow } from "~/lib/limits.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useSettingWrite } from "~/lib/useSettingWrite.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";

/** Each group's heading and glyph, in the engine's drawing order. */
const LIMIT_GROUPS: Readonly<
  Record<LimitRow["group"], { title: string; icon: ReactNode; note?: string }>
> = {
  turn: { title: "Guardrails on every turn", icon: <GaugeGlyph size="sm" /> },
  delegation: { title: "Delegation", icon: <NetworkGlyph size="sm" /> },
  notifications: { title: "Notifications", icon: <BellGlyph size="sm" /> },
  schedules: {
    title: "Schedules",
    icon: <CalendarClockGlyph size="sm" />,
    // THE ONE CAP A SCHEDULE OVERRIDES, said once over its row rather than
    // as a clause that wrapped the row's name onto two lines.
    note: "A schedule may set its own time limit.",
  },
};

/**
 * What each cap is called, by its key in the company document. Words only: a
 * key this build has no words for is drawn by the key itself, so a cap a
 * newer engine adds is still shown.
 */
const LABELS: Readonly<Record<string, string>> = {
  "turn_engine.max_iterations": "Review rounds per turn",
  "turn_engine.onboarding_max_tool_rounds": "Tool rounds on a first turn",
  "turn_engine.delegation_depth_limit": "Delegation depth",
  "turn_engine.delegation.max_parallel": "Workers at once",
  "turn_engine.delegation.max_tasks_per_call": "Tasks per call",
  "turn_engine.delegation.max_turns": "Turns per worker",
  "turn_engine.delegation.budget_fraction": "Share of the budget a call may spend",
  "turn_engine.delegation.task_timeout_seconds": "One task's time limit",
  "turn_engine.delegation.call_timeout_seconds": "One call's time limit",
  notification_rate_limit: "Wake-ups per seat",
  notification_coalesce_max_batch: "Notices gathered into one wake-up",
  "schedules.timeout_seconds": "A scheduled turn's time limit",
};

/**
 * A number of a cap's unit, in words: the value in force, or the default it
 * replaced. A value that switches the limit off says Off, and so does a
 * default of zero for a cap whose zero is off.
 */
function amountWords(row: LimitRow, amount: number, off: boolean): string {
  if (off) return "Off";
  switch (row.unit) {
    case "seconds":
      return fmtSetting(amount);
    case "fraction":
      return `${Math.round(amount * 100)}%`;
    case "per_second":
      return `${amount.toLocaleString()} per second`;
    case "count":
      return amount.toLocaleString();
  }
}

/**
 * Where a cap's value comes from: the engine's default, or a value the
 * company document sets, with the default it replaced.
 */
export function limitSource(row: LimitRow): { changed: boolean; words: string } {
  if (row.value === row.default) return { changed: false, words: "Default" };
  return { changed: true, words: `Changed from ${amountWords(row, row.default, row.default_off)}` };
}

export function Limits() {
  const { data, loading, error, refusal, detail, refetch } = useQuery("limits", undefined, {
    pollMs: 300_000,
  });
  const rows = data?.limits ?? [];
  const groups = (Object.keys(LIMIT_GROUPS) as LimitRow["group"][])
    .map((group) => ({ group, rows: rows.filter((r) => r.group === group) }))
    .filter((g) => g.rows.length > 0);
  return (
    <>
      <PageNote>
        The caps the engine holds the company&rsquo;s work to. Each is a setting of the company
        document: change one here and the engine checks it against the whole company first.
      </PageNote>
      <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
        {data && rows.length === 0 && (
          <EmptyState
            icon={<GaugeGlyph size="xl" />}
            title="No limits to show"
            description="This node runs no company, so no turn runs to be limited."
          />
        )}
        {/* THE SECTION'S OWN RHYTHM: the cards stand in the section body,
            whose gap every Settings page shares, rather than in a stack of
            their own a step tighter. */}
        {groups.map(({ group, rows: caps }) => (
          <LimitGroupCard key={group} group={group} caps={caps} onApplied={refetch} />
        ))}
      </QueryState>
    </>
  );
}

/** One group of caps: a card whose table is named by the card's title. */
function LimitGroupCard({
  group,
  caps,
  onApplied,
}: {
  group: LimitRow["group"];
  caps: LimitRow[];
  onApplied: () => void;
}) {
  const { title, icon, note } = LIMIT_GROUPS[group];
  const titleID = `limits-${group}`;
  return (
    // FLUSH, so the table's row rules reach the card's edges under the
    // head's own rule rather than stopping at a body's inset.
    <Card padding="none">
      <Card.Header icon={icon} subtitle={note}>
        <Card.Title id={titleID}>{title}</Card.Title>
      </Card.Header>
      <SettingsTable aria-labelledby={titleID}>
        {caps.map((cap) => (
          <LimitRowLine key={cap.key} cap={cap} onApplied={onApplied} />
        ))}
      </SettingsTable>
    </Card>
  );
}

/**
 * One cap: its name and key, the value in force with the pencil that changes
 * it, and where the value comes from.
 *
 * WHAT IT SHOWS AFTER A SAVE is the value SAVED, marked as applying, until
 * this node has applied it and the answer the page draws carries it: going
 * back to the served value at once drew the cap being replaced for a beat
 * after the save, which reads as a save that did not take. A node that
 * refused the revision goes on enforcing the value before, so that is what
 * is drawn, marked as refused here.
 */
function LimitRowLine({ cap, onApplied }: { cap: LimitRow; onApplied: () => void }) {
  const label = LABELS[cap.key] ?? cap.key;
  const access = useConfigWriteAccess();
  const write = useSettingWrite({
    reread: "Reload to see the limits as they are now.",
    onApplied,
  });
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState("");
  const [error, setError] = useState<string | undefined>();
  // The value the last save wrote (null: the default), drawn while it applies.
  const [expected, setExpected] = useState<number | null | undefined>(undefined);
  const pencil = useRef<HTMLButtonElement>(null);
  const fieldId = `${useId()}-limit`;

  const saved = write.state.kind === "saved" ? write.state : null;
  const standing = saved?.standing;
  const { reset } = write;
  // SETTLED once the answer re-read after the apply has arrived (the row is a
  // new object, or already says the saved value), and at once where a later
  // activation passed the save: the served value is the truth again.
  const seenAtApply = useRef<LimitRow | null>(null);
  useEffect(() => {
    if (standing === "passed") {
      seenAtApply.current = null;
      reset();
      setExpected(undefined);
      return;
    }
    if (standing !== "applied") {
      seenAtApply.current = null;
      return;
    }
    if (seenAtApply.current === null) seenAtApply.current = cap;
    const wanted = expected === null ? cap.default : expected;
    if (cap.value === wanted || cap !== seenAtApply.current) {
      seenAtApply.current = null;
      reset();
      setExpected(undefined);
    }
  }, [standing, cap, expected, reset]);

  // A SAVE CLOSES THE FIELD: the value beside the pencil says the new cap and
  // that it is applying, which is the answer the person was waiting for.
  useEffect(() => {
    if (!editing || !saved) return;
    setEditing(false);
    returnFocus(pencil.current);
  }, [editing, saved]);

  function open() {
    write.reset();
    setText(cap.value === cap.default ? "" : limitText(cap, cap.value));
    setError(undefined);
    setEditing(true);
  }

  function close() {
    write.reset();
    setEditing(false);
    // FOCUS GOES BACK TO WHAT OPENED IT, handed back rather than focused, so
    // a pencil a pointer opened is not ringed for leaving the field.
    queueMicrotask(() => returnFocus(pencil.current));
  }

  function submit(event?: FormEvent) {
    event?.preventDefault();
    if (write.busy) return;
    // ENTER ON A WARNING IS THE SECOND PRESS the warning asked for.
    if (write.state.kind === "warned") {
      write.confirm();
      return;
    }
    const read = readLimit(cap, text);
    if (!read.ok) {
      setError(read.error);
      return;
    }
    setError(undefined);
    setExpected(read.value);
    write.submit(limitPlan(cap, label, read.value));
  }

  // THE SAVED VALUE while this node applies it; the served one otherwise.
  const drawsSaved =
    saved &&
    (saved.standing === "applying" || saved.standing === "applied") &&
    expected !== undefined;
  const shownValue = drawsSaved ? (expected ?? cap.default) : cap.value;
  // Which zeros switch a cap off is the engine's to say, and it says it of the
  // value in force and of the default: a saved zero reads Off while it applies
  // only where one of those already told this page that zero is off.
  const shown: LimitRow = drawsSaved
    ? { ...cap, value: shownValue, off: shownValue === 0 && (cap.off || cap.default_off) }
    : cap;
  const source = limitSource(shown);
  const mark = saved ? standingWords(saved.standing) : "";
  return (
    <SettingsTable.Row
      label={label}
      settingKey={cap.key}
      source={source.words}
      changed={source.changed}
    >
      <span className="ceiling">
        <span>{amountWords(shown, shown.value, shown.off)}</span>
        {mark && (
          <span className="ceiling-applying t-caption" role="status">
            {mark}
          </span>
        )}
        {cap.settable && (
          // A PANEL ANCHORED TO THE PENCIL, not a field in the cell: the
          // engine's refusal of a value is a sentence, and the value column
          // is a quarter of the card wide.
          <Popover
            label={label}
            align="start"
            className="ceiling-pop"
            open={editing}
            onOpenChange={(next) => (next ? open() : close())}
            trigger={(isOpen, toggle) => (
              <IconButton
                ref={pencil}
                size="sm"
                variant="ghost"
                icon={<PencilGlyph />}
                label={`Change ${label}`}
                aria-expanded={isOpen}
                disabledReason={access.can ? undefined : access.reason}
                title={access.can ? undefined : access.reason}
                onClick={toggle}
              />
            )}
          >
            <form className="ceiling-edit" onSubmit={submit} noValidate>
              <FormField
                htmlFor={fieldId}
                label={label}
                optional
                error={error}
                helper={`${limitTyping(cap)} Empty is the default, ${amountWords(cap, cap.default, cap.default_off)}.`}
              >
                <Input
                  id={fieldId}
                  value={text}
                  autoFocus
                  inputMode="decimal"
                  placeholder={`Default: ${amountWords(cap, cap.default, cap.default_off)}`}
                  autoComplete="off"
                  spellCheck={false}
                  error={Boolean(error)}
                  disabled={write.busy}
                  onChange={(event) => {
                    setText(event.target.value);
                    setError(undefined);
                    if (write.state.kind !== "idle" && !write.busy) write.reset();
                  }}
                />
              </FormField>
              <SettingWriteStatus
                write={write}
                unchanged="Already the limit."
                onReload={() => {
                  onApplied();
                  close();
                }}
              />
              <div className="ceiling-actions">
                <Button type="button" size="small" variant="ghost" onClick={close}>
                  Cancel
                </Button>
                <Button
                  type={write.state.kind === "warned" ? "button" : "submit"}
                  size="small"
                  variant="primary"
                  loading={write.busy}
                  onClick={write.state.kind === "warned" ? write.confirm : undefined}
                >
                  {write.state.kind === "warned" ? "Save anyway" : "Save"}
                </Button>
              </div>
            </form>
          </Popover>
        )}
      </span>
    </SettingsTable.Row>
  );
}
