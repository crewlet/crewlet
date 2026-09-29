/**
 * What is wrong with the draft, placed on the nodes it is about.
 *
 * THREE SOURCES, ONE INDEX. The chart has no dry run: a batch and a content
 * write are decided when they are sent, and nothing validates a draft of the
 * chart without writing it. So the problems a check shows come from three
 * places, and each is placed where the reader looks for it:
 *
 * - THE DRAFT'S OWN SHAPE ([preflight]). The rules a chart write refuses on
 *   before it reads a single row — an address's grammar, a reserved word, a
 *   field past its cap — and the one rule of the organization a seat's kind
 *   brings (a person's seat carries no model chain). Each is the engine's
 *   rule restated at the boundary it applies at, and none is a guess about a
 *   row: whether an address is TAKEN by a removed or renamed object is the
 *   chart's to say, at the save. A reference naming nothing is a WARNING,
 *   because the chart keeps it as written and the organization reports it
 *   rather than refusing it.
 * - THE SETTINGS' DRY RUN ([placeSettingsFindings]). The settings document is
 *   still a revision the engine validates whole, and its problems name paths
 *   in it: the charter's are the company node's, an integration's go to the
 *   Integrations screen, a schedule's to the Schedules screen.
 * - A SAVE'S REFUSAL (`save.ts`), placed on the node the refused write was
 *   about, beside the step it stopped at.
 */

import type { ConfigProblem, ConfigWarning } from "~/protocol/index.ts";
import { COMPANY_KEY, handleOfKey, unitKeyOf, type NodeKey } from "./keys.ts";
import { CHARTER_FIELDS, MAX_ADDRESS, type Segment } from "./document.ts";
import {
  addressIndex,
  allSeats,
  allUnits,
  type Draft,
  type SeatData,
  type UnitData,
} from "./draft.ts";
import { AGENT_FORBIDDEN, HUMAN_FORBIDDEN, fieldName, kindOf } from "./operations.ts";
import { getPath } from "./json.ts";

/** Where a problem sends the operator when the builder cannot fix it. */
export type ProblemLink = "integrations" | "schedules";

/** One problem or warning, placed. */
export interface PlacedProblem {
  readonly severity: "problem" | "warning";
  /** The engine's kind: a problem kind, or a warning kind. */
  readonly kind: string;
  readonly message: string;
  /** The node it is about; `null` at document level. */
  readonly node: NodeKey | null;
  /** The segments below the node: `["goal"]`, `["runtime", "schedules", 0, "cron"]`. Empty for the node itself. */
  readonly field: readonly Segment[];
  readonly link: ProblemLink | null;
  /** The record in the engine's own shape, for a caller that needs `ref`, `from` or `to`. */
  readonly source: ConfigProblem | ConfigWarning;
}

/** Every problem and warning of one check, placed. */
export interface ProblemIndex {
  /** Per node, in the order they were found. */
  readonly byNode: ReadonlyMap<NodeKey, readonly PlacedProblem[]>;
  /** What names no node, in the order found. */
  readonly document: readonly PlacedProblem[];
  readonly problemCount: number;
  readonly warningCount: number;
}

export const EMPTY_PROBLEMS: ProblemIndex = {
  byNode: new Map(),
  document: [],
  problemCount: 0,
  warningCount: 0,
};

/** Indexes placed problems by node, keeping their order. */
export function indexProblems(placed: readonly PlacedProblem[]): ProblemIndex {
  const byNode = new Map<NodeKey, PlacedProblem[]>();
  const document: PlacedProblem[] = [];
  let problemCount = 0;
  let warningCount = 0;
  for (const p of placed) {
    if (p.severity === "problem") problemCount++;
    else warningCount++;
    if (p.node === null) document.push(p);
    else {
      const list = byNode.get(p.node);
      if (list) list.push(p);
      else byNode.set(p.node, [p]);
    }
  }
  return { byNode, document, problemCount, warningCount };
}

// ---------------------------------------------------------------------------
// The draft's own shape
// ---------------------------------------------------------------------------

