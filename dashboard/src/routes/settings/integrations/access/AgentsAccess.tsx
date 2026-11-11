/**
 * Every agent's access to one integration: on or off, the access it holds in
 * the vendor's own terms, its scope where the vendor has one, and what the
 * last sync found. Changes are a draft until reviewed; the review shows what
 * the next sync does at the vendor, step by step and by whom, and saving
 * writes one revision of the company document and starts a sync.
 *
 * Drawn on an integration's Agents tab for every agent, and on an agent's own
 * page for that agent alone (`only`), one component for both (D13).
 *
 * A ROW SAYS WHAT NOTHING HERE CAN DO ON ITS OWN: a vendor that refused, a
 * step only a person can take, an account somebody changed at the vendor by
 * hand, each with the one choice it offers. Its details are one list at one
 * size (option A): the account, the access it holds, its key, its tool and its
 * last sync, with only the actions the seat can take now.
 *
 * WHERE THE OPERATOR WRITES THE DOCUMENT, access settings live on each
 * agent's Seat object, so the tab says so in one line and draws nothing that
 * writes a revision (no switch, picker, scope, Keep or save). Actions at the
 * vendor write none and still run: rotate, remove, restore, retry, check
 * again and install.
 */

import { memo, useId, useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  DescriptionList,
  IconButton,
  Inline,
  InlineCode,
  Link,
  Select,
  SelectionBar,
  Spinner,
  Switch,
  Tag,
  Text,
  VisuallyHidden,
  type DataGridColumn,
  type DataGridRowTone,
  type SelectOption,
} from "@crewlethq/ui";
import { KeyGlyph, PencilGlyph, PlusGlyph } from "@crewlethq/icons/glyphs";
import type {
  AccessAnswer,
  AccessDesired,
  AccessLaneKey,
  AccessSeat,
  AccessSeatAction,
  AccessSeatLane,
} from "~/contract/access.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { asSentence, fmtDate, relTime } from "~/lib/format.ts";
import { handleLabel } from "~/lib/seats.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { Clocked } from "../Clocked.tsx";
import "~/styles/access.css";
import {
  accessApi,
  refusalParts,
  refusalWords,
  type AccessDraft,
  type PlacedRefusal,
} from "./api.ts";
import {
  PermissionsDialog,
  RemoveDialog,
  ReviewDialog,
  RotateDialog,
  ScopeDialog,
} from "./Dialogs.tsx";
import { HandoffSheet } from "./HandoffSheet.tsx";
import {
  accessWords,
  availableActions,
  boxSettingsFact,
  ceilingNote,
  clockValue,
  credentialNote,
  emptyScopeHint,
  handoffOf,
  joinLevels,
  lanesOf,
  levelIds,
  levelName,
  managedOutside,
  MANAGED_OUTSIDE,
  offNote,
  levelsHeading,
  levelSummary,
  noToolRemedy,
  onlyScope,
  offSentence,
  permissionCount,
  permissionWords,
  productOnly,
  productOnlyNote,
  providedExternally,
  refusedLevel,
  refusedScope,
  rotateHandoffOf,
  sameDesired,
  scopeApplies,
  scopeLabel,
  scopeUnavailable,
  scopeWords,
  statusOf,
  statusRank,
  toolWords,
  type Handoff,
  type Lane,
  type SeatAction,
  type Status,
} from "./logic.ts";
import { laneWords, RETIRED_GENERIC } from "./words.ts";

export interface AgentsAccessProps {
  /** The integration's key, which the access routes are keyed on. */
  kind: string;
  /** The integration in the reader's own words. */
  name: string;
  data: AccessAnswer;
  /** Read again: after a write that was refused, or one that answered nothing to draw. */
  reload: () => void;
  /** Draw the answer a write returned, which is the tab as it now stands. */
  onAnswer?: ((answer: AccessAnswer) => void) | undefined;
  /** Why nothing here may be changed (a managed document, a reader who is not an admin), or undefined. */
  held: string | undefined;
  /** The one line a read-only tab shows (the operator manages access), or undefined. */
  readOnly?: string | undefined;
  avatarOf: (handle: string) => AgentAvatar | null;
  /** Draw this agent alone, as its own page does. */
  only?: string | undefined;
  /** The integration's own key replacement, for the shared-credential notice. */
  onReplaceKey?: (() => void) | undefined;
  /** The gesture a person takes at the vendor for one seat (GitHub's create and install), where there is one. */
  stepFor?: ((handle: string) => ReactNode) | undefined;
}

/** One row of the table: one agent on one product. */
interface AccessRow {
  key: string;
  seat: AccessSeat;
  lane: Lane;
  saved: AccessSeatLane;
  want: AccessDesired;
  drafted: boolean;
  /** The agent's first product, where its name is drawn. */
  first: boolean;
  status: Status;
}

/** The rail a row's status takes: the kit's three, and none for a row at rest. */
function toneOf(status: Status): DataGridRowTone | undefined {
  if (status.busy) return "info";
  return status.tone === "danger" || status.tone === "warning" ? status.tone : undefined;
}

/** The unsaved changes, in the kit's selection bar's words. */
const UNSAVED_WORDS = {
  count: (n: number) => (n === 1 ? "One change not saved" : `${n} changes not saved`),
  cleared: "Changes discarded",
  clear: "Discard",
  label: "Unsaved changes",
};

