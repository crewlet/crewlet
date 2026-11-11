/**
 * The agents' access as Ask Crewlet reads and proposes it (`useAssistantDraft`,
 * from `AgentsAccess.tsx`): every value a person sets on an integration's Agents
 * tab, agent by agent and product by product, in the tab's own words.
 *
 * # A field for each control
 *
 * An agent's product is a row of the tab, and each control on the row is a
 * field: its switch (On or Off); its level where the product has levels, one,
 * or several on a product of sets; each GitHub permission where it has those;
 * and its scope where it narrows by one. A product whose access is managed
 * outside Crewlet, or fixed by the vendor, has its switch alone, and an agent
 * that left the company has no control, so no field. A value reads as its
 * control shows it: a level by the name its menu gives it, a permission in
 * GitHub's words, a scope entry by the vendor's name.
 *
 * # What is not saved comes first
 *
 * A draft lists at most forty fields, and a page context over its budget sheds
 * them from the end. So the rows holding a change not saved yet come first, in
 * the tab's order, then every other row: what the person is about to review is
 * the last thing either bound cuts.
 *
 * # Named by place, the agent in the label
 *
 * A handle cannot be a path: it may start with a digit, holds dashes, runs to
 * 64 characters by itself, and may hold a word the engine reads as a
 * credential (`token-bot`), whose field would be sent redacted and never set.
 * So a path names the agent by its place on the tab (`agent_3.jira.level`) and
 * the product by its key, and the label names the agent by name and handle, as
 * the tab's row does, the name shortened before the handle is. A change is
 * bound to the draft it was prepared on, and that draft says which agent each
 * place is: another agent at the place is another label, so the change goes
 * stale rather than lands on the wrong agent.
 *
 * # A proposal is a control's own change
 *
 * Each field takes exactly what its control offers: a level from the
 * product's own list, never one offered disabled; a permission's own values
 * (Workflows is No access or Read and write); scope entries from what the
 * connection reaches, or typed where the product takes typed entries. An agent
 * that is off has no level, permission or scope control, so those wait on its
 * switch, which a change may turn first: a change is written in its fields'
 * order, and each row's switch is its first field. What a field takes it lands
 * through the tab's own update path (`logic.ts`), the one its control calls,
 * so a proposal is the draft the control would have made. Saving stays the
 * person's, through Review.
 *
 * # A refusal is one short line
 *
 * What a control would not take is refused in a sentence the card shows and
 * the page's report carries back to the model, which the engine takes as one
 * short line. So each refusal is written within it (`said`): the agent's
 * name, the product's and the value the proposal typed give way, and what the
 * control does offer is listed as far as the line holds, then counted. Said
 * whole, a refusal naming a dozen repositories, or a level nobody's role is
 * called among eight, was past the line, and the report that carried it was
 * refused: the model never heard why.
 *
 * # Several tabs on one page are one draft
 *
 * An agent's own Access tab draws one Agents tab per integration, and the page
 * context carries one draft, the newest any component declared: tabs declaring
 * their own would take turns being the draft. So that page gathers its tabs
 * ([AccessDrafts]) and declares one draft over all of them ([accessDrafts]),
 * the rows holding a change first across every integration, and each tab
 * saves its own through its own Review, as before.
 */

import { createContext, useLayoutEffect, useSyncExternalStore } from "react";
import {
  ASSISTANT_PAGE_DRAFT_HINT_MAX_RUNES,
  ASSISTANT_PAGE_DRAFT_LABEL_MAX_RUNES,
} from "~/contract/assistant.ts";
import type { AccessSeat, GitHubAccess } from "~/contract/access.ts";
import {
  DraftRefusal,
  listedWithin,
  listing,
  refuse,
  said,
  useAssistantDraft,
  type AssistantDraftSpec,
  type DraftFieldSpec,
  type DraftValue,
} from "~/app/assistant/draft.ts";
import { handleLabel } from "~/lib/seats.ts";
import { andList, orList } from "~/lib/words.ts";
import type { AccessDraft } from "./api.ts";
import {
  emptyLevel,
  joinLevels,
  levelIds,
  managedOutside,
  menuName,
  productOnly,
  sameDesired,
  scopeApplies,
  scopeChoices,
  scopeLabel,
  scopeUnavailable,
  wantOf,
  withEnabled,
  withLevel,
  withPermission,
  withScope,
  type Lane,
} from "./logic.ts";
import { GITHUB_ACCESS_NAMES, laneWords } from "./words.ts";