/** The chart's caps (`internal/chart/chart.go`), in UTF-8 bytes where the engine counts bytes. */
export const CHART_LIMITS = {
  /** A seat's or a unit's display name. */
  name: 256,
  /** A backstory, a goal, a purpose, and each entry of a prose list. */
  prose: 16 * 1024,
  /** Entries in a prose list (responsibilities, guidelines, goals, knowledge references). */
  list: 32,
  /** Entries in a `manages:` list. */
  manages: 64,
  /** A seat's email: the longest address RFC 5321 permits. */
  email: 320,
} as const;

/**
 * Words the chart reserves as an address (`chart.ReservedKeys`): the org
 * root and the log's own subject kinds for both, and — for a seat — the word
 * `integrations.datadog.route_to` uses for nobody.
 */
const RESERVED_UNIT_KEYS: readonly string[] = ["root", "tree", "barrier"];
const RESERVED_HANDLES: readonly string[] = [...RESERVED_UNIT_KEYS, "none"];

/** A seat handle's grammar (`iam.ValidSeatHandle`). */
const HANDLE = /^[a-z0-9][a-z0-9-]*$/;

/** The number of bytes a string takes in UTF-8, as the engine measures its caps. */
export function utf8Length(value: string): number {
  let bytes = 0;
  for (const char of value) {
    const code = char.codePointAt(0)!;
    bytes += code < 0x80 ? 1 : code < 0x800 ? 2 : code < 0x10000 ? 3 : 4;
  }
  return bytes;
}

/** A unit key as the chart folds it (`chart.NormalizeKey`): lowercase, whitespace runs as one hyphen. */
export function foldKey(key: string): string {
  return key.trim().split(/\s+/).filter(Boolean).join("-").toLowerCase();
}

/** Why a handle cannot address a seat, or `null`. */
export function handleProblem(handle: unknown): string | null {
  if (typeof handle !== "string" || handle === "") {
    return "A seat needs a handle: it is the address the chart and every reference name it by.";
  }
  if (RESERVED_HANDLES.includes(handle)) {
    return `The handle ${handle} is reserved: the chart uses it for something of its own.`;
  }
  if (utf8Length(handle) > MAX_ADDRESS) {
    return `The handle is ${utf8Length(handle)} bytes long, and a handle is at most ${MAX_ADDRESS}.`;
  }
  if (!HANDLE.test(handle)) {
    return "A handle is lowercase letters, digits and hyphens, starting with a letter or a digit.";
  }
  return null;
}

/** Why a key cannot address a unit, or `null`. */
export function unitKeyProblem(key: unknown): string | null {
  if (typeof key !== "string" || key === "") {
    return "A unit needs a key: it is the address the chart and every reference name it by.";
  }
  if (RESERVED_UNIT_KEYS.includes(key)) {
    return `The key ${key} is reserved: the chart uses it for something of its own.`;
  }
  if (utf8Length(key) > MAX_ADDRESS) {
    return `The key is ${utf8Length(key)} bytes long, and a key is at most ${MAX_ADDRESS}.`;
  }
  if (/[/\s*>]/.test(key)) {
    return "A key carries no whitespace, slash, `*` or `>`: it is a path segment and a subject token.";
  }
  if (key !== foldKey(key)) {
    return `A key is written in its folded form, ${foldKey(key)}: keys are compared folded, so another spelling would read as a second unit.`;
  }
  return null;
}

/**
 * What a draft's own shape says is wrong with it, and — given the chart it
 * was made on — what its history does ([reservedAddresses]).
 */
