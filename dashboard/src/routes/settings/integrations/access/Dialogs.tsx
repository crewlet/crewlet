/**
 * The Agents tab's dialogs.
 *
 * - THE REVIEW, before a save: one card per agent, headed by its face, name
 *   and handle with the change in a word ("Turned off"), then what the draft
 *   changes as labels and values ("Deployments: Read-only → Read and write",
 *   in the vendor's words), what turning an agent off does there, and what
 *   the next sync will do at the vendor because of it, as a timeline in the
 *   agent's own colour. The plan is the engine's (`POST .../access/plan`), so
 *   the preview and the sync cannot disagree. Where the plan says a save
 *   grants administration access, the review says the save asks for a fresh
 *   confirmation of who you are (D10); the transport asks for it when the
 *   engine refuses.
 * - THE REMOVAL of one agent's account, allowed only once it is off (D1).
 * - GITHUB'S PERMISSIONS for one agent, edited into the draft in GitHub's own
 *   words, so the row stays at one size.
 * - ONE AGENT'S SCOPE (repositories, projects, spaces, channels, servers),
 *   ticked from what the connection reaches and edited into the draft, so the
 *   row shows the choice and never a field.
 */

import {
  useEffect,
  useEffectEvent,
  useRef,
  useState,
  type ComponentType,
  type ReactNode,
} from "react";
import {
  Button,
  Callout,
  Card,
  ChangeList,
  ChoiceList,
  DescriptionList,
  Inline,
  Input,
  List,
  SegmentedControl,
  Spinner,
  Stack,
  Tag,
  Text,
  Timeline,
  VisuallyHidden,
  type ChangeListRow,
  ScopeList,
} from "@crewlethq/ui";
import { Modal } from "~/components/Modal.tsx";
import {
  BanGlyph,
  CircleStopGlyph,
  EyeGlyph,
  FolderGlyph,
  HandGlyph,
  KeyGlyph,
  PlusGlyph,
  PowerGlyph,
  RefreshCwGlyph,
  RotateCcwGlyph,
  SearchGlyph,
  ShieldCheckGlyph,
  SlidersVerticalGlyph,
  TrashGlyph,
  UserGlyph,
  type GlyphProps,
} from "@crewlethq/icons/glyphs";
import type {
  AccessAnswer,
  AccessDesired,
  AccessPlanAnswer,
  AccessSeat,
  AccessStep,
  GitHubAccess,
} from "~/contract/access.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { handleLabel } from "~/lib/seats.ts";
import { marked } from "~/ui/Problems.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import {
  accessApi,
  isStale,
  placedRefusal,
  refusalWords,
  type AccessDraft,
  type PlacedRefusal,
} from "./api.ts";
import {
  changeSummary,
  grantWords,
  lanesOf,
  levelName,
  levelsHeading,
  refusedScope,
  reviewWords,
  scopeApplies,
  scopeChoices,
  type ScopeChoice,
  scopeWords,
  takesAccessAway,
  takesTyped,
  type Lane,
} from "./logic.ts";
import { GITHUB_ACCESS_NAMES, laneWords } from "./words.ts";

/**
 * What changes on one agent's lane, as labels and values. Turning the lane on
 * or off is no row: the card's tag says it ("Turned on").
 */
function rows(lane: Lane, from: AccessDesired, to: AccessDesired): ChangeListRow[] {
  const out: ChangeListRow[] = [];
  const access = laneWords(lane.key).access;
  if (from.enabled !== to.enabled) {
    if (!to.enabled) return out;
    // TURNED ON: what it is turned on with.
    if (lane.levels.length > 0)
      out.push({ kind: "fact", label: access, value: levelName(lane, to.level) });
    for (const perm of lane.permissions ?? []) {
      const held = to.permissions?.[perm.key];
      if (held) out.push({ kind: "fact", label: perm.name, value: GITHUB_ACCESS_NAMES[held] });
    }
    if (scopeApplies(lane, to))
      out.push({
        kind: "fact",
        label: laneWords(lane.key).scope?.noun ?? "Scope",
        value: scopeWords(lane, to.scope),
      });
    return out;
  }
  if (!to.enabled) return out;
  if (lane.levels.length > 0 && (from.level ?? "") !== (to.level ?? "")) {
    out.push({
      kind: "changed",
      label: access,
      from: levelName(lane, from.level),
      to: levelName(lane, to.level),
    });
  }
  for (const perm of lane.permissions ?? []) {
    const a = from.permissions?.[perm.key] ?? "none";
    const b = to.permissions?.[perm.key] ?? "none";
    if (a !== b)
      out.push({
        kind: "changed",
        label: perm.name,
        from: GITHUB_ACCESS_NAMES[a],
        to: GITHUB_ACCESS_NAMES[b],
      });
  }
  if (scopeApplies(lane, to) && scopeWords(lane, from.scope) !== scopeWords(lane, to.scope)) {
    out.push({
      kind: "changed",
      label: laneWords(lane.key).scope?.noun ?? "Scope",
      from: scopeWords(lane, from.scope),
      to: scopeWords(lane, to.scope),
    });
  }
  if (grantWords(lane, from.grants) !== grantWords(lane, to.grants)) {
    out.push({
      kind: "changed",
      label: "Grants",
      from: grantWords(lane, from.grants),
      to: grantWords(lane, to.grants),
    });
  }
  return out;
}