function levelOptions(lane: Lane): SelectOption[] {
  // WHERE THE LIST COMES FROM is the menu's own heading; the empty level
  // (product access only) carries the vendor's note on what it then sees. A
  // multi-select has no empty row: choosing nothing is that level. A level the
  // connection cannot grant yet is offered disabled, saying why.
  const group = levelsHeading(lane);
  return lane.levels
    .filter((l) => !(lane.multiple && l.id === ""))
    .map((l) => ({
      value: l.id,
      label: l.custom ? `${l.name} (custom)` : l.name,
      ...(l.unavailable
        ? { description: l.unavailable, disabled: true }
        : l.note
          ? { description: l.note }
          : {}),
      group,
    }));
}

/**
 * `memo`, with stable props from its pages: an unchanged re-read of the
 * readings around it draws no row again.
 */
export const AgentsAccess = memo(function AgentsAccess({
  kind,
  name,
  data,
  reload,
  onAnswer,
  held,
  readOnly,
  avatarOf,
  only,
  onReplaceKey,
  stepFor,
}: AgentsAccessProps) {
  const [draft, setDraft] = useState<AccessDraft>({});
  // WHICH ROWS ARE OPEN, by row key: the grid's expansion is controlled.
  const [opened, setOpened] = useState<ReadonlySet<string>>(() => new Set());
  const [review, setReview] = useState(false);
  const [removing, setRemoving] = useState<{ seat: AccessSeat; lane: Lane } | null>(null);
  const [editing, setEditing] = useState<{ seat: AccessSeat; lane: Lane } | null>(null);
  const [scoping, setScoping] = useState<{ seat: AccessSeat; lane: Lane } | null>(null);
  const [rotating, setRotating] = useState<{ seat: AccessSeat; lane: Lane } | null>(null);
  // THE HANDOFF IS HELD AS IT WAS OPENED, so the sheet keeps its robot and
  // role while the answer behind it moves on to In sync.
  const [handing, setHanding] = useState<{
    seat: AccessSeat;
    lane: Lane;
    handoff: Handoff;
  } | null>(null);
  const [said, setSaid] = useState("");
  // A REFUSAL ABOUT ONE SEAT'S CONTROL, drawn beside that control until the
  // seat's draft changes.
  const [refusal, setRefusal] = useState<PlacedRefusal | null>(null);
  const uid = useId();
  const refusalId = (seat: AccessSeat, lane: Lane) => `${uid}-refused-${seat.handle}-${lane.key}`;
  /** The refusal about this seat's lane and one of these controls, or undefined. */
  const refusalAt = (seat: AccessSeat, lane: Lane, ...fields: PlacedRefusal["field"][]) =>
    refusal?.seat === seat.handle && refusal.lane === lane.key && fields.includes(refusal.field)
      ? refusal
      : undefined;
  /** What a control a refusal names carries: the refused mark, and the sentence that says why. */
  const refusedProps = (seat: AccessSeat, lane: Lane, ...fields: PlacedRefusal["field"][]) =>
    refusalAt(seat, lane, ...fields)
      ? { error: true, "aria-describedby": refusalId(seat, lane) }
      : {};
  // WHAT EACH SEAT'S LANE HAS IN FLIGHT, so its own buttons say so and the
  // rest of the table stays usable.
  const [pending, setPending] = useState<Record<string, Pending>>({});
  // INSTALL LINKS THE BROWSER WOULD NOT OPEN in a new tab, drawn as links.
  const [links, setLinks] = useState<Record<string, string>>({});
  const keyOf = (seat: AccessSeat, lane: Lane) => `${seat.handle}:${lane.key}`;
  const begin = (seat: AccessSeat, lane: Lane, what: Pending) =>
    setPending((p) => ({ ...p, [keyOf(seat, lane)]: what }));
  const end = (seat: AccessSeat, lane: Lane) =>
    setPending((p) => {
      const { [keyOf(seat, lane)]: _, ...rest } = p;
      return rest;
    });
  /** Draw a write's answer, or read again where the caller keeps none. */
  const adopt = (answer: AccessAnswer) => (onAnswer ? onAnswer(answer) : reload());

  const lanes = lanesOf(data);
  const seats = only ? data.seats.filter((s) => s.handle === only) : data.seats;
  const scoped = lanes.some((l) => l.scoped);
  // A HELD TAB says why on each control; a read-only one draws no control at all.
  // A HOLD stops everything; a read-only document stops only what writes it.
  const locked = held !== undefined || readOnly !== undefined || data.syncing;

  const desiredOf = (seat: AccessSeat, lane: AccessLaneKey): AccessDesired =>
    draft[seat.handle]?.[lane] ?? seat.lanes[lane]!.desired;

  let changes = 0;
  // A DRAFT THE ENGINE WOULD REFUSE is held at the review, with its reason.
  let refused: string | undefined;
  for (const seat of data.seats)
    for (const lane of lanes) {
      const d = draft[seat.handle]?.[lane.key];
      const saved = seat.lanes[lane.key];
      if (d && saved && !sameDesired(lane, d, saved.desired)) {
        changes++;
        refused ??= refusedLevel(lane, d) ?? refusedScope(lane, d, seat.name) ?? undefined;
      }
    }

  const change = (seat: AccessSeat, lane: Lane, next: Partial<AccessDesired>) => {
    setSaid("");
    setRefusal((r) => (r?.seat === seat.handle && r.lane === lane.key ? null : r));
    setDraft((d) => {
      const merged = { ...desiredOf(seat, lane.key), ...next };
      const lanesDraft = { ...(d[seat.handle] ?? {}) };
      if (sameDesired(lane, merged, seat.lanes[lane.key]!.desired)) delete lanesDraft[lane.key];
      else lanesDraft[lane.key] = merged;
      const out = { ...d, [seat.handle]: lanesDraft };
      if (Object.keys(lanesDraft).length === 0) delete out[seat.handle];
      return out;
    });
  };

  // THE TAB OPENS IN THE CLICK, so a popup blocker lets it through, and goes
  // to the link once the engine has minted it: single-use, so never stored.
  // A BLOCKED TAB is said, and the link drawn where the button was.
  const install = async (seat: AccessSeat, lane: Lane) => {
    const tab = window.open("", "_blank");
    begin(seat, lane, "install");
    try {
      const { url } = await accessApi.install(kind, seat.handle, lane.key);
      if (new URL(url).protocol !== "https:") throw new Error("The install link was not secure.");
      if (tab) {
        tab.opener = null;
        tab.location.href = url;
      } else {
        setLinks((l) => ({ ...l, [keyOf(seat, lane)]: url }));
      }
      setSaid("");
    } catch (err) {
      tab?.close();
      setSaid(refusalWords(err));
    } finally {
      end(seat, lane);
    }
  };

  const act = async (seat: AccessSeat, lane: Lane, action: AccessSeatAction) => {
    begin(seat, lane, action);
    try {
      // KEEP WRITES THE DOCUMENT, so it states the revision it read; a save
      // in between refuses it `revision_advanced`, and the tab reads again.
      const answer = await accessApi.act(
        kind,
        seat.handle,
        lane.key,
        action,
        action === "keep" ? data.revision : undefined,
      );
      setSaid("");
      setLinks((l) => {
        const { [keyOf(seat, lane)]: _, ...rest } = l;
        return rest;
      });
      adopt(answer);
    } catch (err) {
      setSaid(refusalWords(err));
      reload();
    } finally {
      end(seat, lane);
    }
  };

  // ONE ROW PER AGENT AND PRODUCT: Atlassian's agents have two, every other
  // integration's one. The agent is named on its first.
  const rows: AccessRow[] = seats.flatMap((seat) =>
    lanes.flatMap((lane, i) => {
      const saved = seat.lanes[lane.key];
      if (!saved) return [];
      const want = desiredOf(seat, lane.key);
      const drafted = !sameDesired(lane, want, saved.desired);
      return [
        {
          key: `${seat.handle}:${lane.key}`,
          seat,
          lane,
          saved,
          want,
          drafted,
          first: i === 0 || !lanes.slice(0, i).some((l) => seat.lanes[l.key]),
          status: statusOf(lane, saved, drafted),
        },
      ];
    }),
  );

  // ONE ORDER OVER WHAT THE CLIENT HOLDS WHOLE: every agent is in the answer,
  // so Agent and Status sort here. Not across two products (Atlassian), whose
  // rows for one agent must stay together under its name.
  const sortable = lanes.length === 1 && !only;
  const whoWord = only ? "Product" : "Agent";
  const scopeWord = laneWords(lanes.find((l) => l.scoped)?.key ?? "").scope?.noun ?? "Scope";
  const columns: Record<string, DataGridColumn<AccessRow>> = {
    who: {
      key: "who",
      header: whoWord,
      label: whoWord,
      // CAPPED AND WRAPPING, so a long seat name takes two lines rather than
      // pushing the access, scope and status columns past the edge.
      ...(only ? {} : { width: "minmax(10rem, 18rem)", overflow: "wrap" as const }),
      ...(sortable ? { sortValue: ({ seat }: AccessRow) => seat.name.toLowerCase() } : {}),
      cell: ({ seat, lane, first }) =>
        only ? (
          <span className="agent-access-name">{lane.name}</span>
        ) : first ? (
          <span className="agent-access-who">
            <SeatAvatar
              name={seat.name}
              kind="agent"
              avatar={avatarOf(seat.handle)}
              size="sm"
              decorative
            />
            <span className="agent-access-name">
              <span>{seat.name}</span>
              <span className="t-caption mono">{handleLabel(seat.handle)}</span>
            </span>
          </span>
        ) : null,
    },
    product: { key: "product", header: "Product", shrink: true, cell: ({ lane }) => lane.name },
    on: {
      key: "on",
      header: "On",
      shrink: true,
      cell: ({ seat, lane, want }) => (
        <>
          {readOnly ? (
            <span className="agent-access-quiet">{want.enabled ? "On" : "Off"}</span>
          ) : (
            <Switch
              label={<VisuallyHidden>{`${seat.name} on ${lane.name}`}</VisuallyHidden>}
              checked={want.enabled}
              disabled={locked || seat.departed}
              title={held}
              onCheckedChange={(checked) => {
                const only = checked ? onlyScope(lane, want) : undefined;
                change(seat, lane, only ? { enabled: true, scope: only } : { enabled: checked });
              }}
            />
          )}
        </>
      ),
    },
    access: {
      key: "access",
      header: "Access",
      floor: "9rem",
      drop: 2,
      cell: ({ seat, lane, want }) => {
        const off = !want.enabled;
        return (
          <>
            {managedOutside(lane) ? (
              <span className="agent-access-quiet">{MANAGED_OUTSIDE}</span>
            ) : off && !seat.departed && !lane.permissions ? (
              // NOTHING TO CHOOSE WHILE OFF, so no picker: a disabled one with
              // no value read as an empty control waiting for a choice.
              <span className="agent-access-quiet">
                {want.level ? levelName(lane, want.level) : "None while off"}
              </span>
            ) : readOnly ? (
              <span className="agent-access-quiet">
                {lane.permissions
                  ? permissionCount(lane, want.permissions)
                  : (laneWords(lane.key).fixed ?? levelName(lane, want.level))}
              </span>
            ) : lane.multiple && lane.levels.length > 0 ? (
              <Select
                size="sm"
                width="full"
                menuClassName="agent-access-menu"
                multiple
                ariaLabel={`${seat.name}'s ${lane.name} permissions`}
                {...refusedProps(seat, lane, "level")}
                value={levelIds(want.level)}
                options={levelOptions(lane)}
                disabled={off || locked || seat.departed}
                placeholder={levelName(lane, "")}
                onChange={(v) => {
                  const level = joinLevels(lane, (Array.isArray(v) ? v : [v]).map(String));
                  // PRODUCT ACCESS ONLY TAKES NO SCOPE, so choosing it clears one.
                  change(seat, lane, productOnly(lane, level) ? { level, scope: [] } : { level });
                }}
                renderMultiLabel={(sel) =>
                  levelSummary(lane, sel.map((x) => String(x.value)).join(" "))
                }
              />
            ) : lane.levels.length > 0 ? (
              <Select
                size="sm"
                width="full"
                menuClassName="agent-access-menu"
                ariaLabel={`${seat.name}'s ${lane.name} access`}
                {...refusedProps(seat, lane, "level")}
                value={want.level ?? ""}
                options={levelOptions(lane)}
                placeholder="Choose one"
                disabled={off || locked || seat.departed}
                onChange={(v) => {
                  const level = String(v);
                  change(seat, lane, productOnly(lane, level) ? { level, scope: [] } : { level });
                }}
              />
            ) : lane.permissions ? (
              <Inline gap={1}>
                <span title={permissionWords(lane, want.permissions)}>
                  {permissionCount(lane, want.permissions)}
                </span>
                <IconButton
                  size="sm"
                  variant="ghost"
                  icon={<PencilGlyph size="sm" />}
                  label={`Edit ${seat.name}'s ${lane.name} permissions`}
                  title={permissionWords(lane, want.permissions)}
                  disabled={off || locked || seat.departed}
                  aria-describedby={
                    refusalAt(seat, lane, "permissions") ? refusalId(seat, lane) : undefined
                  }
                  onClick={() => setEditing({ seat, lane })}
                />
              </Inline>
            ) : laneWords(lane.key).fixed ? (
              <span className="agent-access-quiet">{laneWords(lane.key).fixed}</span>
            ) : null}
          </>
        );
      },
    },
    scope: {
      key: "scope",
      header: scopeWord,
      label: scopeWord,
      // ROOM FOR THE LONGEST EMPTY WORDS ("All installed repositories") AND
      // THE CONTROL BESIDE THEM, on one line.
      floor: "14rem",
      drop: 1,
      cell: ({ seat, lane, want }) => {
        const words = laneWords(lane.key).scope;
        if (!scopeApplies(lane, want) || !words) return null;
        const label = `${seat.name}'s ${lane.name} ${words.noun.toLowerCase()}`;
        const held = want.scope ?? [];
        // THE CHOICE, READ-ONLY, ON ONE LINE: the first entry as a chip and
        // the rest as a count that names them, or the lane's words for none.
        const names = held.map((id) => scopeLabel(lane, id));
        const rest = names.slice(1);
        const shown =
          names.length === 0 ? (
            <span className="agent-access-quiet">{words.empty}</span>
          ) : (
            <>
              {/* AN INLINE OF ITS OWN, whose minimum width is none where a
                  flex item's is its content, so a long name ends in an
                  ellipsis and the count and the control keep their line. */}
              <Inline>
                <Tag appearance="outline" size="xs" title={names[0]}>
                  {names[0]}
                </Tag>
              </Inline>
              {rest.length > 0 && (
                <Text as="span" variant="caption" tone="secondary" title={rest.join(", ")}>
                  <span aria-hidden="true">+{rest.length}</span>
                  <VisuallyHidden>
                    {`${rest.length.toLocaleString()} more ${rest.length === 1 ? words.singular : words.noun.toLowerCase()}`}
                  </VisuallyHidden>
                </Text>
              )}
            </>
          );
        if (readOnly) return <Inline gap={1}>{shown}</Inline>;
        const unavailable = scopeUnavailable(lane, want.scope);
        if (unavailable)
          return (
            <span className="agent-access-quiet">
              {unavailable.words}
              {unavailable.link && (
                <>
                  {". "}
                  <Link href={unavailable.link.href} external size="inherit">
                    {unavailable.link.label}
                  </Link>
                </>
              )}
            </span>
          );
        // CHANGED IN A DIALOG, as permissions are: a pencil where entries are
        // chosen, a plus where none is yet.
        return (
          <Inline gap={1}>
            {shown}
            <IconButton
              size="sm"
              variant="ghost"
              icon={held.length > 0 ? <PencilGlyph size="sm" /> : <PlusGlyph size="sm" />}
              label={`${held.length > 0 ? "Edit" : "Choose"} ${label}`}
              disabled={!want.enabled || locked || seat.departed}
              aria-describedby={refusalAt(seat, lane, "scope") ? refusalId(seat, lane) : undefined}
              onClick={() => setScoping({ seat, lane })}
            />
          </Inline>
        );
      },
    },
    status: {
      key: "status",
      header: "Status",
      // ITS WHOLE WORDS, as the agents grid of a service keeps its state.
      width: "max-content",
      ...(sortable ? { sortValue: ({ status }: AccessRow) => statusRank(status) } : {}),
      cell: ({ status }) => (
        <Tag
          size="sm"
          variant={status.tone}
          appearance={status.outline ? "outline" : "soft"}
          leadingIcon={status.busy ? <Spinner size="em" /> : undefined}
        >
          {status.label}
        </Tag>
      ),
    },
  };
  const order = [
    "who",
    ...(lanes.length > 1 && !only ? ["product"] : []),
    "on",
    "access",
    ...(scoped ? ["scope"] : []),
    "status",
  ];

  if (lanes.length === 0 || seats.length === 0) {
    return (
      <p className="t-caption">
        {only
          ? `This agent has no access to manage on ${name}.`
          : `No agent to show on ${name} yet.`}
      </p>
    );
  }

  const withoutTool = seats.filter((s) =>
    Object.values(s.lanes).some((l) => l.tool === "no_server"),
  );
  // WHAT TURNING AN OFF AGENT ON DOES is the lane's, the same for every off
  // agent, so it is said once here rather than in each row's details.
  const offNotes = only ? [] : offNote(rows);

  return (
    <div className="agent-access">
      {readOnly && <p className="t-caption">{readOnly}</p>}
      {data.notice && !only && (
        <Callout
          variant={data.notice.tone}
          title={data.notice.title}
          action={
            data.notice.replace_key && onReplaceKey ? (
              <Button
                size="small"
                variant="primary"
                leadingIcon={<KeyGlyph size="sm" />}
                onClick={onReplaceKey}
              >
                Replace key
              </Button>
            ) : undefined
          }
        >
          {data.notice.body}
        </Callout>
      )}
      {withoutTool.length > 0 && (
        <Callout
          variant="warning"
          title={`No ${name} tool for ${only ? "this agent" : "its agents"}`}
        >
          {`${withoutTool.map((s) => s.name).join(", ")} ${withoutTool.length === 1 ? "has" : "have"} an account at ${name} and nothing to use it with: ${noToolRemedy(lanes[0]!)}`}
        </Callout>
      )}

      {offNotes.map((note) => (
        <p key={note} className="t-caption agent-access-quiet">
          {note}
        </p>
      ))}

      <DataGrid<AccessRow>
        name="access"
        rows={rows}
        rowKey={(row) => row.key}
        aria-label={only ? `Access on ${name}` : `Agents on ${name}`}
        columns={order.map((key) => columns[key]!)}
        rowTone={(row) => toneOf(row.status)}
        expansion={{
          isExpanded: (row) => opened.has(row.key),
          onToggle: (row) =>
            setOpened((was) => {
              const next = new Set(was);
              if (!next.delete(row.key)) next.add(row.key);
              return next;
            }),
          label: (row, isOpen) =>
            `${isOpen ? "Hide" : "Show"} ${row.seat.name}'s ${row.lane.name} details`,
          // AN OFF ROW WITH NO ACTION HAS NOTHING TO OPEN: its one sentence is
          // said above the grid.
          canExpand: (row) =>
            only !== undefined ||
            row.saved.desired.enabled ||
            row.seat.departed === true ||
            availableActions(row.lane, row.saved, held !== undefined, data.syncing).length > 0,
          render: (row) => (
            <Details
              lane={row.lane}
              seat={row.seat}
              saved={row.saved}
              said={!only}
              actions={availableActions(row.lane, row.saved, held !== undefined, data.syncing)}
              pending={pending[row.key]}
              onAction={(a) => {
                // A ROTATE THAT A PERSON MAKES AT THE VENDOR opens the sheet at
                // the paste; every other lane rotates by itself.
                const handoff = a === "rotate" ? rotateHandoffOf(row.lane, row.saved) : undefined;
                if (handoff) setHanding({ seat: row.seat, lane: row.lane, handoff });
                else if (a === "rotate") setRotating({ seat: row.seat, lane: row.lane });
                else setRemoving({ seat: row.seat, lane: row.lane });
              }}
            />
          ),
        }}
        rowAccessory={(row) => {
          // NOTHING TO SAY IS NO ROW: the message is read here, not handed to
          // a component that might draw nothing, because an element is never
          // null and every row grew an empty accessory row under it.
          const message = row.drafted
            ? null
            : laneMessage({
                lane: row.lane,
                seat: row.seat,
                saved: row.saved,
                syncing: data.syncing,
                held,
                readOnly: readOnly !== undefined,
                step: stepFor?.(row.seat.handle),
                pending: pending[row.key],
                link: links[row.key],
                onAct: (a) => void act(row.seat, row.lane, a),
                onInstall: () => void install(row.seat, row.lane),
                onHandoff: (handoff) => setHanding({ seat: row.seat, lane: row.lane, handoff }),
                onRemove: () => setRemoving({ seat: row.seat, lane: row.lane }),
              });
          const hint = emptyScopeHint(row.lane, row.want) ?? productOnlyNote(row.lane, row.want);
          const placed = refusalAt(row.seat, row.lane, "level", "permissions", "scope");
          const ceiling = row.seat.departed
            ? null
            : ceilingNote(row.lane, row.want, row.saved.observed);
          return message || hint || placed || ceiling ? (
            <div className="agent-access-under">
              {message}
              {placed && (
                <p
                  id={refusalId(row.seat, row.lane)}
                  className="t-caption agent-access-refused"
                  role="alert"
                >
                  {placed.words}
                </p>
              )}
              {hint && <p className="t-caption">{hint}</p>}
              {ceiling && <p className="t-caption">{ceiling}</p>}
            </div>
          ) : null;
        }}
      />

      {said && (
        <Callout variant="danger" live="polite">
          {said}
        </Callout>
      )}

      {/* THE KIT'S STICKY BAR, which docks to the foot on a phone and says the
          count as it changes: what is drafted, Discard, and the review. */}
      {!readOnly && (
        <SelectionBar
          count={changes}
          onClear={() => {
            setDraft({});
            setRefusal(null);
          }}
          labels={UNSAVED_WORDS}
        >
          <Button
            size="small"
            variant="primary"
            disabledReason={refused}
            onClick={() => {
              if (!refused) setReview(true);
            }}
          >
            Review changes
          </Button>
        </SelectionBar>
      )}

      {review && (
        <ReviewDialog
          kind={kind}
          name={name}
          data={data}
          draft={draft}
          avatarOf={avatarOf}
          onClose={() => setReview(false)}
          onSaved={(answer) => {
            setDraft({});
            setReview(false);
            adopt(answer);
          }}
          // A DOCUMENT THAT MOVED (a save refused as stale, or a plan drawn
          // from a newer revision) keeps the draft and reads again; the review
          // plans it again against the revision that came back.
          onStale={reload}
          // A REFUSAL ABOUT A SEAT ON THIS TABLE is drawn beside its control;
          // one about a seat the table does not show is said in the banner.
          onRefused={(r) => {
            setReview(false);
            if (rows.some((row) => row.seat.handle === r.seat && row.lane.key === r.lane))
              setRefusal(r);
            else setSaid(r.words);
          }}
        />
      )}
      {editing && (
        <PermissionsDialog
          seat={editing.seat}
          lane={editing.lane}
          want={desiredOf(editing.seat, editing.lane.key)}
          onChange={(perm, value) => {
            const next = { ...(desiredOf(editing.seat, editing.lane.key).permissions ?? {}) };
            if (value === "none") delete next[perm];
            else next[perm] = value;
            change(editing.seat, editing.lane, { permissions: next });
          }}
          onClose={() => setEditing(null)}
        />
      )}
      {scoping && (
        <ScopeDialog
          seat={scoping.seat}
          lane={scoping.lane}
          want={desiredOf(scoping.seat, scoping.lane.key)}
          refused={refusalAt(scoping.seat, scoping.lane, "scope")?.words}
          onChange={(scope) => change(scoping.seat, scoping.lane, { scope })}
          onClose={() => setScoping(null)}
        />
      )}
      {removing && (
        <RemoveDialog
          name={name}
          seat={removing.seat}
          lane={removing.lane}
          onClose={() => setRemoving(null)}
          onRemove={async () => {
            await act(removing.seat, removing.lane, "remove");
            setRemoving(null);
          }}
        />
      )}
      {handing && (
        <HandoffSheet
          seat={handing.seat}
          lane={handing.lane}
          handoff={handing.handoff}
          onClose={() => setHanding(null)}
          onPaste={async (token) => {
            const { verdict, access } = await accessApi.credential(
              kind,
              handing.seat.handle,
              handing.lane.key,
              token,
            );
            adopt(access);
            return verdict;
          }}
          onCheck={() => void act(handing.seat, handing.lane, "done")}
          refusal={refusalParts}
        />
      )}
      {rotating && (
        <RotateDialog
          seat={rotating.seat}
          lane={rotating.lane}
          onClose={() => setRotating(null)}
          onRotate={async () => {
            await act(rotating.seat, rotating.lane, "rotate");
            setRotating(null);
          }}
        />
      )}
    </div>
  );
});