export function preflight(draft: Draft, base: Draft | null = null): PlacedProblem[] {
  const out: PlacedProblem[] = [];
  const problem = (node: NodeKey, field: Segment[], kind: string, message: string) =>
    out.push({
      severity: "problem",
      kind,
      message,
      node,
      field,
      link: null,
      source: { path: fieldName(field.map(String)), segments: field, kind, message },
    });
  const prose = (node: NodeKey, data: SeatData | UnitData, field: string, label: string) => {
    const value = data[field];
    if (typeof value === "string" && utf8Length(value) > CHART_LIMITS.prose) {
      problem(
        node,
        [field],
        "out_of_range",
        `The ${label} is ${utf8Length(value)} bytes, and the chart takes at most ${CHART_LIMITS.prose}.`,
      );
    }
  };
  const list = (node: NodeKey, data: SeatData | UnitData, field: string, label: string) => {
    const value = data[field];
    if (!Array.isArray(value)) return;
    if (value.length > CHART_LIMITS.list) {
      problem(
        node,
        [field],
        "out_of_range",
        `${label} has ${value.length} entries, and the chart takes at most ${CHART_LIMITS.list}: each is rendered into a prompt on every turn.`,
      );
    }
    value.forEach((entry, i) => {
      if (typeof entry === "string" && utf8Length(entry) > CHART_LIMITS.prose) {
        problem(
          node,
          [field, i],
          "out_of_range",
          `An entry of ${label.toLowerCase()} is ${utf8Length(entry)} bytes, and the chart takes at most ${CHART_LIMITS.prose}.`,
        );
      }
    });
  };
  const name = (node: NodeKey, data: SeatData | UnitData) => {
    if (utf8Length(data.name) > CHART_LIMITS.name) {
      problem(
        node,
        ["name"],
        "out_of_range",
        `The name is ${utf8Length(data.name)} bytes, and the chart takes at most ${CHART_LIMITS.name}.`,
      );
    }
  };

  const handles = new Map<string, NodeKey[]>();
  const keys = new Map<string, NodeKey[]>();
  for (const { seat } of allSeats(draft)) {
    handles.set(seat.data.handle, [...(handles.get(seat.data.handle) ?? []), seat.key]);
  }
  for (const { unit } of allUnits(draft)) {
    keys.set(unit.data.key, [...(keys.get(unit.data.key) ?? []), unit.key]);
  }

  for (const { seat } of allSeats(draft)) {
    const { key, data } = seat;
    const handle = handleProblem(data.handle);
    if (handle) problem(key, ["handle"], "invalid", handle);
    else if ((handles.get(data.handle) ?? []).length > 1) {
      problem(
        key,
        ["handle"],
        "conflict",
        `Another seat in this draft has the handle ${data.handle}.`,
      );
    }
    if (data.kind !== undefined && data.kind !== "human" && data.kind !== "agent") {
      problem(
        key,
        ["kind"],
        "unknown_value",
        `${String(data.kind)} is not a seat kind this engine runs: a seat is an agent's or a person's.`,
      );
    }
    name(key, data);
    if (typeof data.email === "string" && utf8Length(data.email) > CHART_LIMITS.email) {
      problem(
        key,
        ["email"],
        "out_of_range",
        `The email is longer than the ${CHART_LIMITS.email} bytes an address may be.`,
      );
    }
    prose(key, data, "backstory", "backstory");
    prose(key, data, "goal", "goal");
    list(key, data, "responsibilities", "Responsibilities");
    list(key, data, "behavioral_guidelines", "Behavioral guidelines");
    const manages = Array.isArray(data.manages) ? data.manages : [];
    if (manages.length > CHART_LIMITS.manages) {
      problem(
        key,
        ["manages"],
        "out_of_range",
        `This seat manages ${manages.length} entries, and the chart takes at most ${CHART_LIMITS.manages}.`,
      );
    }
    // THE ONE RULE A SEAT'S KIND BRINGS: what an agent runs on and a person
    // never does (`org.Role.Validate`). The chart keeps either, and the
    // organization refuses the seat, so it is said here before the save.
    const kind = kindOf(data);
    for (const rule of kind === "human" ? HUMAN_FORBIDDEN : AGENT_FORBIDDEN) {
      if (getPath(data, rule.path) === undefined) continue;
      problem(
        key,
        [...rule.path],
        "conflict",
        kind === "human"
          ? `A person's seat does not carry ${fieldName(rule.path)}: it is what an agent seat runs on. Change the kind, or remove it.`
          : `An agent seat does not carry ${fieldName(rule.path)}: it is how a person's seat is reached. Change the kind, or remove it.`,
      );
    }
  }

  if (base) {
    for (const found of reservedAddresses(draft, base)) {
      problem(found.node, [found.kind === "seat" ? "handle" : "key"], "conflict", found.message);
    }
  }

  for (const { unit } of allUnits(draft)) {
    const { key, data } = unit;
    const address = unitKeyProblem(data.key);
    if (address) problem(key, ["key"], "invalid", address);
    else if ((keys.get(data.key) ?? []).length > 1) {
      problem(key, ["key"], "conflict", `Another unit in this draft has the key ${data.key}.`);
    }
    name(key, data);
    prose(key, data, "purpose", "purpose");
    list(key, data, "goals", "Goals");
    list(key, data, "knowledge_refs", "Knowledge references");
  }
  out.push(...referenceWarnings(draft));
  return out;
}

