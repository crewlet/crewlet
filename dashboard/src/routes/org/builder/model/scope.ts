/**
 * A lead's draft: one unit they lead, edited as a document of its own.
 *
 * WHY A DOCUMENT OF ITS OWN. A lead without `config:read` may not read the
 * company document, but the engine admits them to the units they lead
 * (`lib/leadScope.ts`): `GET /config/units/{key}` reads one whole — its seats
 * and child units included, credentials masked — and `PUT
 * /config/units/{key}` writes it back, refused for whatever reaches outside
 * what they lead. So the builder edits `{name, units: [that unit]}` with every
 * rule it already has, and nothing in the model knows it is a lead's: the
 * draft has one top-level unit, and the request is the unit's
 * (`transport.ts`, `scope`).
 *
 * THE ENGINE ANSWERS ABOUT THE WHOLE COMPANY, though. A check's derivation,
 * a problem and a warning name WHOLE-DOCUMENT paths — the unit may sit at
 * `units[2].children[0]` — and the draft's paths start at `units[0]`. The
 * transport here moves every one into the draft's coordinates before the
 * model reads it ([inScope]), using where the derivation says the unit sits,
 * and leaves what lies outside the unit unplaced: a seat elsewhere in the
 * company is no node of this draft, and placed by its path it would land on
 * whichever node of the draft happens to sit at that path.
 *
 * AND WHAT REACHES OUTSIDE IT IS REFUSED HERE FIRST ([scopeLimit]). The
 * engine refuses a lead every change outside what they lead; the parts of the
 * draft that are outside are few and known — the company's charter and
 * settings, its top level, and the unit's own place and lead, which decide who
 * leads it — so the builder disables those controls with the reason and
 * refuses the operation at its recording door, rather than letting a lead
 * build a draft the engine will only refuse.
 */

import type { CompanyDocument, ConfigUnit, Derived } from "~/protocol/index.ts";
import { pathOfSegments, type Segment } from "./document.ts";
import type { Draft } from "./draft.ts";
import { isRecord } from "./json.ts";
import { COMPANY_KEY, type NodeKey } from "./keys.ts";
import type { Intent } from "./operations.ts";
import { unitPath, type ConfigRequest, type ConfigTransport } from "./transport.ts";

/** The document a lead's draft stands for: the company's name and the one unit. */
export function scopedDocument(
  name: string,
  unit: Readonly<Record<string, unknown>>,
): CompanyDocument {
  return { name, units: [unit as ConfigUnit] };
}

/** Where the unit sits in the draft's document. */
const AT: readonly Segment[] = ["units", 0];

/** A path's segments, as the engine renders paths: dotted keys and bracketed indexes. */
function segmentsOf(path: string): Segment[] {
  return (path.match(/\[\d+\]|[^.[\]]+/g) ?? []).map((token) =>
    token.startsWith("[") ? Number(token.slice(1, -1)) : token,
  );
}

/** `segments` moved from under `prefix` to under the draft's unit, or `null` outside it. */
function moved(segments: readonly Segment[], prefix: readonly Segment[]): Segment[] | null {
  if (segments.length < prefix.length) return null;
  for (let i = 0; i < prefix.length; i++) if (segments[i] !== prefix[i]) return null;
  return [...AT, ...segments.slice(prefix.length)];
}

/**
 * An answer's body, with every path the engine wrote moved into the draft's
 * coordinates (see the module doc). What lies outside the unit is dropped from
 * the derivation and the warnings — nothing of the draft is about it — and a
 * problem there is kept at document level, without its path, because it may
 * still be what stops the write.
 */