/** The action a seat's lane has in flight, if any, for its buttons' pending state. */
type Pending = AccessSeatAction | "install" | undefined;

/**
 * What a lane is waiting on, under its row: a seat that left, a refusal, an
 * account turned off at the vendor by hand, a change made there, or a step
 * only a person can take. Null where it waits on nothing, so the row under it
 * is not drawn at all. A plain function of the lane, holding no state.
 */
function laneMessage({
  lane,
  seat,
  saved,
  syncing,
  held,
  readOnly,
  step,
  pending,
  link,
  onAct,
  onInstall,
  onRemove,
  onHandoff,
}: {
  lane: Lane;
  seat: AccessSeat;
  saved: AccessSeatLane;
  syncing: boolean;
  held: string | undefined;
  /** The operator writes the document: Keep, which would write it, is not offered. */
  readOnly: boolean;
  step: ReactNode;
  pending: Pending;
  /** An install link the browser would not open in a new tab, drawn as a link instead. */
  link: string | undefined;
  onAct: (action: "retry" | "keep" | "restore" | "done" | "release") => void;
  onInstall: () => void;
  onRemove: () => void;
  /** Opens the Set up sheet, where only a person can make the seat's account at the vendor. */
  onHandoff: (handoff: Handoff) => void;
}): ReactNode {
  if (syncing && saved.plan.length > 0) return null;
  const busy = held !== undefined;
  const keep = !busy && !readOnly;
  const o = saved.observed;
  const press = (
    action: "retry" | "keep" | "restore" | "done" | "release",
    label: ReactNode,
    quiet = false,
  ) => (
    <Button
      size="small"
      variant={quiet ? "ghost" : "secondary"}
      loading={pending === action}
      disabled={pending !== undefined && pending !== action}
      onClick={() => onAct(action)}
    >
      {label}
    </Button>
  );
  // WHERE A PERSON ACTS AT THE VENDOR is the engine's to say, step by step.
  const at = (url: string | undefined) =>
    url ? (
      <ButtonLink size="small" variant="ghost" href={url} external>
        Open {lane.name}
      </ButtonLink>
    ) : null;
  let body: ReactNode = null;
  if (seat.departed) {
    body = (
      <Callout
        variant="warning"
        title={`${seat.name} left the company`}
        action={
          busy || !o.exists ? undefined : (
            <Button
              size="small"
              variant="secondary"
              loading={pending === "remove"}
              disabled={pending !== undefined && pending !== "remove"}
              onClick={onRemove}
            >
              Remove account
            </Button>
          )
        }
      >
        {o.exists
          ? `Its account at ${lane.name} is kept until you remove it.`
          : `It has no account at ${lane.name}.`}
      </Callout>
    );
  } else if (saved.sync?.outcome === "failed" && saved.sync.error) {
    body = (
      <Callout
        variant="danger"
        title={`${lane.name} refused the last sync`}
        action={
          <span className="agent-access-actions">
            {!busy && press("retry", "Retry")}
            {at(saved.sync.error.action_url)}
            {keep &&
              saved.desired.level !== o.level &&
              o.level &&
              press("keep", `Keep ${levelName(lane, o.level)}`, true)}
          </span>
        }
      >
        <p>{saved.sync.error.said}</p>
        <p>{saved.sync.error.remedy}</p>
      </Callout>
    );
  } else if (
    // TURNED OFF AT THE VENDOR BY HAND (D3): the engine plans nothing for it,
    // and leaves the seat off until somebody decides, so this is read off the
    // account alone.
    saved.desired.enabled &&
    o.exists &&
    !o.enabled &&
    o.disabled_by === "hand"
  ) {
    const at = o.changed_by_hand_at;
    body = (
      <Callout
        variant="warning"
        title={
          <>
            Turned off at {lane.name}
            {at && (
              <>
                {" "}
                <Clocked text={(now) => relTime(at, now)} />
              </>
            )}{" "}
            by somebody else
          </>
        }
        action={
          busy ? undefined : (
            <span className="agent-access-actions">
              {press("restore", "Turn back on")}
              {keep && press("keep", "Keep off", true)}
            </span>
          )
        }
      >
        Crewlet leaves {seat.name} off until you decide, in case that was deliberate.
        {keep && " Keep off turns it off here too."}
      </Callout>
    );
  } else if (o.changed_by_hand_at && saved.plan.length > 0) {
    const at = o.changed_by_hand_at;
    const when = <Clocked text={(now) => relTime(at, now)} />;
    const levelMoved = saved.desired.level !== o.level;
    body = (
      <Callout
        variant="warning"
        title={
          <>
            Changed at {lane.name} {when}
          </>
        }
        action={
          busy ? undefined : (
            <span className="agent-access-actions">
              {press("restore", "Restore now")}
              {keep && press("keep", `Keep ${lane.name}'s setting`, true)}
            </span>
          )
        }
      >
        {lane.name} shows {levelMoved ? accessWords(lane, o) : scopeWords(lane, o.scope)}, and
        Crewlet&apos;s setting is{" "}
        {levelMoved ? levelName(lane, saved.desired.level) : scopeWords(lane, saved.desired.scope)}.
        {keep
          ? ` The next sync restores Crewlet's setting unless you keep ${lane.name}'s.`
          : " The next sync restores Crewlet's setting."}
      </Callout>
    );
  } else if (saved.desired.enabled && (o.retired?.length ?? 0) > 0) {
    // AN OLD CREDENTIAL STILL WORKS AT THE VENDOR, which only a person can
    // delete there: each by the label the vendor shows and when it was made,
    // in place of the wait that repeats it every pass, and cleared on its own
    // once the vendor shows it gone.
    const retired = o.retired!;
    const words = laneWords(lane.key).retired ?? RETIRED_GENERIC;
    const one = retired.length === 1;
    body = (
      <Callout
        variant="warning"
        title={
          one
            ? `${words.one} still works at ${lane.name}`
            : `${retired.length} ${words.many} still work at ${lane.name}`
        }
        action={
          <span className="agent-access-actions">
            {at(saved.sync?.error?.action_url ?? lane.action_url)}
            {!busy && press("done", "Done, check again", true)}
          </span>
        }
      >
        {retired.map((r) => (
          <p key={r.label}>
            <InlineCode>{r.label}</InlineCode>
            {r.since && `, created ${fmtDate(r.since)}`}
          </p>
        ))}
        <p>{`Delete ${one ? "it" : "them"} at ${lane.name}. Crewlet notices once ${one ? "it is" : "they are"} gone.`}</p>
      </Callout>
    );
  } else if (
    saved.sync?.outcome === "waiting" ||
    (!saved.sync && saved.plan.some((s) => s.by === "you"))
  ) {
    // A WAIT IS ONE SENTENCE OF WHAT TO DO, in the sync's own words where it
    // recorded them, else the step it stopped at; the buttons beside it are
    // where and how to say it is done. The plan is read only before any sync
    // ran.
    const error = saved.sync?.error;
    const waiting = saved.sync
      ? saved.sync.steps.find((s) => s.by === "you" && !s.done)
      : saved.plan.find((s) => s.by === "you");
    const todo = error?.said || waiting?.text;
    if (!todo) return null;
    const install = laneWords(lane.key).install;
    // KEEPING THE ACCOUNT AT THE VENDOR IS THE PERSON'S CHOICE on the wait to
    // delete it there, where the lane allows it; Crewlet records it rather
    // than waits on.
    const release =
      error?.step === "remove" && lane.releasable
        ? (laneWords(lane.key).release ?? `Keep the account at ${lane.name}`)
        : undefined;
    // A HANDOFF IS ONE BUTTON THAT OPENS ITS SHEET, which holds the vendor's
    // page, the account to make and the paste; nothing to check again here.
    const handoff = busy ? undefined : handoffOf(lane, saved);
    const control = handoff ? (
      <HandoffButton handoff={handoff} onOpen={() => onHandoff(handoff)} />
    ) : (
      step
    );
    const where = at(error?.action_url ?? waiting?.action_url);
    body = (
      <Callout
        variant="warning"
        title="Waiting on a person"
        action={
          <span className="agent-access-actions">
            {control ??
              where ??
              (link ? (
                <ButtonLink size="small" variant="secondary" href={link} external>
                  Open the install page
                </ButtonLink>
              ) : !busy && install ? (
                <Button
                  size="small"
                  variant="secondary"
                  loading={pending === "install"}
                  disabled={pending !== undefined && pending !== "install"}
                  onClick={onInstall}
                >
                  {install}
                </Button>
              ) : null)}
            {!busy && !control && press("done", "Done, check again", true)}
            {!busy && release && press("release", release, true)}
          </span>
        }
      >
        <p>{asSentence(todo)}</p>
        {link && <p>Your browser kept the install page from opening in a new tab.</p>}
      </Callout>
    );
  }
  return body;
}