/** A glyph for each kind of step, so a plan reads by its shapes before its words. */
const VERB_GLYPH: Record<AccessStep["verb"], ComponentType<GlyphProps>> = {
  create: PlusGlyph,
  grant: ShieldCheckGlyph,
  mint: KeyGlyph,
  enable: PowerGlyph,
  rerole: SlidersVerticalGlyph,
  scope: FolderGlyph,
  ungrant: BanGlyph,
  disable: CircleStopGlyph,
  revoke: BanGlyph,
  remove: TrashGlyph,
  rotate: RefreshCwGlyph,
  restore: RotateCcwGlyph,
  approve: HandGlyph,
  observe: EyeGlyph,
};

/**
 * The steps a sync takes, in order, as a timeline in the agent's own colour:
 * each node holds its kind of step's glyph, and a step that takes access away
 * takes the danger node. The rail keeps the order, so no step is numbered.
 *
 * WHO TAKES A STEP is said only where it is a person: Crewlet takes every
 * other, so a "You" tag beside a step's words is the one mark a reader needs.
 * A variable a step names is drawn as code ([marked]).
 */
function Steps({
  steps,
  label,
  avatar,
}: {
  steps: AccessStep[];
  label: string;
  avatar: AgentAvatar | null;
}) {
  return (
    <Timeline itemLabel="Step" hue={avatar?.color} density="compact" aria-label={label}>
      {steps.map((s, i) => {
        const Glyph = VERB_GLYPH[s.verb];
        return (
          <Timeline.Item
            key={i}
            state={takesAccessAway(s) ? "danger" : "default"}
            marker={<Glyph size="xs" aria-hidden />}
          >
            <span className="agent-access-step">
              <Text>{marked(s.text)}</Text>
              {s.by === "you" && (
                <Tag size="xs" variant="warning" leadingIcon={<UserGlyph size="xs" />}>
                  You
                </Tag>
              )}
            </span>
            {s.note && (
              <Text variant="caption" tone="secondary">
                {marked(s.note)}
              </Text>
            )}
          </Timeline.Item>
        );
      })}
    </Timeline>
  );
}

/**
 * One agent's change on one lane, as a card: its face, name and handle beside
 * the change in a word, what changes in the vendor's words, what turning it
 * off does there, then the next sync's steps. A card that turns an agent off
 * carries the danger edge, so it stands out among several.
 */
function SeatReview({
  seat,
  lane,
  from,
  to,
  steps,
  named,
  name,
  avatar,
}: {
  seat: AccessSeat;
  lane: Lane;
  from: AccessDesired;
  to: AccessDesired;
  /** The next sync's steps for this lane, or undefined while the plan is read. */
  steps: AccessStep[] | undefined;
  /** The integration has more than one lane (Jira and Confluence), so the card names its own. */
  named: boolean;
  name: string;
  avatar: AgentAvatar | null;
}) {
  const summary = changeSummary(lane, from, to);
  const changes = rows(lane, from, to);
  const off = from.enabled && !to.enabled;
  return (
    <Card
      as="section"
      variant="outlined"
      padding="md"
      rail={off ? "danger" : undefined}
      className="agent-access-review-seat"
      aria-label={`${seat.name} on ${lane.name}`}
    >
      <div className="agent-access-review-head">
        <SeatAvatar name={seat.name} kind="agent" avatar={avatar} size="lg" decorative />
        <div className="agent-access-review-who">
          <Text as="h3">
            {seat.name}
            {named ? ` on ${lane.name}` : ""}
          </Text>
          <Text variant="caption" mono>
            {handleLabel(seat.handle)}
          </Text>
        </div>
        <Tag size="sm" variant={summary.tone} className="agent-access-review-change">
          {summary.label}
        </Tag>
      </div>
      {changes.length > 0 && <ChangeList rows={changes} />}
      {off && (
        <Text variant="caption" tone="secondary">
          {laneWords(lane.key).off}
        </Text>
      )}
      {steps ? (
        steps.length > 0 ? (
          <Steps steps={steps} label={`What the next sync does at ${name}`} avatar={avatar} />
        ) : (
          <Text variant="caption">Nothing to do. {name} already matches.</Text>
        )
      ) : (
        <Spinner size="sm" />
      )}
    </Card>
  );
}

