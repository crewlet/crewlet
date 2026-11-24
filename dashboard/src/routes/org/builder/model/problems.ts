/**
 * Placing the engine's problems and warnings on the builder's nodes.
 *
 * THE ENGINE LOCATES, THE BUILDER LOOKS UP. A refused dry run carries each
 * problem's `segments`: the authored path in the document the engine
 * validated, already split into keys and indexes. The builder never parses a
 * `path` string (a map key may hold a dot) and never guesses from a message.
 * It finds the longest prefix of the segments that names a node in the path
 * index of THE DOCUMENT THAT WAS SENT, and the rest of the segments is the
 * field inside that node. The index of the draft as it stands now would be
 * wrong: by the time an answer lands the operator may have moved, added or
 * removed a node, and `units[1]` may be a different unit.
 *
 * Only `roles`, `units` and the company's name map onto nodes, because those
 * are what the builder draws. Everything else stays at document level:
 * the principles (`mission`, `vision`, `policies`) with a link to Company ›
 * Principles, the one place they are written, a model entry or the sandbox
 * default (`providers.llm.*`, `providers.sandbox.*`) with a link to Settings ›
 * Models, where an entry's cell is chosen (as is a seat's model for its
 * review, delegated work, routines and judge, though that stays on its node), `integrations.*` with a link
 * to Company › Integrations (the Datadog
 * fallback and GitLab access levels are edited from seats, but a problem at
 * the company's integration block is fixed there), and any schedule problem,
 * mapped or not, with a link to the Schedules screen, because the builder
 * toggles schedules and authors none.
 *
 * A problem with no segments but a seat handle is placed on the seat holding
 * that handle in the document that was sent — through the derivation that came
 * with the same answer where it carries one, and otherwise by the seat's own
 * key, which is its handle. A seat the document no longer holds (one the write
 * removes) leaves the problem at document level, never matched by name.
 */

import type { ConfigProblem, ConfigWarning, Derived } from "~/protocol/index.ts";
import { warningWords } from "~/protocol/configAnswer.ts";
import { SEAT_JOB_FIELDS } from "~/contract/models.ts";
import { COMPANY_KEY, seatKey, unitKey, type NodeKey } from "./keys.ts";
import {
  COMPANY_FIELDS,
  PRINCIPLES_FIELDS,
  pathOfSegments,
  placeDerivation,
  type IndexedDocument,
  type Segment,
} from "./document.ts";

/** Where a problem sends the operator when the builder cannot fix it. */
export type ProblemLink = "integrations" | "schedules" | "principles" | "models";

/** One problem or warning, placed. */
export interface PlacedProblem {
  readonly severity: "problem" | "warning";
  /** The engine's kind: a problem kind, or a warning kind. */
  readonly kind: string;
  /**
   * What the builder says about it: a warning's words for the person who owns
   * the company where the engine wrote them (`person`, see [warningWords]),
   * and the engine's own sentence otherwise. The operator's sentence stays on
   * `source.message`.
   */
  readonly message: string;
  /** The node it is about; `null` at document level. */
  readonly node: NodeKey | null;
  /** The segments below the node: `["goal"]`, `["schedules", 0, "cron"]`. Empty for the node itself. */
  readonly field: readonly Segment[];
  readonly link: ProblemLink | null;
  /** The engine's own record, for a caller that needs `line`, `ref`, `from` or `to`. */
  readonly source: ConfigProblem | ConfigWarning;
}

/** Every problem and warning of one answer, placed. */
export interface ProblemIndex {
  /** Per node, in the engine's order. */
  readonly byNode: ReadonlyMap<NodeKey, readonly PlacedProblem[]>;
  /** What names no node, in the engine's order. */
  readonly document: readonly PlacedProblem[];
  /** Every one of them, wherever it was placed, in the engine's order. */
  readonly all: readonly PlacedProblem[];
  readonly problemCount: number;
  readonly warningCount: number;
}

export const EMPTY_PROBLEMS: ProblemIndex = {
  byNode: new Map(),
  document: [],
  all: [],
  problemCount: 0,
  warningCount: 0,
};

/** What an answer said, as the scheduler hands it over. */
export interface Findings {
  readonly problems?: readonly ConfigProblem[] | null;
  readonly warnings?: readonly ConfigWarning[] | null;
  /** The derivation the same answer carried, used to place a problem that names only a seat. */
  readonly derived?: Derived | null;
}