/** One field of an integration's access draft. */
type Field = DraftFieldSpec<AccessDraft>;

/** An Agents tab, as the draft it declares is of. */
export interface AccessDraftView {
  /** The integration's key, which the access routes are keyed on. */
  kind: string;
  /** The integration in the reader's words. */
  name: string;
  /** A sync is running, and every control waits on it. */
  syncing: boolean;
  lanes: readonly Lane[];
  /** The agents the tab draws, in its order: every agent, or the one an agent's own page draws. */
  seats: readonly AccessSeat[];
}

/** One agent's row on one product: its fields, and whether it holds a change not saved yet. */
interface Row {
  changed: boolean;
  fields: readonly Field[];
}

/** One tab's draft, as the page gathering several reads it. */
export interface AccessDraftPart {
  /** The integration's key: one part each on a page. */
  kind: string;
  /** The integration in the reader's words. */
  name: string;
  /** Its rows, in the tab's order. */
  rows: readonly Row[];
  /** The tab's draft now. */
  value: AccessDraft;
  /** The tab's own setter. */
  apply(next: AccessDraft): void;
  /** Why nothing on the tab may change now, or null. */
  blocked(): string | null;
}

const ON = "On";
const OFF = "Off";

/** The number of characters (code points) in a string, as the engine counts runes. */
function runes(text: string): number {
  return [...text].length;
}

/** A key as a path segment: lowercase, and nothing the path grammar refuses. */
function segment(key: string): string {
  return key.toLowerCase().replace(/[^a-z0-9_]/g, "_");
}

/** A text field's proposed value, trimmed. */
function typedText(value: DraftValue): string {
  return (typeof value === "string" ? value : value.join(" ")).trim();
}

/** A list field's proposed items, each trimmed, the blank ones left out. */
function typedItems(value: DraftValue): string[] {
  return (typeof value === "string" ? [value] : [...value])
    .map((item) => item.trim())
    .filter((item) => item !== "");
}

/**
 * A label naming an agent as its row does, its name then its handle, around a
 * control's own words (`words` places the name). Within the engine's bound the
 * name gives way, marked, and never the handle: the handle is what tells two
 * agents of one name apart, and what moves the draft, so that a change
 * prepared for one agent goes stale, once another agent holds its place.
 */
function named(seat: AccessSeat, words: (name: string) => string): string {
  const handle = ` (${handleLabel(seat.handle)})`;
  const room = ASSISTANT_PAGE_DRAFT_LABEL_MAX_RUNES - runes(words("")) - runes(handle);
  const name =
    runes(seat.name) <= room
      ? seat.name
      : `${[...seat.name].slice(0, Math.max(room - 1, 1)).join("")}…`;
  return `${words(name)}${handle}`;
}

/** A hint of `head`, then the names that fit beside it and `tail`, within a hint's bound. */
function hinted(head: string, names: readonly string[], tail = ""): string {
  const room = ASSISTANT_PAGE_DRAFT_HINT_MAX_RUNES - runes(head) - runes(tail);
  return `${head}${listedWithin(names, room, runes)}${tail}`;
}

/** The one choice `typed` names, by any name it goes by: as written, else in any case. */
function pick<T>(
  choices: readonly { names: readonly string[]; value: T }[],
  typed: string,
): { value: T } | "none" | "several" {
  const ways = [
    (name: string) => name === typed,
    (name: string) => name.toLowerCase() === typed.toLowerCase(),
  ];
  for (const same of ways) {
    const found = choices.filter((choice) => choice.names.some(same));
    if (found.length === 1) return { value: found[0]!.value };
    if (found.length > 1) return "several";
  }
  return "none";
}

/** Why an agent that is off takes no `what` yet: its control is drawn once it is on. */
function offWords(seat: AccessSeat, lane: Lane, what: string): string {
  return said`${seat.name} is off on ${lane.name}, and its ${what} is chosen once it is on. Turn it on in the same change.`;
}

/** A level's value as its picker shows it: its menu's name, or the stored id of one the vendor no longer lists. */
function levelValue(lane: Lane, id: string): string {
  const level = lane.levels.find((l) => l.id === id);
  return level ? menuName(level) : id;
}

/** The levels a lane of sets holds, by id and as its menu names each: the vendor's order, then any it no longer lists. */
function heldLevels(lane: Lane, level: string | undefined): [id: string, name: string][] {
  const ids = levelIds(level);
  return [
    ...lane.levels
      .filter((l) => ids.includes(l.id))
      .map((l): [string, string] => [l.id, menuName(l)]),
    ...ids
      .filter((id) => !lane.levels.some((l) => l.id === id))
      .map((id): [string, string] => [id, id]),
  ];
}