export function ReviewDialog({
  kind,
  name,
  data,
  draft,
  onClose,
  onSaved,
  onStale,
  onRefused,
  avatarOf,
}: {
  kind: string;
  name: string;
  data: AccessAnswer;
  draft: AccessDraft;
  /** How each agent is drawn, as the tab's own rows draw it. */
  avatarOf: (handle: string) => AgentAvatar | null;
  onClose: () => void;
  onSaved: (answer: AccessAnswer) => void;
  /** A refusal about one seat's control, which the tab draws beside that control. */
  onRefused: (refusal: PlacedRefusal) => void;
  /**
   * The document moved since the tab read it, said by a refused save or by a
   * plan drawn from a newer revision: read again, keeping the draft.
   */
  onStale: () => void;
}) {
  const [plan, setPlan] = useState<AccessPlanAnswer | null>(null);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  // PLANNED AGAIN FOR EVERY REVISION READ, so a review left open across a
  // colleague's save shows what the next sync does now.
  //
  // THE PLAN NAMES THE REVISION IT WAS DRAWN FROM, the document as it stands
  // now. One the tab has not read means the document moved since the tab read
  // it (the access migration writes one just after a first connect), and a
  // save sent with the tab's revision would be refused as stale. So the tab
  // reads again, keeping the draft, once per newer revision, and Save waits
  // for the plan drawn from the revision read.
  const revision = data.revision;
  const asked = useRef("");
  const moved = useEffectEvent((drawnFrom: string) => {
    if (asked.current === drawnFrom) return;
    asked.current = drawnFrom;
    onStale();
  });
  // A REFUSAL ABOUT ONE SEAT'S CONTROL goes back to the tab, beside it;
  // any other stays here, where the save was asked for.
  const refused = (err: unknown) => {
    const placed = placedRefusal(err);
    if (placed) onRefused(placed);
    else setError(refusalWords(err));
  };
  const planRefused = useEffectEvent(refused);
  useEffect(() => {
    let live = true;
    setPlan(null);
    accessApi.plan(kind, draft).then(
      (p) => {
        if (!live) return;
        setPlan(p);
        if (p.revision !== revision) moved(p.revision);
      },
      (err: unknown) => {
        if (live) planRefused(err);
      },
    );
    return () => {
      live = false;
    };
  }, [kind, draft, revision]);
  const behind = plan !== null && plan.revision !== revision;

  const lanes = lanesOf(data);
  const changed = data.seats.flatMap((seat) =>
    lanes
      .filter((lane) => draft[seat.handle]?.[lane.key] && seat.lanes[lane.key])
      .map((lane) => ({
        seat,
        lane,
        from: seat.lanes[lane.key]!.desired,
        to: draft[seat.handle]![lane.key]!,
      })),
  );
  const people = plan?.plans.some((p) => p.steps.some((s) => s.by === "you")) ?? false;
  const raises = changed.filter(
    ({ seat, lane }) =>
      plan?.plans.find((p) => p.handle === seat.handle && p.lane === lane.key)?.step_up,
  );
  // THE AGENTS WHOSE OPEN WORK THE SAVE ASKS THEM TO REVIEW, named as the tab
  // names them: the engine counts, per agent whose access the draft changes,
  // what it raised under its access now.
  const reviews = (plan?.reviews ?? []).map((review) => ({
    handle: review.handle,
    words: reviewWords(
      review.open,
      data.seats.find((seat) => seat.handle === review.handle)?.name ?? handleLabel(review.handle),
      review.at_least,
    ),
  }));

  return (
    <Modal
      open
      size="lg"
      onClose={onClose}
      title={`Review changes to ${name}`}
      subtitle={`Nothing changes at ${name} until you save.`}
      footerStart={
        people ? (
          <span className="t-caption">Steps marked You need somebody at {name}.</span>
        ) : undefined
      }
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Back
          </Button>
          <Button
            size="small"
            variant="primary"
            loading={saving}
            disabledReason={
              !plan
                ? "Reading what the sync will do."
                : behind
                  ? "Reading the latest settings."
                  : undefined
            }
            onClick={async () => {
              setSaving(true);
              setError("");
              try {
                onSaved(await accessApi.save(kind, draft, data.revision));
              } catch (err) {
                refused(err);
                setSaving(false);
                if (isStale(err)) onStale();
              }
            }}
          >
            Save and sync
          </Button>
        </>
      }
    >
      <div className="agent-access-review agent-access-review-cards">
        {error && (
          <Callout variant="danger" live="polite">
            {error}
          </Callout>
        )}
        {plan?.step_up && (
          <Callout variant="warning" title="Administration access">
            {raises.length > 0
              ? `${raises.map(({ seat }) => seat.name).join(", ")} ${raises.length === 1 ? "gets" : "get"} administration access at ${name}. Saving asks you to confirm it is you.`
              : `This change grants administration access at ${name}. Saving asks you to confirm it is you.`}
          </Callout>
        )}
        {reviews.length > 0 && (
          <Callout variant="info" title="Open work to review">
            {reviews.length === 1 ? (
              reviews[0]?.words
            ) : (
              <List variant="bulleted" density="compact">
                {reviews.map((review) => (
                  <List.Item key={review.handle}>{review.words}</List.Item>
                ))}
              </List>
            )}
          </Callout>
        )}
        {changed.map(({ seat, lane, from, to }) => (
          <SeatReview
            key={`${seat.handle}:${lane.key}`}
            seat={seat}
            lane={lane}
            from={from}
            to={to}
            steps={plan?.plans.find((p) => p.handle === seat.handle && p.lane === lane.key)?.steps}
            named={lanes.length > 1}
            name={name}
            avatar={avatarOf(seat.handle)}
          />
        ))}
      </div>
    </Modal>
  );
}

