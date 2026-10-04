/**
 * Everything about the task that is not its prose, down its right-hand side:
 * its fields, what it is related to, the pages it cites, what it has cost and
 * who follows it.
 *
 * # Every field a writer can change is changed where it is read
 *
 * A writer's value IS the control — the status mark opens the statuses, the
 * labels open the project's labels, a date opens a date — through the page's
 * one write ([useItemEdits]), conditional on the version the page was drawn
 * from. A reader sees the same values, and the page says once why they cannot
 * change them. Each row still says who last set it and when, from the task's
 * own history ([attribution]).
 *
 * # What this task has cost, in tokens
 *
 * The task's own counters — every turn the engine charged to it (ADR-0022) —
 * so the cost here, the turn cards' numbers and the board card's figure are one
 * count. Tokens and agent time only: this product renders no money.
 *
 * # An absent value is an em dash, never a zero
 *
 * Zero is a measurement: an unestimated task drawn as "0m" and an unheld one
 * as a blank would be two claims nobody wrote down. A task with no turns yet
 * has cost nothing, which IS a measurement, and says so in words.
 */

import { useMemo, useState, type ReactNode } from "react";
import { Button, EmptyValue, IconButton, Input, Menu, Tag, cx } from "@crewlethq/ui";
import {
  ArrowUpRightGlyph,
  CalendarGlyph,
  EyeGlyph,
  EyeOffGlyph,
  FileTextGlyph,
  LinkGlyph,
  PencilGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { PropertiesRail, type PropertyGroup } from "~/app/frame/PropertiesRail.tsx";
import type { SetBy } from "~/app/frame/ObjectHeader.tsx";
import { pathOf } from "~/app/frame/objects.ts";
import { SeatChip } from "~/components/common.tsx";
import { AssignButton } from "~/components/writes.tsx";
import { PriorityMark, StatusMark, TypeIcon, type RowChrome } from "~/components/work.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { attribution, type ChangeField } from "~/lib/attribution.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import {
  fmtCount,
  fmtDate,
  fmtDateTime,
  fmtDuration,
  humanize,
  inTime,
  plural,
  relTime,
} from "~/lib/format.ts";
import {
  PRIORITIES,
  STATUSES,
  fieldValueState,
  fieldValueText,
  fmtMinutes,
  statusLabel,
  typeName,
  itemPath,
  linkedItem,
} from "~/lib/work.ts";
import type {
  WorkFieldDef,
  WorkFieldValue,
  WorkItemDetail,
  WorkLink,
  WorkProjectDetail,
  WorkUnitRef,
} from "~/protocol/index.ts";
import { useItemEdits } from "./edit.tsx";

/**
 * What a relation is CALLED from this end.
 *
 * The derived half of a dependency is the one nobody authored — `waiting_on`
 * mirrored onto the blocker — and calling both ends "waiting on" would tell a
 * reader their task is blocked by the very task it is blocking. So the word
 * comes from the pair (kind, derived) rather than from the kind alone.
 */
export function linkHeading(link: WorkLink): string {
  switch (link.kind) {
    case "waiting_on":
      return link.derived ? "Blocks" : "Waiting on";
    case "duplicates":
      return link.derived ? "Duplicated by" : "Duplicates";
    case "page":
      return "Pages";
    default:
      return "Related";
  }
}

export function ItemRail({
  detail,
  chrome,
  project,
  now,
  compact,
}: {
  detail: WorkItemDetail;
  chrome: RowChrome;
  project?: WorkProjectDetail | null;
  now: number;
  /** The peek's column: the page's rail less the cost and the record. */
  compact?: boolean;
}) {
  const item = detail.task;
  const edits = useItemEdits();
  const can = Boolean(edits?.can);
  const seatName = chrome.seatName ?? ((h: string) => h);
  const seatKind = chrome.seatKind ?? (() => undefined);
  const setBy = useMemo(() => attribution(detail.history), [detail.history]);
  // WHOEVER THE RECORD NAMES (`iam.ActorFor`): a person the identity
  // directory binds to a seat files AS that seat, so the reporter is them.
  const reporter = item.reporter;
  // NAMED THE WAY THE ROWS BESIDE IT NAME PEOPLE: the change log carries a
  // handle, and a line reading `agent-ceo · 21h ago` under a Reporter row
  // saying "Agent CEO" is one person with two names on one screen.
  const by = (field: ChangeField): SetBy | undefined => {
    const who = setBy.get(field);
    return who
      ? { ...who, actor: seatName(who.actor), ago: who.at ? relTime(who.at, now) : undefined }
      : undefined;
  };
  const defs = useMemo(() => {
    const map = new Map<string, WorkFieldDef>();
    for (const group of project?.fields ?? []) {
      for (const field of group.fields) map.set(field.id, field);
    }
    return map;
  }, [project]);
  const tagLabel = (slug: string) => project?.tags?.find((t) => t.slug === slug)?.label ?? slug;

  const groups: PropertyGroup[] = [
    {
      properties: [
        {
          label: "Status",
          value: (
            <Pick
              label={`Status of ${item.key}`}
              current={item.status}
              options={STATUSES.map((s) => ({
                value: s.value,
                label: statusLabel(s.value, chrome.statuses),
              }))}
              onPick={(next) =>
                edits?.edit(
                  { status: next },
                  `Moved ${item.key} to ${statusLabel(next, chrome.statuses)}`,
                )
              }
            >
              <span className="row gap-2">
                <StatusMark status={item.status} />
                {statusLabel(item.status, chrome.statuses)}
              </span>
            </Pick>
          ),
          setBy: by("status"),
        },
        {
          label: "Priority",
          // `none` IS A VALUE THE ENGINE MINTS, not an absence — a create
          // defaults the field to it and the change log records it — so the
          // rail answers what the field is SET TO in the word. An absent
          // priority is a different case and still dashes.
          value: !item.priority ? undefined : (
            <Pick
              label={`Priority of ${item.key}`}
              current={item.priority}
              options={PRIORITIES.map((p) => ({
                value: p,
                label: p === "none" ? "No priority" : humanize(p),
              }))}
              onPick={(next) =>
                edits?.edit({ priority: next }, `Set ${item.key} to ${next} priority`)
              }
            >
              {item.priority === "none" ? (
                <span>No priority</span>
              ) : (
                <PriorityMark priority={item.priority} word />
              )}
            </Pick>
          ),
          setBy: by("priority"),
        },
        {
          label: "Assignee",
          value: (
            <span className="row gap-2 task-rail-assignee">
              {item.assignee ? (
                <>
                  <SeatChip
                    name={seatName(item.assignee)}
                    handle={item.assignee}
                    kind={seatKind(item.assignee)}
                  />
                  <Tag appearance="outline" size="xs">
                    {seatKind(item.assignee) === "human" ? "person" : "agent"}
                  </Tag>
                </>
              ) : (
                <span className="muted">Unassigned</span>
              )}
              {/* THE HAND-OFF HAS ITS OWN DIALOG — who, and one line saying
                  why, which is what the new holder is woken with — and it
                  is never hidden from a reader who cannot make it. Not on a
                  removed task: assigning what nobody can see on a board
                  reaches nobody, and the page offers Restore instead. */}
              {!item.removed && (
                <AssignButton
                  item={item.key}
                  version={item.version}
                  assignee={item.assignee ?? ""}
                />
              )}
            </span>
          ),
          setBy: by("assignee"),
        },
        {
          label: "Reporter",
          value: reporter ? (
            <SeatChip name={seatName(reporter)} handle={reporter} kind={seatKind(reporter)} />
          ) : undefined,
          setBy: by("reporter"),
        },
        {
          label: "Project",
          // THE KEY BESIDE THE NAME: the name is what a person calls it and
          // the key is what every item, board and link is addressed by. The
          // project read is the one that supplies this rail's vocabulary, so a
          // rail still loading shows the key alone rather than waiting.
          value: (
            <span className="row gap-2">
              <span className="project-key mono">{item.project}</span>
              {project?.name && <span className="truncate">{project.name}</span>}
            </span>
          ),
          path: ["work", item.project],
          setBy: by("project"),
        },
        {
          label: "Type",
          value: item.type ? (
            <span className="row gap-1">
              <TypeIcon type={item.type} types={chrome.types} decorative />
              {typeName(item.type, chrome.types)}
            </span>
          ) : undefined,
          setBy: by("type"),
        },
        {
          label: "Labels",
          value: (
            <LabelsValue
              current={item.tags ?? []}
              declared={(project?.tags ?? []).filter((t) => !t.archived)}
              labelOf={tagLabel}
              itemKey={item.key}
            />
          ),
          setBy: by("tags"),
        },
        {
          label: "Due",
          value: (
            <DateValue
              label={`Due date of ${item.key}`}
              at={item.due_at}
              now={now}
              allDay={item.due_all_day}
              standing={detail.due_standing}
              onSave={(day) =>
                edits?.edit(
                  { due: day },
                  day ? `Set ${item.key} due ${fmtDate(day)}` : `Cleared ${item.key}'s due date`,
                )
              }
            />
          ),
          setBy: by("due"),
        },
        ...(item.start_at || can
          ? [
              {
                label: "Start",
                value: (
                  <DateValue
                    label={`Start date of ${item.key}`}
                    at={item.start_at}
                    now={now}
                    onSave={(day) =>
                      edits?.edit(
                        { start: day },
                        day
                          ? `Set ${item.key} to start ${fmtDate(day)}`
                          : `Cleared ${item.key}'s start`,
                      )
                    }
                  />
                ),
                setBy: by("start"),
              },
            ]
          : []),
        {
          label: "Estimate",
          value: (
            <EstimateValue
              points={item.points}
              minutes={item.estimate_minutes}
              itemKey={item.key}
            />
          ),
          setBy: by("points") ?? by("estimate"),
        },
      ],
    },
  ];

  if ((detail.fields ?? []).length > 0) {
    groups.push({
      name: "Fields",
      properties: (detail.fields ?? []).map((field) => ({
        label: field.name || field.slug || field.id,
        value: <FieldValueRow field={field} defs={defs} seatName={seatName} itemKey={item.key} />,
      })),
    });
  }

  // THE RECORD IS READ LAST — when and where the task was filed is what a
  // reader looks up, not what they come to the rail for — so it closes the
  // rail rather than sitting between the fields and the relations.
  const record: PropertyGroup[] = [];
  if (!compact) {
    record.push({
      name: "Record",
      properties: [
        { label: "Created", value: item.created_at ? fmtDateTime(item.created_at) : undefined },
        { label: "Updated", value: item.updated_at ? fmtDateTime(item.updated_at) : undefined },
        {
          label: "In status since",
          value: item.status_entered_at ? fmtDateTime(item.status_entered_at) : undefined,
        },
        ...(item.done_at ? [{ label: "Delivered", value: fmtDateTime(item.done_at) }] : []),
        {
          label: "Filed into",
          value: item.filed_unit ? (
            <UnitValue unit={detail.units?.filed} stored={item.filed_unit} />
          ) : (
            <span className="muted">no unit</span>
          ),
        },
        // ROUTING IS THE MUTABLE HALF — whose lead hears about this now —
        // shown only where it has MOVED: the pair being equal is the
        // ordinary case and repeating it is noise.
        ...(item.routing_unit && item.routing_unit !== item.filed_unit
          ? [
              {
                label: "Routes to",
                value: <UnitValue unit={detail.units?.routing} stored={item.routing_unit} />,
                setBy: by("routing_unit"),
              },
            ]
          : []),
        ...((item.former_keys ?? []).length > 0
          ? [
              {
                label: "Former keys",
                value: <span className="mono">{(item.former_keys ?? []).join(", ")}</span>,
              },
            ]
          : []),
      ],
    });
  }

  return (
    <div className="task-rail-body">
      <PropertiesRail groups={groups} />
      <Relations detail={detail} chrome={chrome} />
      <PageLinks links={(detail.links ?? []).filter((l) => l.kind === "page")} />
      {!compact && <Cost detail={detail} />}
      <Watching detail={detail} chrome={chrome} />
      {record.length > 0 && (
        <div className="task-rail-section">
          <PropertiesRail groups={record} />
        </div>
      )}
    </div>
  );
}

/**
 * A writer's picker, drawn AS THE VALUE: the kit's menu, whose trigger is the
 * mark the rail always drew, so a writer's rail reads exactly like a reader's
 * and the choices open under the value they change.
 */
function Pick({
  label,
  current,
  options,
  onPick,
  children,
}: {
  label: string;
  current: string;
  options: readonly { value: string; label: string }[];
  onPick: (value: string) => unknown;
  children: ReactNode;
}) {
  const edits = useItemEdits();
  if (!edits?.can) return <>{children}</>;
  return (
    <span className="cell-edit">
      <Menu
        label={label}
        trigger={
          <>
            <span className="sr-only">{label}: </span>
            {children}
          </>
        }
        items={options.map((option) => ({
          key: option.value || "-",
          label: option.label,
          checked: option.value === current,
          disabled: edits.busy,
          onSelect: () => {
            if (option.value !== current) void onPick(option.value);
          },
        }))}
      />
    </span>
  );
}

/** The labels, and for a writer the project's declared set to tick. */
function LabelsValue({
  current,
  declared,
  labelOf,
  itemKey,
}: {
  current: string[];
  declared: { slug: string; label: string }[];
  labelOf: (slug: string) => string;
  itemKey: string;
}) {
  const edits = useItemEdits();
  const chips =
    current.length > 0 ? (
      <span className="row wrap gap-1">
        {current.map((slug) => (
          <Tag key={slug} appearance="outline" size="sm">
            {labelOf(slug)}
          </Tag>
        ))}
      </span>
    ) : (
      <EmptyValue label="No labels" />
    );
  // THE PROJECT'S OWN SET, and nothing else: `labels` REPLACES the task's,
  // and every one must be a tag the project declares — the engine refuses the
  // rest rather than inventing a grouping somebody mistyped.
  if (!edits?.can || declared.length === 0) return chips;
  return (
    <span className="cell-edit">
      <Menu
        label={`Labels of ${itemKey}`}
        trigger={
          <>
            <span className="sr-only">{`Labels of ${itemKey}: `}</span>
            {chips}
          </>
        }
        items={declared.map((tag) => {
          const on = current.includes(tag.slug);
          return {
            key: tag.slug,
            label: tag.label,
            checked: on,
            disabled: edits.busy,
            onSelect: () => {
              const next = on ? current.filter((s) => s !== tag.slug) : [...current, tag.slug];
              void edits.edit(
                { labels: next },
                `${on ? "Took" : "Put"} “${tag.label}” ${on ? "off" : "on"} ${itemKey}`,
              );
            },
          };
        })}
      />
    </span>
  );
}

/** A day: the date and how far off it is — and for a writer, a date to pick. */
/**
 * A due date's distance in CALENDAR days on the company's clock, as the
 * engine stood it: "today", "tomorrow", "in 42 days", "2 days overdue".
 *
 * OVERDUE IS THE ENGINE'S WORD, not the sign of `days`: finished work past
 * its date is not late, it is done, and it reads "2 days ago" like any other
 * past date. Whole days rather than the shared clock's hours, because a date
 * is a day on a calendar — "1d ago" for a task due the day before yesterday
 * was an instant's distance read against the reader's own midnight.
 */
export function standingWords(standing: { days: number; overdue?: boolean }): string {
  const { days, overdue } = standing;
  if (days === 0) return "today";
  if (days === 1) return "tomorrow";
  if (days > 1) return `in ${days} days`;
  const late = -days;
  if (overdue) return `${plural(late, "day")} overdue`;
  return late === 1 ? "yesterday" : `${late} days ago`;
}

function DateValue({
  label,
  at,
  now,
  allDay,
  standing,
  onSave,
}: {
  label: string;
  at?: string;
  now: number;
  allDay?: boolean;
  /** The engine's standing of a DUE date on the company's calendar — see
   *  [standingWords]. A start date has none, and reads its distance off the
   *  shared clock instead. */
  standing?: { days: number; overdue?: boolean };
  /** A `YYYY-MM-DD`, or null to clear. */
  onSave: (day: string | null) => unknown;
}) {
  const edits = useItemEdits();
  const [draft, setDraft] = useState<string | null>(null);
  if (draft !== null && edits?.can) {
    return (
      <form
        className="task-rail-edit"
        onSubmit={(event) => {
          event.preventDefault();
          void Promise.resolve(onSave(draft || null)).then(() => setDraft(null));
        }}
      >
        <Input
          type="date"
          aria-label={label}
          value={draft}
          autoFocus
          onChange={(event) => setDraft(event.target.value)}
        />
        <Button size="small" variant="primary" type="submit" loading={edits.busy}>
          Save
        </Button>
        <Button size="small" variant="ghost" onClick={() => setDraft(null)}>
          Cancel
        </Button>
      </form>
    );
  }
  // THE DISTANCE, never the date a second time: past a month the shared
  // clock's relative reading IS the absolute date, and the rail printed
  // "Nov 09, 2026" twice. A due date takes the engine's words instead.
  const absolute = at ? fmtDate(at) : "";
  const distance = !at
    ? ""
    : standing
      ? standingWords(standing)
      : Date.parse(at) >= now
        ? inTime(at, now)
        : relTime(at, now);
  const shown = at ? (
    <span className={cx("task-date", standing?.overdue && "overdue")}>
      <CalendarGlyph size="xs" />
      <span title={fmtDateTime(at)}>{absolute}</span>
      {distance && distance !== absolute && (
        <span className={standing?.overdue ? undefined : "muted"}>{distance}</span>
      )}
      {allDay && <span className="muted">all day</span>}
    </span>
  ) : undefined;
  if (!edits?.can) return shown ?? <EmptyValue />;
  return (
    <span className="row gap-1">
      {shown ?? <EmptyValue />}
      <IconButton
        size="sm"
        variant="ghost"
        className="task-edit"
        label={`Change the ${label.toLowerCase()}`}
        icon={<PencilGlyph size="xs" />}
        onClick={() => setDraft(at ? at.slice(0, 10) : "")}
      />
    </span>
  );
}

/** The size: points, time, or both — and for a writer, both to set. */
function EstimateValue({
  points,
  minutes,
  itemKey,
}: {
  points?: number;
  minutes?: number;
  itemKey: string;
}) {
  const edits = useItemEdits();
  const [draft, setDraft] = useState<{ points: string; minutes: string } | null>(null);
  if (draft !== null && edits?.can) {
    const toNumber = (raw: string) => (raw.trim() === "" ? null : Number(raw));
    const p = toNumber(draft.points);
    const m = toNumber(draft.minutes);
    const bad = [p, m].some((v) => v !== null && (!Number.isFinite(v) || v < 0));
    return (
      <form
        className="task-rail-edit"
        onSubmit={(event) => {
          event.preventDefault();
          if (bad) return;
          void edits
            .edit({ points: p, estimate_minutes: m }, `Sized ${itemKey}`)
            .then((r) => r && (r.kind === "applied" || r.kind === "pending") && setDraft(null));
        }}
      >
        <Input
          type="number"
          min={0}
          aria-label={`Points of ${itemKey}`}
          placeholder="points"
          value={draft.points}
          onChange={(event) => setDraft({ ...draft, points: event.target.value })}
        />
        <Input
          type="number"
          min={0}
          aria-label={`Estimate of ${itemKey} in minutes`}
          placeholder="minutes"
          value={draft.minutes}
          onChange={(event) => setDraft({ ...draft, minutes: event.target.value })}
        />
        <Button
          size="small"
          variant="primary"
          type="submit"
          loading={edits.busy}
          disabledReason={bad ? "A size is a number, zero or more." : undefined}
        >
          Save
        </Button>
        <Button size="small" variant="ghost" onClick={() => setDraft(null)}>
          Cancel
        </Button>
      </form>
    );
  }
  const parts = [
    points ? `${points} ${points === 1 ? "point" : "points"}` : "",
    minutes ? fmtMinutes(minutes) : "",
  ].filter(Boolean);
  const shown = parts.length > 0 ? <span>{parts.join(" · ")}</span> : undefined;
  if (!edits?.can) return shown ?? <EmptyValue label="Not estimated" />;
  return (
    <span className="row gap-1">
      {shown ?? <EmptyValue label="Not estimated" />}
      <IconButton
        size="sm"
        variant="ghost"
        className="task-edit"
        label={`Change the estimate of ${itemKey}`}
        icon={<PencilGlyph size="xs" />}
        onClick={() =>
          setDraft({
            points: points ? String(points) : "",
            minutes: minutes ? String(minutes) : "",
          })
        }
      />
    </span>
  );
}

/**
 * A custom field's value — and for a writer, the value to set, in the shape
 * its declaration takes: one of its options, a box to tick, or the value as
 * text, which the engine checks against the declaration and refuses naming the
 * rule rather than rounding or coercing it to fit.
 */
function FieldValueRow({
  field,
  defs,
  seatName,
  itemKey,
}: {
  field: WorkFieldValue;
  defs: Map<string, WorkFieldDef>;
  seatName: (handle: string) => string;
  itemKey: string;
}) {
  const edits = useItemEdits();
  const [draft, setDraft] = useState<string | null>(null);
  const def = defs.get(field.id);
  const state = fieldValueState(field);
  const unset = field.value === null || field.value === undefined || field.value === "";
  const name = field.name || field.slug || field.id;
  // A FIELD NOTHING DECLARES, or one another tracker wrote, is read and never
  // set from here: there is no declaration to check a value against.
  const editable = Boolean(edits?.can && def && !def.archived && field.slug && !state);
  const set = (value: unknown, done: string) =>
    edits?.edit({ fields: { [field.slug!]: value } }, done);
  const shown = (
    <>
      {unset ? <EmptyValue label="Not set" /> : fieldValueText(field, defs, seatName)}
      {state && <span className="work-field-state">{state}</span>}
    </>
  );
  if (!editable || !def) return shown;
  const options = def.config?.options ?? [];
  if (def.type === "dropdown" && options.length > 0) {
    return (
      <Pick
        label={`${name} of ${itemKey}`}
        current={String(field.value ?? "")}
        options={[
          { value: "", label: "Not set" },
          ...options.map((o) => ({ value: o.id, label: o.name })),
        ]}
        onPick={(next) => set(next || null, `Set ${name} on ${itemKey}`)}
      >
        {shown}
      </Pick>
    );
  }
  if (def.type === "checkbox") {
    return (
      <Pick
        label={`${name} of ${itemKey}`}
        current={field.value === true ? "yes" : field.value === false ? "no" : ""}
        options={[
          { value: "yes", label: "Yes" },
          { value: "no", label: "No" },
        ]}
        onPick={(next) => set(next === "yes", `Set ${name} on ${itemKey}`)}
      >
        {shown}
      </Pick>
    );
  }
  if (draft !== null) {
    return (
      <form
        className="task-rail-edit"
        onSubmit={(event) => {
          event.preventDefault();
          void Promise.resolve(
            set(draft.trim() === "" ? null : draft.trim(), `Set ${name} on ${itemKey}`),
          ).then((r) => r && (r.kind === "applied" || r.kind === "pending") && setDraft(null));
        }}
      >
        <Input
          type={def.type === "date" && !def.config?.time ? "date" : "text"}
          aria-label={`${name} of ${itemKey}`}
          value={draft}
          autoFocus
          onChange={(event) => setDraft(event.target.value)}
        />
        <Button size="small" variant="primary" type="submit" loading={edits?.busy}>
          Save
        </Button>
        <Button size="small" variant="ghost" onClick={() => setDraft(null)}>
          Cancel
        </Button>
      </form>
    );
  }
  return (
    <span className="row gap-1">
      {shown}
      <IconButton
        size="sm"
        variant="ghost"
        className="task-edit"
        label={`Change ${name}`}
        icon={<PencilGlyph size="xs" />}
        onClick={() =>
          setDraft(
            unset ? "" : Array.isArray(field.value) ? field.value.join(", ") : String(field.value),
          )
        }
      />
    </span>
  );
}

/**
 * A team this item names, as a person reads it and as a link to its work.
 *
 * THE NAME IS WHAT IS DRAWN AND THE KEY IS WHAT IS FOLLOWED — the key is what
 * the row holds, and the engine matches it against the whole set of that
 * team's spellings. A reference the chart no longer has is a finding, drawn in
 * a warning tag and unlinked.
 */
function UnitValue({ unit, stored }: { unit?: WorkUnitRef; stored: string }) {
  if (unit && unit.resolved === false) {
    return (
      <Tag variant="warning" appearance="outline" title="The current org chart has no such unit">
        {unit.key || stored}
      </Tag>
    );
  }
  const key = unit?.key || stored;
  return (
    <a className="t-link truncate" href={href(["work"], { unit: key })}>
      {unit?.name || key}
    </a>
  );
}

/**
 * What the task is attached to, by what each relation MEANS from this end —
 * the task it is filed under first, then its dependencies and relations.
 */
export function Relations({ detail, chrome }: { detail: WorkItemDetail; chrome: RowChrome }) {
  const links = (detail.links ?? []).filter((l) => l.kind !== "page");
  const parent = detail.parent;
  if (links.length === 0 && !parent && !detail.task.parent) return null;
  return (
    <section className="task-rail-section" aria-label="Relations">
      <div className="task-lbl">Relations</div>
      {(parent || detail.task.parent) && (
        <a
          className="task-rail-link"
          href={href(parent ? itemPath(parent) : ["work", detail.task.parent!])}
        >
          <ArrowUpRightGlyph size="xs" />
          <span className="muted">Part of</span>
          <span className="work-key mono">{parent?.key || detail.task.parent}</span>
          {parent?.title && <span className="truncate">{parent.title}</span>}
        </a>
      )}
      {links.map((link) => (
        <div key={`${link.kind}:${link.other}`} className="task-rail-link-row">
          <a className="task-rail-link" href={href(itemPath(linkedItem(link)))}>
            <LinkGlyph size="xs" />
            <span className="muted">{linkHeading(link)}</span>
            <span className="work-key mono">{link.key || link.other}</span>
            <span className="truncate">{link.title}</span>
          </a>
          {/* THE OTHER END'S STATE AS ITS MARK, with its word on hover and
              to a screen reader: a badge beside a key and a title in a 316px
              rail squeezed the title to three letters. */}
          {link.status && (
            <span className="task-rail-status" title={statusLabel(link.status, chrome.statuses)}>
              <StatusMark status={link.status} />
              <span className="sr-only">{statusLabel(link.status, chrome.statuses)}</span>
            </span>
          )}
          {/* THE REPAIR AND THE FINDING ARE DIFFERENT STATES: the first is a
              commit the tracker duty writes shortly, the second an edge whose
              mirror was refused for good. */}
          {link.one_sided && !link.one_sided_final && (
            <Tag variant="info" size="xs" title="The duty writes the other half shortly">
              mirror pending
            </Tag>
          )}
          {link.one_sided_final && (
            <Tag variant="warning" size="xs" title="The other end is gone, removed, or full">
              one-sided
            </Tag>
          )}
        </div>
      ))}
    </section>
  );
}

/**
 * The pages the task cites, by their titles.
 *
 * A PAGE IS ADDRESSED BY THE FRAME'S OWN MAP — `#/knowledge/pages/{id}` — and
 * a page edge's `other` IS that id: the tracker resolves the far end of an edge
 * against its own tasks only, so on a page edge the key and title are empty and
 * the title is the page's own answer.
 */
function PageLinks({ links }: { links: WorkLink[] }) {
  if (links.length === 0) return null;
  return (
    <section className="task-rail-section" aria-label="Pages">
      <div className="task-lbl">Pages</div>
      {links.map((link) => (
        <PageLink key={link.other} id={link.other} />
      ))}
    </section>
  );
}

function PageLink({ id }: { id: string }) {
  const page = useQuery("page", { id }, { pollMs: 300_000 });
  return (
    <a className="task-rail-link" href={href(pathOf({ kind: "page", id }))}>
      <FileTextGlyph size="xs" />
      <span className="truncate">
        {page.data?.page?.title || (page.error ? "A page this node cannot read" : id)}
      </span>
    </a>
  );
}

/** "What this task has cost": turns, tokens and agent time, and the hand-offs. */
export function Cost({ detail }: { detail: WorkItemDetail }) {
  const item = detail.task;
  const spend = item.spend;
  const handoffs = item.reassignments ?? 0;
  const budget = detail.reassignment_budget;
  return (
    <section className="task-rail-section" aria-label="What this task has cost">
      <div className="task-lbl">What this task has cost</div>
      {/* ANY FIGURE IS A CHARGE. A segment continuing a turn counted on
          another item charges tokens and time with no turn, and gating the
          panel on the turn count alone called that task free. The term comes
          first in each group, as a definition list requires, and the style
          lifts the figure above it. */}
      {spend && (spend.turns || spend.tokens || spend.wall_ms) ? (
        <dl className="task-cost">
          <div>
            <dt>{spend.turns === 1 ? "turn" : "turns"}</dt>
            <dd className="task-num">{fmtCount(spend.turns ?? 0)}</dd>
          </div>
          <div>
            <dt>tokens</dt>
            <dd className="task-num">{fmtCount(spend.tokens ?? 0)}</dd>
          </div>
          <div>
            <dt>agent time</dt>
            <dd className="task-num">
              {spend.wall_ms ? fmtDuration(spend.wall_ms) : <EmptyValue label="Not measured" />}
            </dd>
          </div>
        </dl>
      ) : (
        <p className="t-caption">No agent turn has been charged to it yet.</p>
      )}
      {/* THE HAND-OFF COUNT IS A BUDGET, not trivia: a task handed on too many
          times has stopped being work and started being a hot potato, and the
          engine refuses the next hand-off rather than letting it circle. The
          limit is the ENGINE'S, served beside the count, and the warning is
          where the engine starts refusing. */}
      <p className="task-handoffs t-caption">
        {budget === undefined ? `${handoffs} hand-offs` : `Hand-offs ${handoffs} of ${budget}`}
        {budget !== undefined && handoffs >= budget && (
          <span className="task-handoffs-full">
            <TriangleAlertGlyph size="xs" /> the next hand-off will be refused
          </span>
        )}
      </p>
    </section>
  );
}

/**
 * Who follows the task, and — for the reader — following it or not.
 *
 * MUTED IS NOT THE SAME AS NOT WATCHING, and both travel: a set that carried
 * only the difference would silently re-add everybody on the next mention.
 */
function Watching({ detail, chrome }: { detail: WorkItemDetail; chrome: RowChrome }) {
  const item = detail.task;
  const edits = useItemEdits();
  const viewer = useViewer();
  const watchers = item.watchers ?? [];
  const muted = new Set(item.muted ?? []);
  // UNDER THE NAME A WATCH IS WRITTEN UNDER (`iam.ActorFor`): a person the
  // directory binds to a seat follows AS the seat, anybody bound to none under
  // their login — so an unbound reader who follows the task reads as following
  // it, where by the seat they hold none of they never did.
  const me = viewer.owner;
  const mine = me !== "" && watchers.includes(me) && !muted.has(me);
  const seatName = chrome.seatName ?? ((h: string) => h);
  return (
    <section className="task-rail-section task-watching" aria-label="Watching">
      <span className="task-lbl">Watching</span>
      <span className="task-watchers">
        {watchers.length === 0 ? (
          <span className="t-caption">Nobody</span>
        ) : (
          watchers.map((handle) => (
            <span key={handle} title={`${seatName(handle)}${muted.has(handle) ? " (muted)" : ""}`}>
              <SeatAvatar
                name={seatName(handle)}
                kind={chrome.seatKind?.(handle) === "human" ? "human" : "agent"}
                size="xs"
              />
            </span>
          ))
        )}
      </span>
      {edits && (
        <Button
          size="small"
          variant="ghost"
          leadingIcon={mine ? <EyeOffGlyph size="sm" /> : <EyeGlyph size="sm" />}
          loading={edits.busy}
          disabledReason={edits.can ? undefined : edits.reason}
          onClick={() =>
            void edits.edit(
              { watch: !mine },
              mine ? `Stopped watching ${item.key}` : `Watching ${item.key}`,
              false,
            )
          }
        >
          {mine ? "Unwatch" : "Watch"}
        </Button>
      )}
      {watchers.some((h) => muted.has(h)) && (
        <span className="t-caption">
          {`Muted: ${watchers
            .filter((h) => muted.has(h))
            .map(seatName)
            .join(", ")}`}
        </span>
      )}
    </section>
  );
}