/** An address a node of the draft may not take, and why. */
interface Reserved {
  readonly node: NodeKey;
  readonly kind: "seat" | "unit";
  readonly message: string;
}

/**
 * The addresses the chart will refuse a node of the draft, given the chart it
 * was made on. Three rules, each the chart's own (`chart.refuseCreate`):
 *
 * - A REMOVED node's address and its identity are never issued again — its
 *   history, its references and the tombstone that stops its old records
 *   applying are keyed on them — so neither a creation nor a rename may take
 *   one.
 * - The address a node was CREATED under is its identity, never issued twice
 *   however long ago it was renamed away from: a second object on it would
 *   share the first one's mailbox, memory and agent id. A node's own identity
 *   is no collision — renaming back claims what already resolves to it. The
 *   identity is what a saved node is KEYED by (`keys.ts`), so it is read off
 *   the key rather than guessed from the addresses.
 * - A RETIRED address — one a saved node used to answer to — still reaches
 *   that node, and the chart renames nothing else onto it. A creation may take
 *   one, which is how an alias is finally let go.
 *
 * Only a node whose address this draft chose is judged: one it created, or
 * one it gave a new address. A node still under its saved address is the
 * chart's as it is. What the draft cannot see — an object somebody removed
 * before it was read, whose tombstone the chart does not serve — is the
 * batch's to refuse at the save, naming the operation.
 */
export function reservedAddresses(draft: Draft, base: Draft): Reserved[] {
  const out: Reserved[] = [];
  const former = (value: unknown): string[] =>
    Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];
  for (const kind of ["seat", "unit"] as const) {
    const nodes = (d: Draft) =>
      kind === "seat"
        ? [...allSeats(d)].map(({ seat }) => ({
            key: seat.key,
            address: seat.data.handle,
            former: former(seat.data.former_handles),
          }))
        : [...allUnits(d)].map(({ unit }) => ({
            key: unit.key,
            address: unit.data.key,
            former: former(unit.data.former_keys),
          }));
    const identityOf = kind === "seat" ? handleOfKey : unitKeyOf;
    const now = new Map(nodes(draft).map((n) => [n.key, n]));
    const saved = new Map(nodes(base).map((n) => [n.key, n]));
    const word = kind === "seat" ? "handle" : "key";
    const at = kind === "seat" ? "@" : "";
    const noun = kind === "seat" ? "seat" : "unit";
    const removed = new Set<string>();
    /** An identity or a retired address, and the node of the draft it belongs to. */
    const identities = new Map<string, NodeKey>();
    const aliases = new Map<string, NodeKey>();
    for (const was of saved.values()) {
      const identity = identityOf(was.key) ?? was.address;
      if (!now.has(was.key)) {
        removed.add(was.address);
        removed.add(identity);
        continue;
      }
      identities.set(identity, was.key);
      for (const alias of was.former) aliases.set(alias, was.key);
    }
    for (const node of now.values()) {
      const was = saved.get(node.key);
      if (was && was.address === node.address) continue;
      const owner = identities.get(node.address);
      const alias = aliases.get(node.address);
      const addressOf = (key: NodeKey) => now.get(key)?.address ?? "";
      if (removed.has(node.address)) {
        out.push({
          node: node.key,
          kind,
          message: `The ${word} ${node.address} belongs to a ${noun} this draft removes, and the chart never gives a removed address to anything else. Choose another ${word}, or keep that ${noun}.`,
        });
      } else if (owner !== undefined && owner !== node.key) {
        out.push({
          node: node.key,
          kind,
          message: `The ${word} ${node.address} is the address ${at}${addressOf(owner)} was created under, which the chart keeps as that ${noun}'s identity and never gives to anything else. Choose another ${word}.`,
        });
      } else if (was !== undefined && alias !== undefined && alias !== node.key) {
        out.push({
          node: node.key,
          kind,
          message: `The ${word} ${node.address} still reaches ${at}${addressOf(alias)}, which used to answer to it, and the chart renames nothing onto an address that reaches somebody. Choose another ${word}.`,
        });
      }
    }
  }
  return out;
}