/** A confirmation before one action on one seat's lane, saying what it does at the vendor. */
function SeatConfirm({
  title,
  confirm,
  danger,
  onClose,
  onConfirm,
  children,
}: {
  title: string;
  confirm: string;
  danger?: boolean;
  onClose: () => void;
  onConfirm: () => Promise<void>;
  children: ReactNode;
}) {
  const [busy, setBusy] = useState(false);
  return (
    <Modal
      open
      size="md"
      role="alertdialog"
      onClose={onClose}
      title={title}
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant={danger ? "danger" : "primary"}
            loading={busy}
            onClick={async () => {
              setBusy(true);
              await onConfirm();
            }}
          >
            {confirm}
          </Button>
        </>
      }
    >
      <div className="agent-access-review">{children}</div>
    </Modal>
  );
}

export function RemoveDialog({
  name,
  seat,
  lane,
  onClose,
  onRemove,
}: {
  name: string;
  seat: AccessSeat;
  lane: Lane;
  onClose: () => void;
  onRemove: () => Promise<void>;
}) {
  return (
    <SeatConfirm
      title={`Remove ${seat.name}'s ${lane.name} account?`}
      confirm="Remove account"
      danger
      onClose={onClose}
      onConfirm={onRemove}
    >
      <p>{laneWords(lane.key).remove}</p>
      <p className="t-caption">
        Turning {seat.name} on at {name} again later creates a new account with a new credential.
      </p>
    </SeatConfirm>
  );
}

/** Rotation, said before it runs: what the vendor does, and what stops working. */
export function RotateDialog({
  seat,
  lane,
  onClose,
  onRotate,
}: {
  seat: AccessSeat;
  lane: Lane;
  onClose: () => void;
  onRotate: () => Promise<void>;
}) {
  return (
    <SeatConfirm
      title={`Rotate ${seat.name}'s ${lane.name} credential?`}
      confirm="Rotate credential"
      onClose={onClose}
      onConfirm={onRotate}
    >
      <p>{laneWords(lane.key).rotate}</p>
    </SeatConfirm>
  );
}