/** The switch: On or Off, as the row's switch is. */
function switchField(seat: AccessSeat, lane: Lane, path: string): Field {
  return {
    path: `${path}.enabled`,
    label: named(seat, (name) => `${name} on ${lane.name}`),
    hint: `${ON} or ${OFF}`,
    read: (d) => (wantOf(d, seat, lane.key).enabled ? ON : OFF),
    write: (d, value) => {
      const on = pick(
        [
          { names: [ON], value: true },
          { names: [OFF], value: false },
        ],
        typedText(value),
      );
      return typeof on === "object"
        ? withEnabled(d, seat, lane, on.value)
        : refuse(said`${seat.name} on ${lane.name} is ${ON} or ${OFF}.`);
    },
  };
}

/** The level picker of a lane that holds one level: one of the lane's own. */
function levelField(seat: AccessSeat, lane: Lane, path: string): Field {
  const offered = lane.levels.filter((l) => !l.unavailable).map(menuName);
  return {
    path: `${path}.level`,
    label: named(seat, (name) => `${name}'s ${lane.name} access`),
    hint: hinted("One of: ", offered),
    read: (d) => levelValue(lane, wantOf(d, seat, lane.key).level ?? ""),
    // NONE CHOSEN reads as the tab's word for it, never as an empty row.
    summary: (value) => (typeof value === "string" && value !== "" ? value : "None"),
    write: (d, value) => {
      const want = wantOf(d, seat, lane.key);
      if (!want.enabled) return refuse(offWords(seat, lane, "access"));
      const typed = typedText(value);
      // WHAT IT HOLDS NOW is kept as it reads, a level the vendor dropped included.
      if (typed === levelValue(lane, want.level ?? "")) return d;
      const found = pick(
        lane.levels.map((l) => ({ names: [menuName(l)], value: l })),
        typed,
      );
      if (found === "none") {
        return refuse(
          said`${lane.name} offers ${seat.name} no level called “${typed}”. It offers ${listing(offered, orList)}.`,
        );
      }
      if (found === "several") {
        return refuse(
          said`More than one ${lane.name} level is called “${typed}”: choose it on ${seat.name}'s row.`,
        );
      }
      // A LEVEL OFFERED DISABLED is refused in the sentence the menu shows under it.
      if (found.value.unavailable) return refuse(said`${found.value.unavailable}`);
      return withLevel(d, seat, lane, found.value.id);
    },
  };
}

/** The level picker of a lane of sets: any of the lane's own, the level the others need joined to them. */
function levelsField(seat: AccessSeat, lane: Lane, path: string): Field {
  // THE PICKER HAS NO EMPTY ROW: choosing none is the empty level.
  const options = lane.levels.filter((l) => l.id !== "");
  const offered = options.filter((l) => !l.unavailable).map(menuName);
  const none = emptyLevel(lane)?.name ?? "No permissions";
  const required = lane.required_level ? levelValue(lane, lane.required_level) : undefined;
  const tail = `. None is ${none}${required ? `; ${required} comes with any other` : ""}.`;
  return {
    path: `${path}.levels`,
    label: named(seat, (name) => `${name}'s ${lane.name} permissions`),
    hint: hinted("Any of: ", offered, tail),
    read: (d) => heldLevels(lane, wantOf(d, seat, lane.key).level).map(([, name]) => name),
    summary: (value) => {
      const items = typeof value === "string" ? [value] : value;
      return items.length > 0 ? items.join(", ") : none;
    },
    write: (d, value) => {
      const want = wantOf(d, seat, lane.key);
      if (!want.enabled) return refuse(offWords(seat, lane, "permissions"));
      const holds = heldLevels(lane, want.level);
      const ids: string[] = [];
      for (const typed of typedItems(value)) {
        // WHAT IT HOLDS NOW is kept as it reads, a level the vendor dropped included.
        const kept = holds.find(([, name]) => name === typed);
        if (kept) {
          ids.push(kept[0]);
          continue;
        }
        const found = pick(
          options.map((l) => ({ names: [menuName(l)], value: l })),
          typed,
        );
        if (found === "none") {
          return refuse(
            said`${lane.name} offers ${seat.name} no permission called “${typed}”. It offers ${listing(offered, orList)}.`,
          );
        }
        if (found === "several") {
          return refuse(
            said`More than one ${lane.name} permission is called “${typed}”: choose it on ${seat.name}'s row.`,
          );
        }
        if (found.value.unavailable && !holds.some(([id]) => id === found.value.id)) {
          return refuse(said`${found.value.unavailable}`);
        }
        ids.push(found.value.id);
      }
      return withLevel(d, seat, lane, joinLevels(lane, ids));
    },
  };
}