/** What the Set up sheet opens on, as the button that opens it. */
function handoffLabel(handoff: Handoff): string {
  if (handoff.step === "create") return "Set up";
  if (handoff.step === "rerole") return "Fix the role";
  return handoff.renew ? "Paste a new token" : "Paste its token";
}

function HandoffButton({ handoff, onOpen }: { handoff: Handoff; onOpen: () => void }) {
  return (
    <Button size="small" variant="primary" onClick={onOpen}>
      {handoffLabel(handoff)}
    </Button>
  );
}

const ACTION_WORDS: Record<SeatAction, string> = {
  rotate: "Rotate credential",
  remove: "Remove account",
};

/**
 * A seat's details, as a full-width row of the table: five pairs at one size,
 * and only the actions it can take now; a seat that is off reads as one
 * sentence.
 */
function Details({
  lane,
  seat,
  saved,
  said,
  actions,
  pending,
  onAction,
}: {
  lane: Lane;
  seat: AccessSeat;
  saved: AccessSeatLane;
  /** The off sentence is already said above the grid, so an off row draws only its actions. */
  said: boolean;
  actions: SeatAction[];
  pending: Pending;
  onAction: (action: SeatAction) => void;
}) {
  const o = saved.observed;
  const outside = managedOutside(lane);
  // WHAT ELSE CREWLET PUTS IN THE SEAT'S BOX, beside the credentials its tool reads.
  const settings = boxSettingsFact(lane, saved);
  const buttons =
    actions.length > 0 ? (
      <span className="agent-access-detail-actions">
        {actions.map((a) => (
          <Button
            key={a}
            size="small"
            variant="ghost"
            loading={pending === a}
            disabled={pending !== undefined && pending !== a}
            onClick={() => onAction(a)}
          >
            {ACTION_WORDS[a]}
          </Button>
        ))}
      </span>
    ) : null;
  return (
    <div className="agent-access-details">
      {seat.departed || (!saved.desired.enabled && !said) ? (
        <p className="agent-access-off">{offSentence(lane, saved, seat.departed)}</p>
      ) : !saved.desired.enabled ? null : (
        <DescriptionList
          uniform
          dense
          fit
          className="agent-access-facts"
          aria-label={`${seat.name} on ${lane.name}`}
          items={[
            [
              "Account",
              o.exists
                ? (o.account ?? "Created")
                : outside
                  ? (o.account ?? MANAGED_OUTSIDE)
                  : "Not created yet",
            ],
            [
              laneWords(lane.key).access,
              o.exists || outside ? accessWords(lane, o) : "Not granted yet",
            ],
            [
              "Credential",
              o.credential ? (
                <span key="credential">
                  <span className="mono">{o.credential.variable}</span>
                  <Clocked text={(now) => credentialNote(lane, o, now)} />
                </span>
              ) : providedExternally(lane, o) ? (
                "Provided externally"
              ) : (
                "Not minted yet"
              ),
            ],
            ["Tool", toolWords(lane, saved)],
            ...(settings
              ? [
                  [
                    "Set by Crewlet",
                    <span key="box">
                      <span className="mono">{settings.settings}</span>
                      {settings.note && <>. {settings.note}</>}
                    </span>,
                  ] as const,
                ]
              : []),
            ["Sync", <Clocked key="sync" text={(now) => clockValue(saved, now)} />],
          ]}
        />
      )}
      {buttons}
    </div>
  );
}