export function PermissionsDialog({
  seat,
  lane,
  want,
  onChange,
  onClose,
}: {
  seat: AccessSeat;
  lane: Lane;
  want: AccessDesired;
  onChange: (key: string, access: "none" | GitHubAccess) => void;
  onClose: () => void;
}) {
  return (
    <Modal
      open
      size="md"
      onClose={onClose}
      title={`${seat.name}'s ${lane.name} permissions`}
      subtitle={levelsHeading(lane)}
      footer={
        <Button size="small" variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <DescriptionList
        uniform
        fit
        items={(lane.permissions ?? []).map((perm) => [
          perm.name,
          <SegmentedControl
            key={perm.key}
            size="sm"
            semantics="radio"
            label={`${seat.name}'s ${perm.name} permission`}
            value={want.permissions?.[perm.key] ?? "none"}
            onValueChange={(v) => onChange(perm.key, v as "none" | GitHubAccess)}
            options={(perm.write_only
              ? (["none", "write"] as const)
              : (["none", "read", "write"] as const)
            ).map((v) => ({ value: v, label: GITHUB_ACCESS_NAMES[v] }))}
          />,
        ])}
      />
    </Modal>
  );
}

/** Past this many entries the list is searched rather than scrolled: about what a phone's dialog shows at once. */
const SCOPE_SEARCH_FROM = 8;

/**
 * One agent's scope, ticked from what the connection reaches; an open one also takes a typed
 * entry. An empty scope is a choice of its own that clears the list, unless it reaches nothing.
 */
export function ScopeDialog({
  seat,
  lane,
  want,
  refused,
  note,
  onChange,
  onClose,
}: {
  seat: AccessSeat;
  lane: Lane;
  want: AccessDesired;
  /** The engine's refusal of this seat's scope, said over the list until the draft changes. */
  refused?: string | undefined;
  /** Why the entries show by identifier and not by name, said under the list's label. */
  note?: string | undefined;
  onChange: (scope: string[]) => void;
  onClose: () => void;
}) {
  const words = laneWords(lane.key).scope;
  const noun = (words?.noun ?? "Scope").toLowerCase();
  const singular = words?.singular ?? "entry";
  const held = want.scope ?? [];
  // CHOSEN IS HELD HERE, not read off the draft, so unticking the last entry
  // keeps the list open under the pointer rather than folding it away.
  const [chosen, setChosen] = useState(held.length > 0);
  const [term, setTerm] = useState("");
  const body = useRef<HTMLDivElement>(null);
  const title = `${seat.name}'s ${lane.name} ${noun}`;
  const choices = scopeChoices(lane, held);
  const find = term.trim().toLowerCase();
  const shown = find
    ? choices.filter((c) =>
        `${c.label} ${c.value} ${c.description ?? ""}`.toLowerCase().includes(find),
      )
    : choices;
  // A SEARCH IS NOT AN ENTRY: where the engine sends an entry's shape (an
  // Apple ID's digits), a name typed to find one is never offered.
  const typed =
    takesTyped(lane, term) && !choices.some((c) => c.value.toLowerCase() === find)
      ? term.trim()
      : null;
  const add = () => {
    if (!typed) return;
    onChange([...held, typed]);
    setTerm("");
  };
  // "AN APP", "AN AD ACCOUNT": the article follows the noun's own sound.
  const one = `${/^[aeiou]/i.test(singular) ? "an" : "a"} ${singular}`;
  const searchWords = lane.scope_open ? `Find or add ${one}` : `Find ${one}`;
  const alone = lane.scope_required || !words;

  // EVERY ENTRY BY ITS NAME: the kit's scope list draws the vendor's name
  // first, its second identifier quiet under it (an app's bundle ID), and the
  // stored ID quiet at the row's end where the two differ.
  const rows = (list: ScopeChoice[]) =>
    list.map((c) => ({
      value: c.value,
      label: c.label,
      ...(lane.scope_details?.[c.value] ? { detail: lane.scope_details[c.value] } : {}),
      ...(c.label !== c.value ? { id: c.value } : {}),
      ...(lane.scope_groups?.[c.value] ? { group: lane.scope_groups[c.value] } : {}),
    }));
  // ENTRIES ARE CHOSEN BY NAME: where entries are IDs the vendor names (an
  // Apple ID, a property's digits), are grouped or carry a second identifier,
  // the list searches itself and is counted, with Choose all, and an entry it
  // does not list is added by its ID.
  const byName =
    lane.scope_details !== undefined ||
    lane.scope_typed !== undefined ||
    lane.scope_groups !== undefined;
  const typedReason = term.trim()
    ? (words?.typedHint ?? `Type ${one}'s ID to add it`)
    : `Type ${one}'s ID to add it`;
  // THE ENGINE'S REFUSAL OVER THE LANE'S OWN, then why names are missing.
  const said = (
    <>
      {(refused || refusedScope(lane, want, seat.name)) && (
        <p
          className={refused ? "t-caption agent-access-refused" : "t-caption"}
          role={refused ? "alert" : undefined}
        >
          {refused ?? refusedScope(lane, want, seat.name)}
        </p>
      )}
      {note && <p className="t-caption">{note}</p>}
    </>
  );
  const addWords = `Add ${one} by its ID`;
  const picker = byName ? (
    <Stack gap={3}>
      <ScopeList
        label={words?.noun ?? "Scope"}
        options={rows(choices)}
        value={held}
        onChange={onChange}
        searchFrom={SCOPE_SEARCH_FROM}
        bulk={choices.length > 0}
        labels={{
          none: lane.scope_unlisted ?? `${words?.none ?? `No ${singular} to choose from`}.`,
        }}
      />
      {lane.scope_live && choices.length > 0 && words?.listed && (
        <Text as="p" variant="caption" tone="secondary">
          {words.listed}
        </Text>
      )}
      {lane.scope_open && (
        <Inline gap={2} align="center">
          <Input
            aria-label={addWords}
            placeholder={addWords}
            inputSize="sm"
            value={term}
            onChange={(e) => setTerm(e.target.value)}
          />
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<PlusGlyph size="sm" />}
            disabledReason={typed ? undefined : typedReason}
            onClick={add}
          >
            Add
          </Button>
        </Inline>
      )}
      {/* WHAT AN ENTRY IS, said where the text typed is not one. */}
      {lane.scope_open && term.trim() && !typed && words?.typedHint && (
        <Text as="p" variant="caption" tone="secondary">
          {`${words.typedHint}.`}
        </Text>
      )}
      {said}
    </Stack>
  ) : (
    <Stack gap={3}>
      {(lane.scope_open || choices.length > SCOPE_SEARCH_FROM) && (
        <Input
          type="search"
          aria-label={searchWords}
          placeholder={searchWords}
          inputSize="sm"
          width="full"
          leading={<SearchGlyph size="sm" />}
          value={term}
          onChange={(e) => setTerm(e.target.value)}
        />
      )}
      {typed && (
        <span>
          <Button
            size="small"
            variant="secondary"
            leadingIcon={<PlusGlyph size="sm" />}
            onClick={add}
          >
            {`Add “${typed}”`}
          </Button>
        </span>
      )}
      {/* THE FIND BOX ABOVE FILTERS, and may add what it holds, so the list
          draws what it shows and never searches a second time. */}
      <ScopeList
        label={words?.noun ?? "Scope"}
        options={rows(shown)}
        value={held}
        onChange={onChange}
        searchFrom={Infinity}
        bulk={false}
        labels={{
          none: find ? `No ${singular} matches “${term.trim()}”.` : `Type ${one} to add it.`,
        }}
      />
      {said}
    </Stack>
  );

  return (
    <Modal
      open
      size="md"
      onClose={onClose}
      title={title}
      // FOCUS STARTS ON WHAT IS CHOSEN: the search field over the list, or the
      // selected choice, never the first choice drawn when it is not the one.
      initialFocus={() =>
        body.current?.querySelector<HTMLElement>('input[type="search"]') ??
        body.current?.querySelector<HTMLElement>('input[type="radio"]:checked') ??
        null
      }
      // ENTER IN THE SEARCH FIELD ADDS what was typed, where an entry may be.
      {...(lane.scope_open ? { onSubmit: add } : {})}
      footer={
        <Button size="small" variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <div ref={body}>
        {alone ? (
          picker
        ) : (
          <ChoiceList
            label={title}
            value={chosen ? "chosen" : "empty"}
            onChange={(v) => {
              const next = v === "chosen";
              setChosen(next);
              if (!next) {
                setTerm("");
                if (held.length > 0) onChange([]);
              }
            }}
            options={[
              { value: "empty", label: words.empty },
              {
                value: "chosen",
                // WHAT IS TICKED, COUNTED beside the choice, so a long list says
                // it without scrolling.
                label: chosen ? (
                  <Inline as="span" gap={2} align="baseline">
                    <span>{`Chosen ${noun}`}</span>
                    <Text as="span" variant="caption" tone="secondary">
                      <VisuallyHidden>, </VisuallyHidden>
                      {held.length === 0 ? "none chosen" : `${held.length.toLocaleString()} chosen`}
                    </Text>
                  </Inline>
                ) : (
                  `Chosen ${noun}`
                ),
                panel: picker,
              },
            ]}
          />
        )}
      </div>
    </Modal>
  );
}