/** One GitHub permission, as the permissions dialog sets it: No access, Read-only or Read and write. */
function permissionField(
  seat: AccessSeat,
  lane: Lane,
  path: string,
  perm: NonNullable<Lane["permissions"]>[number],
): Field {
  // WRITE ONLY IS GITHUB'S RULE (Workflows): the dialog offers no Read-only.
  const allowed: readonly ("none" | GitHubAccess)[] = perm.write_only
    ? ["none", "write"]
    : ["none", "read", "write"];
  const words = orList(allowed.map((access) => GITHUB_ACCESS_NAMES[access]));
  return {
    path: `${path}.permission.${segment(perm.key)}`,
    label: named(seat, (name) => `${name}'s ${perm.name} permission`),
    hint: words,
    read: (d) => GITHUB_ACCESS_NAMES[wantOf(d, seat, lane.key).permissions?.[perm.key] ?? "none"],
    write: (d, value) => {
      if (!wantOf(d, seat, lane.key).enabled) {
        return refuse(offWords(seat, lane, `${perm.name} permission`));
      }
      const found = pick(
        allowed.map((access) => ({ names: [GITHUB_ACCESS_NAMES[access]], value: access })),
        typedText(value),
      );
      return typeof found === "object"
        ? withPermission(d, seat, lane, perm.key, found.value)
        : refuse(said`${seat.name}'s ${perm.name} permission is ${words}.`);
    },
  };
}

/** The scope dialog: entries the connection reaches, or typed where the lane takes them; null where the row draws none. */
function scopeField(seat: AccessSeat, lane: Lane, path: string): Field | null {
  const words = laneWords(lane.key).scope;
  if (!lane.scoped || !words || managedOutside(lane)) return null;
  const noun = words.noun.toLowerCase();
  const reached = (lane.scope ?? []).map((id) => scopeLabel(lane, id));
  const tail =
    (lane.scope_open ? ", or any other typed" : "") +
    (lane.scope_required ? ". At least one while it is on." : `. None is ${words.empty}.`);
  return {
    path: `${path}.scope`,
    label: named(seat, (name) => `${name}'s ${lane.name} ${noun}`),
    hint:
      reached.length > 0
        ? hinted(`${words.noun} by name, any of: `, reached, tail)
        : `${words.noun} by name${tail}`,
    read: (d) => (wantOf(d, seat, lane.key).scope ?? []).map((id) => scopeLabel(lane, id)),
    // AN EMPTY SCOPE READS AS THE LANE'S WORDS FOR IT ("All installed
    // repositories"), as the row and the review say it.
    summary: (value) => {
      const items = typeof value === "string" ? [value] : value;
      return items.length > 0 ? andList(items) : words.empty;
    },
    write: (d, value) => {
      const want = wantOf(d, seat, lane.key);
      if (!want.enabled) return refuse(offWords(seat, lane, noun));
      if (!scopeApplies(lane, want)) {
        return refuse(
          said`${productOnly(lane, want.level) ?? ""} on ${lane.name} takes no ${noun}. Choose ${seat.name}'s access first, in the same change.`,
        );
      }
      const unavailable = scopeUnavailable(lane, want.scope);
      if (unavailable) return refuse(said`${unavailable.words}.`);
      // WHAT THE DIALOG OFFERS: what the connection reaches, and what the
      // agent holds that it no longer does, so that can be kept or cleared.
      const choices = scopeChoices(lane, want.scope);
      const scope: string[] = [];
      for (const typed of typedItems(value)) {
        const found = pick(
          choices.map((c) => ({ names: [c.label, c.value], value: c.value })),
          typed,
        );
        if (found === "several") {
          return refuse(
            said`More than one ${words.singular} is called “${typed}”: choose it in ${seat.name}'s ${noun} on the tab.`,
          );
        }
        const id = typeof found === "object" ? found.value : lane.scope_open ? typed : null;
        if (id === null) {
          return refuse(
            said`${lane.name}'s connection reaches no ${words.singular} called “${typed}”. It reaches: ${listing(choices.map((c) => c.label))}.`,
          );
        }
        if (!scope.includes(id)) scope.push(id);
      }
      return withScope(d, seat, lane, scope);
    },
  };
}

