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

import { useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  DataTable,
  DescriptionList,
  Select,
  SelectionBar,
  Spinner,
  Switch,
  Tag,
  VisuallyHidden,
  type DataTableColumn,
  type DataTableRowTone,
  type SelectOption,
} from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import type {
  AccessAnswer,
  AccessDesired,
  AccessLaneKey,
  AccessSeat,
  AccessSeatAction,
  AccessSeatLane,
} from "~/contract/access.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { useNow } from "~/lib/clock.ts";
import { plural, relTime } from "~/lib/format.ts";
import { handleLabel } from "~/lib/seats.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import "~/styles/access.css";
import { accessApi, refusalWords, type AccessDraft } from "./api.ts";
import { PermissionsDialog, RemoveDialog, ReviewDialog, RotateDialog } from "./Dialogs.tsx";
import {
  accessWords,
  availableActions,
  emptyScopeHint,
  joinLevels,
  lanesOf,
  levelIds,
  levelName,
  managedOutside,
  MANAGED_OUTSIDE,
  levelsHeading,
  levelSummary,
  offSentence,
  permissionCount,
  permissionWords,
  productOnly,
  productOnlyNote,
  providedExternally,
  refusedLevel,
  sameDesired,
  scopeApplies,
  scopeWords,
  statusOf,
  syncValue,
  toolWords,
  type Lane,
  type SeatAction,
  type Status,
} from "./logic.ts";
import { laneWords } from "./words.ts";

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
function toneOf(status: Status): DataTableRowTone | null {
  if (status.busy) return "info";
  return status.tone === "danger" || status.tone === "warning" ? status.tone : null;
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
  // multi-select has no empty row: choosing nothing is that level.
  const group = levelsHeading(lane);
  return lane.levels
    .filter((l) => !(lane.multiple && l.id === ""))
    .map((l) => ({
      value: l.id,
      label: l.custom ? `${l.name} (custom)` : l.name,
      ...(l.note ? { description: l.note } : {}),
      group,
    }));
}