export function inScope(body: unknown, scope: string): unknown {
  if (!isRecord(body)) return body;
  const derived = isRecord(body.derived) ? (body.derived as unknown as Derived) : null;
  const at = derived?.units?.find((u) => u.id === scope)?.path;
  const prefix = at === undefined ? null : segmentsOf(at);
  const place = (path: string | undefined): string | null => {
    if (prefix === null || path === undefined) return null;
    const next = moved(segmentsOf(path), prefix);
    return next === null ? null : pathOfSegments(next);
  };
  const out: Record<string, unknown> = { ...body };
  if (derived) {
    out.derived = {
      seats: (derived.seats ?? []).flatMap((s) => {
        const path = place(s.path);
        return path === null ? [] : [{ ...s, path }];
      }),
      units: (derived.units ?? []).flatMap((u) => {
        const path = place(u.path);
        return path === null ? [] : [{ ...u, path }];
      }),
    };
  }
  const located = (finding: Record<string, unknown>): Segment[] | null => {
    const segments = Array.isArray(finding.segments) ? (finding.segments as Segment[]) : null;
    return prefix === null || segments === null ? null : moved(segments, prefix);
  };
  if (Array.isArray(body.problems)) {
    out.problems = body.problems.filter(isRecord).map((p) => {
      const segments = located(p);
      return segments === null
        ? { ...p, path: "", segments: null }
        : { ...p, path: pathOfSegments(segments), segments };
    });
  }
  if (Array.isArray(body.warnings)) {
    out.warnings = body.warnings.filter(isRecord).flatMap((w) => {
      const segments = located(w);
      return segments === null ? [] : [{ ...w, path: pathOfSegments(segments), segments }];
    });
  }
  return out;
}

/**
 * The configuration transport for a lead's draft of the unit `scope`: the
 * company read as that unit ([scopedDocument], named by `company()`), and
 * every answer moved into the draft's coordinates ([inScope]).
 */
export function scopedTransport(
  inner: ConfigTransport,
  scope: string,
  company: () => string,
): ConfigTransport {
  const read: ConfigRequest = {
    method: "GET",
    path: unitPath(scope),
    query: {},
    contentType: "application/json",
    headers: {},
  };
  return {
    async current(signal) {
      const answer = await inner.send(read, signal);
      return answer.status === 200 && isRecord(answer.body)
        ? { ...answer, body: scopedDocument(company(), answer.body) }
        : answer;
    },
    async send(request, signal) {
      const answer = await inner.send(request, signal);
      return { ...answer, body: inScope(answer.body, scope) };
    },
    revision: (id, signal) => inner.revision(id, signal),
  };
}

/**
 * What about a node a lead's draft may not change: the company's own charter
 * and settings (`edit` of the company), anything at its top level (`add`
 * under the company), and the unit's own place and lead (`place` and `lead`
 * of the draft's one top-level unit).
 */
export type ScopedChange = "edit" | "add" | "place" | "lead";

/**
 * Why a draft scoped to one unit may not make `change` to `key`, or `null`
 * when it may — always `null` for the whole company's draft (`scope` null).
 */
export function scopeLimit(
  scope: string | null,
  draft: Draft,
  key: NodeKey,
  change: ScopedChange,
): string | null {
  if (scope === null) return null;
  const top = draft.units[0];
  const name = top?.data.name || "this unit";
  if (key === COMPANY_KEY && change === "edit") {
    return `The company's charter and settings are not part of ${name}: changing them takes the config:write grant.`;
  }
  if (key === COMPANY_KEY && change === "add") {
    return `Only ${name} and what is inside it can be changed here: the company's top level takes the config:write grant.`;
  }
  if (top !== undefined && key === top.key && change === "place") {
    return `Where ${name} sits decides who leads it, so moving or deleting it takes the config:write grant.`;
  }
  if (top !== undefined && key === top.key && change === "lead") {
    return `${name}'s own lead decides who may change it, so changing it takes the config:write grant.`;
  }
  return null;
}

/** The first limit [scopeLimit] puts on an intent, or `null` when it reaches nowhere it may not. */
export function outsideScope(scope: string | null, draft: Draft, intent: Intent): string | null {
  if (scope === null) return null;
  const limit = (key: NodeKey, change: ScopedChange) => scopeLimit(scope, draft, key, change);
  switch (intent.type) {
    case "updateCompany":
    case "setDatadogRouteTo":
    case "applyTemplate":
      return limit(COMPANY_KEY, "edit");
    case "addSeat":
    case "addUnit":
      return limit(intent.placement.parent, "add");
    case "remove":
    case "reorder":
      return limit(intent.target, "place");
    case "move":
      return limit(intent.target, "place") ?? limit(intent.to.parent, "add");
    case "setLead":
      return limit(intent.target, "lead");
    case "edit":
      for (const part of intent.intents) {
        const reason = outsideScope(scope, draft, part);
        if (reason !== null) return reason;
      }
      return null;
    default:
      return null;
  }
}
