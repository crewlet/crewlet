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
  DescriptionList,
  IconButton,
  Select,
  Spinner,
  Switch,
  Tag,
  VisuallyHidden,
  type SelectOption,
} from "@crewlethq/ui";
import { ChevronDownGlyph, ChevronRightGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import type {
  AccessAnswer,
  AccessDesired,
  AccessDraft,
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
import { accessApi, refusalWords } from "./api.ts";
import { PermissionsDialog, RemoveDialog, ReviewDialog } from "./Dialogs.tsx";
import {
  accessWords,
  availableActions,
  emptyScopeHint,
  lanesOf,
  levelName,
  managedOutside,
  MANAGED_OUTSIDE,
  levelsHeading,
  offSentence,
  permissionCount,
  permissionWords,
  providedExternally,
  sameDesired,
  scopeWords,
  statusOf,
  syncValue,
  toolWords,
  type Lane,
  type SeatAction,
} from "./logic.ts";
import { laneWords } from "./words.ts";

export interface AgentsAccessProps {
  /** The integration's key, which the access routes are keyed on. */
  kind: string;
  /** The integration in the reader's own words. */
  name: string;
  data: AccessAnswer;
  /** Read again after a write. */
  reload: () => void;
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

function levelOptions(lane: Lane): SelectOption[] {
  // WHERE THE LIST COMES FROM is the menu's own heading.
  const group = levelsHeading(lane);
  return lane.levels.map((l) => ({
    value: l.id,
    label: l.custom ? `${l.name} (custom)` : l.name,
    group,
  }));
}

export function AgentsAccess({
  kind,
  name,
  data,
  reload,
  held,
  readOnly,
  avatarOf,
  only,
  onReplaceKey,
  stepFor,
}: AgentsAccessProps) {
  const now = useNow();
  const [draft, setDraft] = useState<AccessDraft>({});
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [review, setReview] = useState(false);
  const [removing, setRemoving] = useState<{ seat: AccessSeat; lane: Lane } | null>(null);
  const [editing, setEditing] = useState<{ seat: AccessSeat; lane: Lane } | null>(null);
  const [said, setSaid] = useState("");

  const lanes = lanesOf(data);
  const seats = only ? data.seats.filter((s) => s.handle === only) : data.seats;
  const scoped = lanes.some((l) => l.scope !== undefined);
  // A HELD TAB says why on each control; a read-only one draws no control at all.
  // A HOLD stops everything; a read-only document stops only what writes it.
  const locked = held !== undefined || readOnly !== undefined || data.syncing;

  const desiredOf = (seat: AccessSeat, lane: AccessLaneKey): AccessDesired =>
    draft[seat.handle]?.[lane] ?? seat.lanes[lane]!.desired;

  let changes = 0;
  for (const seat of data.seats)
    for (const lane of lanes) {
      const d = draft[seat.handle]?.[lane.key];
      const saved = seat.lanes[lane.key];
      if (d && saved && !sameDesired(lane, d, saved.desired)) changes++;
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
  const install = async (seat: AccessSeat, lane: Lane) => {
    const tab = window.open("", "_blank");
    try {
      const { url } = await accessApi.install(kind, seat.handle, lane.key);
      if (new URL(url).protocol !== "https:") throw new Error("The install link was not secure.");
      if (tab) {
        tab.opener = null;
        tab.location.href = url;
      } else {
        window.open(url, "_blank", "noopener,noreferrer");
      }
      setSaid("");
    } catch (err) {
      tab?.close();
      setSaid(refusalWords(err));
    }
  };

  const act = async (seat: AccessSeat, lane: Lane, action: AccessSeatAction) => {
    try {
      await accessApi.act(kind, seat.handle, lane.key, action);
      setSaid("");
    } catch (err) {
      setSaid(refusalWords(err));
    } finally {
      reload();
    }
  };

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

      <div
        className={
          scoped ? "agent-access-table" : "agent-access-table agent-access-table--unscoped"
        }
        role="table"
        aria-label={only ? `Access on ${name}` : `Agents on ${name}`}
      >
        <div className="agent-access-row agent-access-row--head" role="row">
          <span role="columnheader">{only ? "Product" : "Agent"}</span>
          {lanes.length > 1 && !only && <span role="columnheader">Product</span>}
          <span role="columnheader">On</span>
          <span role="columnheader">Access</span>
          {scoped && (
            <span role="columnheader">
              {laneWords(lanes.find((l) => l.scope !== undefined)!.key).scope?.noun ?? "Scope"}
            </span>
          )}
          <span role="columnheader">Status</span>
          <span role="columnheader">
            <VisuallyHidden>Details</VisuallyHidden>
          </span>
        </div>
        {seats.map((seat) => (
          <div key={seat.handle} className="agent-access-seat" role="rowgroup">
            {lanes.map((lane, i) => {
              const saved = seat.lanes[lane.key];
              if (!saved) return null;
              const want = desiredOf(seat, lane.key);
              const drafted = !sameDesired(lane, want, saved.desired);
              const status = statusOf(lane, saved, drafted, data.syncing, seat.departed);
              const hint = emptyScopeHint(lane, want);
              const key = `${seat.handle}:${lane.key}`;
              const expanded = open[key] ?? false;
              const off = !want.enabled;
              const words = laneWords(lane.key);
              return (
                <div
                  key={lane.key}
                  className={expanded ? "agent-access-lane is-open" : "agent-access-lane"}
                >
                  <div className="agent-access-row" role="row">
                    <span className="agent-access-who" role="cell">
                      {only ? (
                        <span className="agent-access-name">{lane.name}</span>
                      ) : (
                        i === 0 && (
                          <>
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
                          </>
                        )
                      )}
                    </span>
                    {lanes.length > 1 && !only && (
                      <span role="cell" className="agent-access-product">
                        {lane.name}
                      </span>
                    )}
                    <span role="cell">
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
                    </span>
                    <span role="cell" className="agent-access-level">
                      {managedOutside(lane) ? (
                        <span className="agent-access-quiet">{MANAGED_OUTSIDE}</span>
                      ) : readOnly ? (
                        <span className="agent-access-quiet">
                          {lane.permissions
                            ? permissionCount(lane, want.permissions)
                            : (laneWords(lane.key).fixed ?? levelName(lane, want.level))}
                        </span>
                      ) : lane.levels.length > 0 ? (
                        <Select
                          size="sm"
                          width="full"
                          menuClassName="agent-access-menu"
                          ariaLabel={`${seat.name}'s ${lane.name} access`}
                          value={want.level}
                          options={levelOptions(lane)}
                          disabled={off || locked || seat.departed}
                          onChange={(v) => change(seat, lane, { level: String(v) })}
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
                    </span>
                    {scoped && (
                      <span role="cell" className="agent-access-scope">
                        {managedOutside(lane) ? null : lane.scope !== undefined &&
                          words.scope &&
                          readOnly ? (
                          <span className="agent-access-quiet">{scopeWords(lane, want.scope)}</span>
                        ) : lane.scope !== undefined && words.scope ? (
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
                                  : plural(
                                      sel.length,
                                      words.scope!.singular,
                                      words.scope!.noun.toLowerCase(),
                                    )
                            }
                          />
                        ) : null}
                      </span>
                    )}
                    <span role="cell">
                      <Tag
                        size="sm"
                        variant={status.tone}
                        appearance={status.outline ? "outline" : "soft"}
                        leadingIcon={status.busy ? <Spinner size="em" /> : undefined}
                      >
                        {status.label}
                      </Tag>
                    </span>
                    <span role="cell">
                      <IconButton
                        size="sm"
                        variant="ghost"
                        label={`${expanded ? "Hide" : "Show"} ${seat.name}'s ${lane.name} details`}
                        aria-expanded={expanded}
                        icon={
                          expanded ? (
                            <ChevronDownGlyph size="sm" />
                          ) : (
                            <ChevronRightGlyph size="sm" />
                          )
                        }
                        onClick={() => setOpen((o) => ({ ...o, [key]: !expanded }))}
                      />
                    </span>
                  </div>
                  {!drafted && (
                    <LaneMessage
                      lane={lane}
                      seat={seat}
                      saved={saved}
                      syncing={data.syncing}
                      now={now}
                      held={held}
                      readOnly={readOnly !== undefined}
                      step={stepFor?.(seat.handle)}
                      onAct={(a) => void act(seat, lane, a)}
                      onInstall={() => void install(seat, lane)}
                      onRemove={() => setRemoving({ seat, lane })}
                    />
                  )}
                  {hint && <p className="agent-access-hint t-caption">{hint}</p>}
                  {expanded && (
                    <Details
                      lane={lane}
                      seat={seat}
                      saved={saved}
                      now={now}
                      actions={availableActions(lane, saved, held !== undefined, data.syncing)}
                      onAction={(a) =>
                        a === "rotate"
                          ? void act(seat, lane, "rotate")
                          : setRemoving({ seat, lane })
                      }
                    />
                  )}
                </div>
              );
            })}
          </div>
        ))}
      </div>

      {said && (
        <Callout variant="danger" live="polite">
          {said}
        </Callout>
      )}

      {changes > 0 && !readOnly && (
        <div className="agent-access-bar" role="region" aria-label="Unsaved changes">
          <span className="t-label">
            {changes === 1 ? "One change" : `${changes} changes`} not saved
          </span>
          <span className="t-caption">Nothing changes at {name} until you save.</span>
          <span className="agent-access-bar-actions">
            <Button size="small" variant="ghost" onClick={() => setDraft({})}>
              Discard
            </Button>
            <Button size="small" variant="primary" onClick={() => setReview(true)}>
              Review changes
            </Button>
          </span>
        </div>
      )}

      {review && (
        <ReviewDialog
          kind={kind}
          name={name}
          data={data}
          draft={draft}
          onClose={() => setReview(false)}
          onSaved={() => {
            setDraft({});
            setReview(false);
            reload();
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
    </div>
  );
}

/** What a lane is waiting on, under its row: a refusal, a person's step, or a change made by hand. */
function LaneMessage({
  lane,
  seat,
  saved,
  syncing,
  now,
  held,
  readOnly,
  step,
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
  onAct: (action: "retry" | "keep" | "restore" | "done") => void;
  onInstall: () => void;
  onRemove: () => void;
}) {
  if (syncing && saved.plan.length > 0) return null;
  const busy = held !== undefined;
  const keep = !busy && !readOnly;
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
          busy || !saved.observed.exists ? undefined : (
            <Button size="small" variant="secondary" onClick={onRemove}>
              Remove account
            </Button>
          )
        }
      >
        {saved.observed.exists
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
            {!busy && (
              <Button size="small" variant="secondary" onClick={() => onAct("retry")}>
                Retry
              </Button>
            )}
            {at(saved.sync.error.action_url)}
            {keep && saved.desired.level !== saved.observed.level && saved.observed.level && (
              <Button size="small" variant="ghost" onClick={() => onAct("keep")}>
                Keep {levelName(lane, saved.observed.level)}
              </Button>
            )}
          </span>
        }
      >
        <p>{saved.sync.error.said}</p>
        <p>{saved.sync.error.remedy}</p>
      </Callout>
    );
  } else if (saved.observed.changed_by_hand_at && saved.plan.length > 0) {
    const handOff =
      saved.observed.exists && !saved.observed.enabled && saved.observed.disabled_by === "hand";
    const when = relTime(saved.observed.changed_by_hand_at, now);
    const levelMoved = saved.desired.level !== saved.observed.level;
    body = handOff ? (
      <Callout
        variant="warning"
        title={`Turned off at ${lane.name} ${when} by somebody else`}
        action={
          busy ? undefined : (
            <span className="agent-access-actions">
              <Button size="small" variant="secondary" onClick={() => onAct("restore")}>
                Turn back on
              </Button>
              {keep && (
                <Button size="small" variant="ghost" onClick={() => onAct("keep")}>
                  Keep off
                </Button>
              )}
            </span>
          )
        }
      >
        Crewlet leaves {seat.name} off until you decide, in case that was deliberate.
        {keep && " Keep off turns it off here too."}
      </Callout>
    ) : (
      <Callout
        variant="warning"
        title={`Changed at ${lane.name} ${when}`}
        action={
          busy ? undefined : (
            <span className="agent-access-actions">
              <Button size="small" variant="secondary" onClick={() => onAct("restore")}>
                Restore now
              </Button>
              {keep && (
                <Button size="small" variant="ghost" onClick={() => onAct("keep")}>
                  Keep {lane.name}&apos;s setting
                </Button>
              )}
            </span>
          )
        }
      >
        {lane.name} shows{" "}
        {levelMoved ? accessWords(lane, saved.observed) : scopeWords(lane, saved.observed.scope)},
        and Crewlet&apos;s setting is{" "}
        {levelMoved ? levelName(lane, saved.desired.level) : scopeWords(lane, saved.desired.scope)}.
        {keep
          ? ` The next sync restores Crewlet's setting unless you keep ${lane.name}'s.`
          : " The next sync restores Crewlet's setting."}
      </Callout>
    );
  } else if (saved.sync?.outcome === "waiting" || saved.plan.some((s) => s.by === "you")) {
    const waiting =
      (saved.sync?.steps ?? []).find((s) => s.by === "you" && !s.done) ??
      saved.plan.find((s) => s.by === "you");
    if (!waiting) return null;
    body = (
      <Callout
        variant="warning"
        title="Waiting on a person"
        action={
          <span className="agent-access-actions">
            {step ??
              at(waiting.action_url) ??
              (!busy && laneWords(lane.key).install ? (
                <Button size="small" variant="secondary" onClick={onInstall}>
                  {laneWords(lane.key).install}
                </Button>
              ) : null)}
            {!busy && !step && (
              <Button size="small" variant="ghost" onClick={() => onAct("done")}>
                Done, check again
              </Button>
            )}
          </span>
        }
      >
        <p>{waiting.text}.</p>
        {waiting.note && <p>{waiting.note}</p>}
      </Callout>
    );
  }
  return body ? <div className="agent-access-message">{body}</div> : null;
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
  onAction,
}: {
  lane: Lane;
  seat: AccessSeat;
  saved: AccessSeatLane;
  now: number;
  actions: SeatAction[];
  onAction: (action: SeatAction) => void;
}) {
  const o = saved.observed;
  const buttons =
    actions.length > 0 ? (
      <span className="agent-access-detail-actions">
        {actions.map((a) => (
          <Button key={a} size="small" variant="ghost" onClick={() => onAction(a)}>
            {ACTION_WORDS[a]}
          </Button>
        ))}
      </span>
    ) : null;
  return (
    <div role="row">
      <div className="agent-access-details-cell" role="cell" aria-colspan={7}>
        {!saved.desired.enabled ? (
          <p className="agent-access-off">{offSentence(lane, saved)}</p>
        ) : (
          <DescriptionList
            uniform
            dense
            fit
            className="agent-access-facts"
            aria-label={`${seat.name} on ${lane.name}`}
            items={[
              ["Account", o.exists ? (o.account ?? "Created") : "Not created yet"],
              [
                laneWords(lane.key).access,
                o.exists || managedOutside(lane) ? accessWords(lane, o) : "Not granted yet",
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
    </div>
  );
}