/**
 * Every reference of the draft that names nothing in it: a unit's lead that
 * resolves to no seat, a `manages:` entry that resolves to neither a seat nor
 * a unit — resolved as the chart resolves one, a retired address included
 * (`draft.addressIndex`).
 *
 * WARNINGS, NOT PROBLEMS: the chart keeps a reference as written and the
 * organization reports one that resolves to nothing rather than refusing it.
 * Its own function because the charts mark the same references on the nodes
 * that hold them, and the two must never disagree about which ones those are.
 */
export function referenceWarnings(draft: Draft): PlacedProblem[] {
  const out: PlacedProblem[] = [];
  const { seats: handles, units: keys } = addressIndex(draft);
  const dangling = (
    node: NodeKey,
    field: Segment[],
    ref: "lead" | "manages",
    from: string,
    to: string,
    message: string,
  ) =>
    out.push({
      severity: "warning",
      kind: "dangling_reference",
      message,
      node,
      field,
      link: null,
      source: {
        kind: "dangling_reference",
        ref,
        path: fieldName(field.map(String)),
        segments: field,
        seat: "",
        unit: "",
        from,
        to,
        message,
      },
    });
  for (const { seat } of allSeats(draft)) {
    const manages = Array.isArray(seat.data.manages) ? seat.data.manages : [];
    manages.forEach((entry, i) => {
      if (!handles.has(entry) && !keys.has(entry)) {
        dangling(
          seat.key,
          ["manages", i],
          "manages",
          seat.data.name || seat.data.handle,
          entry,
          `Manages ${entry}, which names no seat and no unit in this draft.`,
        );
      }
    });
  }
  for (const { unit } of allUnits(draft)) {
    const lead = unit.data.lead;
    if (typeof lead === "string" && lead !== "" && !handles.has(lead)) {
      dangling(
        unit.key,
        ["lead"],
        "lead",
        unit.data.name || unit.data.key,
        lead,
        `The lead ${lead} names no seat in this draft.`,
      );
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// The settings' dry run
// ---------------------------------------------------------------------------

/** What a settings dry run said. */
export interface Findings {
  readonly problems?: readonly ConfigProblem[] | null;
  readonly warnings?: readonly ConfigWarning[] | null;
}

/**
 * Places a settings dry run's problems and warnings: a charter field's on the
 * company node, everything else at document level, linked to the screen that
 * edits it where there is one.
 */
export function placeSettingsFindings(findings: Findings): PlacedProblem[] {
  const out: PlacedProblem[] = [];
  const place = (severity: PlacedProblem["severity"], source: ConfigProblem | ConfigWarning) => {
    const segments: readonly Segment[] = source.segments ?? [];
    const head = segments[0];
    const charter =
      typeof head === "string" && (CHARTER_FIELDS as readonly string[]).includes(head);
    out.push({
      severity,
      kind: source.kind,
      message: source.message,
      node: charter ? COMPANY_KEY : null,
      field: [...segments],
      link: head === "scheduling" ? "schedules" : head === "integrations" ? "integrations" : null,
      source,
    });
  };
  for (const problem of findings.problems ?? []) place("problem", problem);
  for (const warning of findings.warnings ?? []) place("warning", warning);
  return out;
}

/** How many problems (not warnings) a node carries, for its badge. */
export function problemCountOf(index: ProblemIndex, key: NodeKey): number {
  return (index.byNode.get(key) ?? []).filter((p) => p.severity === "problem").length;
}