/** One agent's row on one product: its switch first, then what its access cell and scope cell draw. */
function rowFields(seat: AccessSeat, at: number, lane: Lane): Field[] {
  const path = `agent_${at}.${segment(lane.key)}`;
  const fields = [switchField(seat, lane, path)];
  if (managedOutside(lane)) return fields;
  if (lane.levels.length > 0) {
    fields.push(lane.multiple ? levelsField(seat, lane, path) : levelField(seat, lane, path));
  } else {
    for (const perm of lane.permissions ?? []) fields.push(permissionField(seat, lane, path, perm));
  }
  const scope = scopeField(seat, lane, path);
  if (scope) fields.push(scope);
  return fields;
}

/**
 * One tab's draft as a part: a row for each agent on each product it draws,
 * the agents numbered by their place on the tab, and its setter.
 */
export function accessDraftPart(
  view: AccessDraftView,
  draft: AccessDraft,
  apply: (next: AccessDraft) => void,
): AccessDraftPart {
  const rows = view.seats.flatMap((seat, i) =>
    // AN AGENT THAT LEFT has no control, so no field; its place is still its.
    seat.departed
      ? []
      : view.lanes
          .filter((lane) => seat.lanes[lane.key])
          .map((lane) => ({
            changed: !sameDesired(
              lane,
              wantOf(draft, seat, lane.key),
              seat.lanes[lane.key]!.desired,
            ),
            fields: rowFields(seat, i + 1, lane),
          })),
  );
  return {
    kind: view.kind,
    name: view.name,
    rows,
    value: draft,
    apply,
    blocked: () =>
      view.syncing
        ? `A sync with ${view.name} is running. Access can be changed here once it ends.`
        : null,
  };
}

/** Every row's fields, the rows holding a change first, each set in its own order. */
function ordered<T>(rows: readonly { changed: boolean; fields: readonly T[] }[]): T[] {
  return [...rows.filter((row) => row.changed), ...rows.filter((row) => !row.changed)].flatMap(
    (row) => row.fields,
  );
}

/** What Ask Crewlet is told it can do here, `whose` naming the access the draft is of. */
function helpWords(whose: string, names: string): string {
  return (
    `${whose} access on ${names} as this page holds it, the rows with a change not saved yet first. ` +
    "A path names an agent by its place on the page, and its label by name and handle. You can " +
    "propose any field from what its control offers: a level from its list, a permission from its " +
    "values, scope entries the connection reaches, or typed where it takes them. An agent's access " +
    "waits on its switch, so turn it on in the same change. Nothing changes until the person " +
    "reviews and saves it."
  );
}

/** An integration's Agents tab, declared as one draft of the company document at `revision`. */
export function accessDraft(
  part: AccessDraftPart,
  revision: string,
): AssistantDraftSpec<AccessDraft> {
  return {
    kind: "client",
    id: part.kind,
    revision,
    help: helpWords("Every agent's", part.name),
    scope: `Agents' access on ${part.name} (not saved)`,
    value: part.value,
    fields: ordered(part.rows),
    apply: (next) => part.apply(next),
    blocked: () => part.blocked(),
  };
}

/** What several tabs' drafts are, as one: each tab's draft by its integration's key. */
export type AccessDrafts = Readonly<Record<string, AccessDraft>>;

/** A tab's field over the drafts of every tab on the page, reading and setting its own. */
function lifted(kind: string, field: Field): DraftFieldSpec<AccessDrafts> {
  const write = field.write;
  return {
    path: field.path,
    label: field.label,
    ...(field.hint === undefined ? {} : { hint: field.hint }),
    ...(field.summary === undefined ? {} : { summary: field.summary }),
    read: (drafts) => field.read(drafts[kind] ?? {}),
    ...(write === undefined
      ? {}
      : {
          write: (drafts: AccessDrafts, value: DraftValue) => {
            const next = write(drafts[kind] ?? {}, value);
            return next instanceof DraftRefusal ? next : { ...drafts, [kind]: next };
          },
        }),
  };
}