/** Places an answer's problems and warnings against the document that was sent. */
export function placeProblems(sent: IndexedDocument, findings: Findings): ProblemIndex {
  const byNode = new Map<NodeKey, PlacedProblem[]>();
  const document: PlacedProblem[] = [];
  const all: PlacedProblem[] = [];
  const { keyOfHandle } = placeDerivation(sent.index, findings.derived ?? null);
  const seatPath = (handle: string): readonly Segment[] =>
    sent.index.segmentsOf.get(keyOfHandle.get(handle) ?? seatKey(handle)) ?? [];
  // A REFUSED PART names a unit by its KEY (`configAnswer.refusedProblem`),
  // which is the unit's node key; a validation problem names one by its name
  // and always carries the path it is at.
  const unitPath = (id: string): readonly Segment[] => sent.index.segmentsOf.get(unitKey(id)) ?? [];

  const place = (severity: PlacedProblem["severity"], source: ConfigProblem | ConfigWarning) => {
    let segments: readonly Segment[] = source.segments ?? [];
    if (segments.length === 0 && source.seat) segments = seatPath(source.seat);
    if (segments.length === 0 && source.kind === "refused" && source.unit) {
      segments = unitPath(source.unit);
    }
    const { node, field } = locate(sent, segments);
    const placed: PlacedProblem = {
      severity,
      kind: source.kind,
      message: severity === "warning" ? warningWords(source as ConfigWarning) : source.message,
      node,
      field,
      link: linkFor(segments, node, field),
      source,
    };
    all.push(placed);
    if (node === null) document.push(placed);
    else {
      const list = byNode.get(node);
      if (list) list.push(placed);
      else byNode.set(node, [placed]);
    }
  };

  const problems = findings.problems ?? [];
  const warnings = findings.warnings ?? [];
  for (const problem of problems) place("problem", problem);
  for (const warning of warnings) place("warning", warning);
  return { byNode, document, all, problemCount: problems.length, warningCount: warnings.length };
}

/** The node a path names, and the field below it; document level when it names none. */
function locate(
  sent: IndexedDocument,
  segments: readonly Segment[],
): { node: NodeKey | null; field: Segment[] } {
  const head = segments[0];
  if (head === "roles" || head === "units") {
    for (let length = segments.length; length > 0; length--) {
      const key = sent.index.byPath.get(pathOfSegments(segments.slice(0, length)));
      if (key !== undefined && key !== COMPANY_KEY)
        return { node: key, field: segments.slice(length) };
    }
    return { node: null, field: [...segments] };
  }
  if (typeof head === "string" && (COMPANY_FIELDS as readonly string[]).includes(head)) {
    return { node: COMPANY_KEY, field: [...segments] };
  }
  return { node: null, field: [...segments] };
}

/**
 * Where a problem the builder cannot fix is fixed. A schedule is one of a
 * node's own `schedules`, or the company's `scheduling` block; matching
 * "schedules" anywhere in the segments would also match a tool server of
 * that name under `mcp_env`.
 */
function linkFor(
  segments: readonly Segment[],
  node: NodeKey | null,
  field: readonly Segment[],
): ProblemLink | null {
  if (node !== null && node !== COMPANY_KEY) {
    if (field[0] === "schedules") return "schedules";
    // A SEAT'S MODEL FOR A JOB BESIDE ITS OWN WORK (a review, delegated work,
    // its routines, the judge) is chosen on Settings › Models & Fleets, by what each
    // model is for; the builder edits the seat's own chain and only shows
    // these.
    return typeof field[0] === "string" && JOB_MODEL_FIELDS.includes(field[0]) ? "models" : null;
  }
  if (segments[0] === "scheduling") return "schedules";
  if (segments[0] === "integrations") return "integrations";
  // WHERE A MODEL ENTRY RUNS, and the default its entries take, are chosen
  // on Settings › Models & Fleets (each entry's "Where executors run"), which the
  // builder does not author.
  if (segments[0] === "providers" && (segments[1] === "llm" || segments[1] === "sandbox"))
    return "models";
  const head = segments[0];
  if (typeof head === "string" && (PRINCIPLES_FIELDS as readonly string[]).includes(head))
    return "principles";
  return null;
}

/** A seat's model fields for every job but its own work, which Settings › Models & Fleets chooses. */
const JOB_MODEL_FIELDS: readonly string[] = Object.values(SEAT_JOB_FIELDS).filter(
  (f) => f !== SEAT_JOB_FIELDS.executor,
);

/** How many problems (not warnings) a node carries, for its badge. */
export function problemCountOf(index: ProblemIndex, key: NodeKey): number {
  return (index.byNode.get(key) ?? []).filter((p) => p.severity === "problem").length;
}