export function AgentsAccess({
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
  const now = useNow();
  const [draft, setDraft] = useState<AccessDraft>({});
  const [review, setReview] = useState(false);
  const [removing, setRemoving] = useState<{ seat: AccessSeat; lane: Lane } | null>(null);
  const [editing, setEditing] = useState<{ seat: AccessSeat; lane: Lane } | null>(null);
  const [rotating, setRotating] = useState<{ seat: AccessSeat; lane: Lane } | null>(null);
  const [said, setSaid] = useState("");
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
  const scoped = lanes.some((l) => l.scope !== undefined);
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
        refused ??= refusedLevel(lane, d) ?? undefined;
      }
    }

  const change = (seat: AccessSeat, lane: Lane, next: Partial<AccessDesired>) => {
    setSaid("");
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
          status: statusOf(lane, saved, drafted, data.syncing, seat.departed),
        },
      ];
    }),
  );

  const columns: Record<string, DataTableColumn<AccessRow>> = {
    who: {
      label: only ? "Product" : "Agent",
      sortable: false,
      hideable: false,
      render: ({ seat, lane, first }) =>
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
    product: { label: "Product", sortable: false, render: ({ lane }) => lane.name },
    on: {
      label: "On",
      sortable: false,
      shrink: true,
      render: ({ seat, lane, want }) => (
        <>
          {readOnly ? (
            <span className="agent-access-quiet">{want.enabled ? "On" : "Off"}</span>
          ) : (
            <Switch
              label={<VisuallyHidden>{`${seat.name} on ${lane.name}`}</VisuallyHidden>}
              checked={want.enabled}
              disabled={locked || seat.departed}
              title={held}
              onCheckedChange={(checked) => change(seat, lane, { enabled: checked })}
            />
          )}
        </>
      ),
    },
    access: {
      label: "Access",
      sortable: false,
      render: ({ seat, lane, want }) => {
        const off = !want.enabled;
        return (
          <>
            {managedOutside(lane) ? (
              <span className="agent-access-quiet">{MANAGED_OUTSIDE}</span>
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
                value={levelIds(want.level)}
                options={levelOptions(lane)}
                disabled={off || locked || seat.departed}
                placeholder={levelName(lane, "")}
                onChange={(v) => {
                  const level = joinLevels((Array.isArray(v) ? v : [v]).map(String));
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
                value={want.level ?? ""}
                options={levelOptions(lane)}
                disabled={off || locked || seat.departed}
                onChange={(v) => {
                  const level = String(v);
                  change(seat, lane, productOnly(lane, level) ? { level, scope: [] } : { level });
                }}
              />
            ) : lane.permissions ? (
              <Button
                size="small"
                variant="ghost"
                disabled={off || locked || seat.departed}
                title={permissionWords(lane, want.permissions)}
                onClick={() => setEditing({ seat, lane })}
              >
                {permissionCount(lane, want.permissions)}
              </Button>
            ) : laneWords(lane.key).fixed ? (
              <span className="agent-access-quiet">{laneWords(lane.key).fixed}</span>
            ) : null}
          </>
        );
      },
    },
    scope: {
      label: laneWords(lanes.find((l) => l.scope !== undefined)?.key ?? "").scope?.noun ?? "Scope",
      sortable: false,
      render: ({ seat, lane, want }) => {
        const off = !want.enabled;
        const words = laneWords(lane.key);
        return (
          <>
            {!scopeApplies(lane, want) || !words.scope ? null : readOnly ? (
              <span className="agent-access-quiet">{scopeWords(lane, want.scope)}</span>
            ) : lane.scope ? (
              <Select
                size="sm"
                width="full"
                menuClassName="agent-access-menu"
                multiple
                searchable
                ariaLabel={`${seat.name}'s ${lane.name} ${words.scope.noun.toLowerCase()}`}
                value={want.scope ?? []}
                options={lane.scope.map((o) => ({ value: o, label: o }))}
                disabled={off || locked || seat.departed}
                placeholder={words.scope.empty}
                onChange={(v) =>
                  change(seat, lane, {
                    scope: (Array.isArray(v) ? v : [v]).map(String),
                  })
                }
                renderMultiLabel={(sel) =>
                  sel.length === 0
                    ? words.scope!.empty
                    : sel.length <= 2
                      ? sel.map((s) => s.label).join(", ")
                      : plural(sel.length, words.scope!.singular, words.scope!.noun.toLowerCase())
                }
              />
            ) : null}
          </>
        );
      },
    },
    status: {
      label: "Status",
      sortable: false,
      shrink: true,
      render: ({ status }) => (
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
          {`${withoutTool.map((s) => s.name).join(", ")} ${withoutTool.length === 1 ? "has" : "have"} an account at ${name} and nothing to use it with: the company declares no ${laneWords(lanes[0]!.key).server} tool server. Add it under Settings › MCP Tools as one instance per agent, and each agent's own credential is sent with it.`}
        </Callout>
      )}

      <DataTable<AccessRow>
        variant="compact"
        data={rows}
        getRowKey={(row) => row.key}
        ariaLabel={only ? `Access on ${name}` : `Agents on ${name}`}
        paginated={false}
        showSettings={false}
        resizable={false}
        defaultColumnOrder={order}
        columns={columns}
        rowTone={(row) => toneOf(row.status)}
        expandLabel={(row, isOpen) =>
          `${isOpen ? "Hide" : "Show"} ${row.seat.name}'s ${row.lane.name} details`
        }
        renderExpandedRow={(row) => (
          <Details
            lane={row.lane}
            seat={row.seat}
            saved={row.saved}
            now={now}
            actions={availableActions(row.lane, row.saved, held !== undefined, data.syncing)}
            pending={pending[row.key]}
            onAction={(a) =>
              a === "rotate"
                ? setRotating({ seat: row.seat, lane: row.lane })
                : setRemoving({ seat: row.seat, lane: row.lane })
            }
          />
        )}
        renderAccessoryRow={(row) => {
          const message = row.drafted ? null : (
            <LaneMessage
              lane={row.lane}
              seat={row.seat}
              saved={row.saved}
              syncing={data.syncing}
              now={now}
              held={held}
              readOnly={readOnly !== undefined}
              step={stepFor?.(row.seat.handle)}
              pending={pending[row.key]}
              link={links[row.key]}
              onAct={(a) => void act(row.seat, row.lane, a)}
              onInstall={() => void install(row.seat, row.lane)}
              onRemove={() => setRemoving({ seat: row.seat, lane: row.lane })}
            />
          );
          const hint = emptyScopeHint(row.lane, row.want) ?? productOnlyNote(row.lane, row.want);
          return message || hint ? (
            <div className="agent-access-under">
              {message}
              {hint && <p className="t-caption">{hint}</p>}
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
        <SelectionBar count={changes} onClear={() => setDraft({})} labels={UNSAVED_WORDS}>
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
          onClose={() => setReview(false)}
          onSaved={(answer) => {
            setDraft({});
            setReview(false);
            adopt(answer);
          }}
          // A SAVE REFUSED AS STALE keeps the draft and reads again; the
          // review plans it again against the revision that came back.
          onStale={reload}
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
}

/** The action a seat's lane has in flight, if any, for its buttons' pending state. */
type Pending = AccessSeatAction | "install" | undefined;

/**
 * What a lane is waiting on, under its row: a seat that left, a refusal, an
 * account turned off at the vendor by hand, a change made there, or a step
 * only a person can take.
 */
function LaneMessage({
  lane,
  seat,
  saved,
  syncing,
  now,
  held,
  readOnly,
  step,
  pending,
  link,
  onAct,
  onInstall,
  onRemove,
}: {
  lane: Lane;
  seat: AccessSeat;
  saved: AccessSeatLane;
  syncing: boolean;
  now: number;
  held: string | undefined;
  /** The operator writes the document: Keep, which would write it, is not offered. */
  readOnly: boolean;
  step: ReactNode;
  pending: Pending;
  /** An install link the browser would not open in a new tab, drawn as a link instead. */
  link: string | undefined;
  onAct: (action: "retry" | "keep" | "restore" | "done") => void;
  onInstall: () => void;
  onRemove: () => void;
}) {
  if (syncing && saved.plan.length > 0) return null;
  const busy = held !== undefined;
  const keep = !busy && !readOnly;
  const o = saved.observed;
  const press = (
    action: "retry" | "keep" | "restore" | "done",
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
    const when = o.changed_by_hand_at ? ` ${relTime(o.changed_by_hand_at, now)}` : "";
    body = (
      <Callout
        variant="warning"
        title={`Turned off at ${lane.name}${when} by somebody else`}
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
    const when = relTime(o.changed_by_hand_at, now);
    const levelMoved = saved.desired.level !== o.level;
    body = (
      <Callout
        variant="warning"
        title={`Changed at ${lane.name} ${when}`}
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
  } else if (
    saved.sync?.outcome === "waiting" ||
    (!saved.sync && saved.plan.some((s) => s.by === "you"))
  ) {
    // A WAIT IS SAID IN THE SYNC'S OWN WORDS: its reason and remedy where the
    // sync recorded them, else the step it stopped at. The plan is read only
    // before any sync ran.
    const error = saved.sync?.error;
    const waiting = saved.sync
      ? saved.sync.steps.find((s) => s.by === "you" && !s.done)
      : saved.plan.find((s) => s.by === "you");
    if (!error && !waiting) return null;
    const install = laneWords(lane.key).install;
    const where = at(error?.action_url ?? waiting?.action_url);
    body = (
      <Callout
        variant="warning"
        title="Waiting on a person"
        action={
          <span className="agent-access-actions">
            {step ??
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
            {!busy && !step && press("done", "Done, check again", true)}
          </span>
        }
      >
        {error ? (
          <>
            <p>{error.said}</p>
            <p>{error.remedy}</p>
          </>
        ) : (
          <>
            <p>{waiting!.text}.</p>
            {waiting!.note && <p>{waiting!.note}</p>}
          </>
        )}
        {link && <p>Your browser kept the install page from opening in a new tab.</p>}
      </Callout>
    );
  }
  return body;
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
  now,
  actions,
  pending,
  onAction,
}: {
  lane: Lane;
  seat: AccessSeat;
  saved: AccessSeatLane;
  now: number;
  actions: SeatAction[];
  pending: Pending;
  onAction: (action: SeatAction) => void;
}) {
  const o = saved.observed;
  const outside = managedOutside(lane);
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
      {!saved.desired.enabled || seat.departed ? (
        <p className="agent-access-off">{offSentence(lane, saved, seat.departed)}</p>
      ) : (
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
                  {providedExternally(lane, o)
                    ? ", provided externally"
                    : `, minted ${relTime(o.credential.minted_at, now)}`}
                </span>
              ) : providedExternally(lane, o) ? (
                "Provided externally"
              ) : (
                "Not minted yet"
              ),
            ],
            ["Tool", toolWords(lane, saved)],
            ["Last sync", syncValue(saved, now)],
          ]}
        />
      )}
      {buttons}
    </div>
  );
}