/**
 * One agent's Access page, declared as one draft over every tab it draws: the
 * rows holding a change first across every integration, each field read and
 * set through its own tab, and a change applied to each tab it changes. Null
 * where no tab is open to change.
 *
 * THE PATHS ARE ALREADY DISTINCT: one agent, so one place, and a product's key
 * is one integration's alone. Every tab is held while any is (a sync running
 * on one), since one change may touch several.
 */
export function accessDraftsOf(
  parts: readonly AccessDraftPart[],
  agent: string,
): AssistantDraftSpec<AccessDrafts> | null {
  if (parts.length === 0) return null;
  const rows = parts.flatMap((part) =>
    part.rows.map((row) => ({
      changed: row.changed,
      fields: row.fields.map((field) => lifted(part.kind, field)),
    })),
  );
  return {
    kind: "client",
    help: helpWords(`${agent}'s`, andList(parts.map((part) => part.name))),
    scope: `${agent}'s access (not saved)`,
    value: Object.fromEntries(parts.map((part) => [part.kind, part.value])),
    fields: ordered(rows),
    apply: (next) => {
      for (const part of parts) {
        const drafted = next[part.kind];
        if (drafted !== undefined && drafted !== part.value) part.apply(drafted);
      }
    },
    blocked: () => parts.map((part) => part.blocked()).find((why) => why !== null) ?? null,
  };
}

/**
 * The tabs open on one agent's Access page: each tab puts its part in as it
 * renders, and the page declares them as one draft ([accessDraftsOf]).
 *
 * PUBLISHED ONLY WHEN WHAT A DRAFT WOULD SAY MOVED (each field's path, label,
 * hint and value, and why the tab is held), so a tab's render that changed
 * nothing renders nothing again; a part's setter and its gate are read
 * through the latest render's part.
 */
export class AccessDraftParts {
  private readonly open = new Map<string, { part: AccessDraftPart; said: string }>();
  private readonly listeners = new Set<() => void>();
  private current: readonly AccessDraftPart[] = [];

  /** A tab's part as it renders. */
  put(part: AccessDraftPart): void {
    const said = JSON.stringify([
      part.name,
      part.blocked(),
      part.rows.map((row) => [
        row.changed,
        row.fields.map((f) => [f.path, f.label, f.hint ?? "", f.read(part.value)]),
      ]),
    ]);
    const before = this.open.get(part.kind);
    this.open.set(part.kind, { part, said });
    if (before?.said !== said) this.publish();
  }

  /** A tab that closed, or that is held for this reader now. */
  drop(kind: string): void {
    if (this.open.delete(kind)) this.publish();
  }

  /** The parts open now: the same array until one opens, closes or says something else. */
  readonly read = (): readonly AccessDraftPart[] => this.current;

  readonly subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  private publish(): void {
    this.current = [...this.open.keys()].map((kind) => this.live(kind));
    for (const listener of [...this.listeners]) listener();
  }

  /** A part whose setter and gate are the latest render's. */
  private live(kind: string): AccessDraftPart {
    const part = this.open.get(kind)!.part;
    return {
      ...part,
      apply: (next) => this.open.get(kind)?.part.apply(next),
      blocked: () => this.open.get(kind)?.part.blocked() ?? null,
    };
  }
}

/** The agent's Access page's parts, which every tab on it puts its own into. */
export const AccessDraftPartsContext = createContext<AccessDraftParts | null>(null);

/**
 * Put a tab's part into the page's while it is open to change; null takes it
 * out. Outside a page that gathers its tabs it does nothing, and the tab
 * declares its own draft.
 */
export function useAccessDraftPart(
  parts: AccessDraftParts | null,
  part: AccessDraftPart | null,
): void {
  const kind = part?.kind ?? null;
  // EVERY RENDER, so the page reads the tab as it is now; the page declares
  // again only when what its draft says moved.
  useLayoutEffect(() => {
    if (parts && part) parts.put(part);
  });
  useLayoutEffect(() => {
    if (!parts || kind === null) return;
    return () => parts.drop(kind);
  }, [parts, kind]);
}

/** The parts open on a page now, read again whenever one opens, closes or moves. */
export function useAccessDraftParts(parts: AccessDraftParts): readonly AccessDraftPart[] {
  return useSyncExternalStore(parts.subscribe, parts.read, parts.read);
}

/**
 * Declare the one draft over every tab gathered on `agent`'s Access page,
 * from a small child of the page, so a part that moved renders it alone.
 */
export function useGatheredAccessDraft(parts: AccessDraftParts, agent: string): void {
  useAssistantDraft(accessDraftsOf(useAccessDraftParts(parts), agent));
}
