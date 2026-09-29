/**
 * Fixtures for the Builder's component suites. Imported by tests only.
 *
 * A SCRIPTED ENGINE BEHIND A STUBBED FETCH, in the Secrets suite's idiom: the
 * Builder runs its real transport, its real clock and real session storage,
 * so what a suite asserts is the request that left the page and what the page
 * did with the answer.
 *
 * TWO SURFACES, AS THE ENGINE HAS: the settings revision `/config` serves and
 * validates whole, and the org chart `/chart` serves and writes per object.
 * The chart half keeps rows and applies the batch operations and content
 * writes the builder sends by their documented meaning — renames carrying
 * every reference with them, a refused batch applying nothing, an operation id
 * answered from its ledger — which is enough to read back what a save wrote.
 * It is not a port of the chart's rules: a suite that needs a refusal scripts
 * it. The derivation an org push carries is the model's fixture derivation
 * ([Engine.orgPush]), laid out from the rows and never computed.
 *
 * The views are stand-ins that read and act only through `BuilderContext`,
 * exactly as the real canvas and outline do, so the suites exercise the
 * Builder rather than any one view.
 */

import { act, render } from "@testing-library/react";
import { vi } from "vitest";
import type { ReactNode } from "react";
import { Router } from "~/app/router.tsx";
import { REDACTED } from "~/lib/format.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import {
  LiveSocket,
  Store,
  type ChartRead,
  type ChartSeat,
  type ChartUnit,
  type CompanyDocument,
  type ConfigProblem,
  type OrgProjection,
} from "~/protocol/index.ts";
import { Builder, type BuilderSurfaces } from "./Builder.tsx";
import { useBuilder, type ChartKind } from "./BuilderContext.tsx";
import { allSeats, allUnits } from "./model/draft.ts";
import { COMPANY_KEY, type KeySource } from "./model/keys.ts";
import type { DraftStorage } from "./model/persistence.ts";
import { chartOf, fixtureDerived, type DerivedOverrides } from "./model/testkit.ts";
import { builderSurfaces } from "./surfaces.ts";

export class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

/**
 * A socket query channel that answers `viewer` as whoever `who()` names now —
 * the session the browser's cookie carries, which a suite changes the way
 * another tab signing in would — and nothing else. `""` is nobody.
 */
export function asReader(who: () => string): (what: string) => unknown {
  return (what) =>
    what === "viewer"
      ? { login: who(), grants: [], handle: "", name: "", kind: "", owner: who() }
      : null;
}

/**
 * Has the lens ask who it writes as again, as a reconnect does: the viewer is
 * re-read on every change of connection, which is how a sign-in in another
 * tab reaches a tab that stayed open.
 */
export function rereadViewer(store: Store): void {
  act(() => store.setConnected(!store.state.connected));
}

/** One request as it left the page. */
export interface SentRequest {
  readonly method: string;
  readonly path: string;
  readonly query: URLSearchParams;
  readonly headers: Record<string, string>;
  readonly body: unknown;
}

/** What the scripted engine answers a request with; `null` falls through to the default. */
export type Script = (request: SentRequest, engine: Engine) => Response | null | Promise<Response>;

export const json = (payload: unknown, status = 200, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

/** A company as the engine holds it: its settings revision and its org chart. */
export interface Company {
  settings: CompanyDocument;
  chart: ChartRead;
}

/**
 * A small company with a model provider, two root seats and a unit: the CEO
 * manages Engineering, whose lead is Dev.
 */
export function company(): Company {
  return {
    settings: {
      name: "Acme",
      providers: { llm: { default: { type: "anthropic", model: "claude-sonnet-5" } } },
    },
    chart: chartOf({
      units: [{ key: "engineering", name: "Engineering", type: "department", lead: "dev" }],
      seats: [
        { handle: "ceo", name: "CEO", goal: "Lead" },
        { handle: "designer", name: "Designer", goal: "Design" },
        { handle: "dev", name: "Dev", unit: "engineering" },
      ],
      manages: { ceo: ["engineering"] },
    }),
  };
}

/**
 * A merge patch applied to a document, as RFC 7396 defines it. The builder
 * sends one; the scripted engine has to hold the result to answer reads.
 */
export function mergePatch(target: unknown, patch: unknown): unknown {
  if (patch === null || typeof patch !== "object" || Array.isArray(patch)) return patch;
  const out: Record<string, unknown> =
    target !== null && typeof target === "object" && !Array.isArray(target)
      ? { ...(target as Record<string, unknown>) }
      : {};
  for (const [key, value] of Object.entries(patch as Record<string, unknown>)) {
    if (value === null) delete out[key];
    else out[key] = mergePatch(out[key], value);
  }
  return out;
}

/** The chart log's name in the positions the scripted engine answers. */
const CHART_LOG = "CREWLET_CHART_LOG";

/** One batch operation as the builder sends it. */
interface BatchOperation {
  kind: string;
  object: { kind: "seat" | "unit"; id: string };
  parent?: string;
  lead?: string;
  seat_kind?: string;
  manages?: string[];
  to?: string;
}

/** Why a batch was refused: the operation, and the chart's sentence. */
class BatchRefusal extends Error {
  constructor(
    readonly index: number,
    readonly rule: string,
    detail: string,
  ) {
    super(detail);
  }
}

const clone = <T,>(value: T): T => structuredClone(value);
const isObject = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === "object" && !Array.isArray(value);

/**
 * A stated runtime half with every mask put back from the stored one, as the
 * chart's writer restores a value a read masked: the mask means "unchanged".
 */
function restoreMasked(stated: unknown, stored: unknown): unknown {
  if (stated === REDACTED) return stored;
  if (Array.isArray(stated)) {
    const was = Array.isArray(stored) ? stored : [];
    return stated.map((v, i) => restoreMasked(v, was[i]));
  }
  if (isObject(stated)) {
    const was = isObject(stored) ? stored : {};
    return Object.fromEntries(
      Object.entries(stated).map(([k, v]) => [k, restoreMasked(v, was[k])]),
    );
  }
  return stated;
}

/** A runtime half as a read masks it: every string that is not a whole reference, masked. */
function maskRuntime(value: unknown): unknown {
  if (typeof value === "string")
    return /^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/.test(value) ? value : REDACTED;
  if (Array.isArray(value)) return value.map(maskRuntime);
  if (isObject(value)) {
    return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, maskRuntime(v)]));
  }
  return value;
}

/** A row's content fields: the stated ones that hold something. */
function contentOf(
  body: Record<string, unknown>,
  fields: readonly string[],
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const field of fields) {
    const value = body[field];
    if (typeof value === "string" ? value !== "" : Array.isArray(value) && value.length > 0) {
      out[field] = clone(value);
    }
  }
  return out;
}

const SEAT_CONTENT = [
  "name",
  "email",
  "backstory",
  "goal",
  "responsibilities",
  "behavioral_guidelines",
  "project",
  "space",
] as const;
const UNIT_CONTENT = [
  "name",
  "type",
  "purpose",
  "goals",
  "channel",
  "project",
  "space",
  "knowledge_refs",
] as const;

/** The scripted engine: the active settings revision, the chart's rows, and every request the page sent. */
export class Engine {
  settings: CompanyDocument | null;
  revision: string;
  units: ChartUnit[];
  seats: ChartSeat[];
  /** Every seat's authored `manages:` list, by handle. */
  manages: Record<string, string[]>;
  /** Whether a `?runtime=true` read is served the runtime half; `false` is a reader without `config:read`. */
  runtimeVisible = true;
  /**
   * Whether the runtime half is served as a read serves it: every credential
   * that is not a whole reference masked. Off, a suite sees the rows it wrote.
   */
  maskRuntime = false;
  /** The chart log's last position. */
  position = 10;
  readonly requests: SentRequest[] = [];
  /** Every revision a write stored: its parent and its audit summary. */
  readonly revisions = new Map<string, { parent: string | null; summary: string }>();
  /** Every chart write's answer, by its operation id, as the chart's ledger keeps it. */
  readonly ledger = new Map<string, { status: number; body: unknown }>();
  script: Script = () => null;

  constructor(company: Company | null, revision = "r1") {
    this.settings = company ? clone(company.settings) : null;
    this.revision = revision;
    this.units = company ? clone(company.chart.units) : [];
    this.seats = company ? clone(company.chart.seats) : [];
    this.manages = company ? clone(company.chart.manages ?? {}) : {};
  }

  /** The requests with this method and path, in order. */
  sent(method: string, path = "/config"): SentRequest[] {
    return this.requests.filter((r) => r.method === method && r.path === path);
  }

  /** The settings dry runs, in order. */
  checks(): SentRequest[] {
    return this.requests.filter((r) => r.query.get("dry_run") === "true");
  }

  /** The reads of the whole chart, in order: every check sends one. */
  chartReads(): SentRequest[] {
    return this.sent("GET", "/chart");
  }

  /** The chart writes, in order: batches and content writes. */
  chartWrites(): SentRequest[] {
    return this.requests.filter(
      (r) => r.method !== "GET" && (r.path === "/chart/batch" || r.path.startsWith("/chart/")),
    );
  }

  /** The chart as `GET /chart` serves it, with or without the runtime half. */
  chart(runtime = true): ChartRead {
    const served = runtime && this.runtimeVisible;
    const strip = <T extends { runtime?: unknown }>(row: T): T => {
      const { runtime: half, ...rest } = clone(row);
      if (!served || half === undefined) return rest as T;
      return { ...rest, runtime: this.maskRuntime ? maskRuntime(half) : half } as T;
    };
    const read = chartOf(
      { units: this.units.map(strip), seats: this.seats.map(strip), manages: this.manages },
      served,
    );
    return {
      ...read,
      answer: { level: "linearizable", position: `${CHART_LOG}@1:${this.position}` },
    };
  }

  /**
   * The org push the engine sends after an apply: the company's name and the
   * derivation of the chart it holds now. The derivation is fixture data (see
   * `model/testkit.ts`), so a suite states the managers and leads it needs.
   */
  orgPush(overrides: DerivedOverrides = {}): OrgProjection {
    return {
      name: typeof this.settings?.name === "string" ? this.settings.name : "",
      roles: [],
      units: [],
      derived: fixtureDerived(this.chart(), overrides),
    };
  }

  /** The settings document a write or dry run would produce. */
  result(request: SentRequest): CompanyDocument {
    const body = { ...(request.body as Record<string, unknown>) };
    delete body._summary;
    return (
      request.method === "PUT" ? body : mergePatch(this.settings ?? {}, body)
    ) as CompanyDocument;
  }

  /** Stores a settings write as the engine does, making it the active revision, and names it. */
  commit(request: SentRequest): string {
    const revision = this.revisions.size === 0 ? "r-saved" : `r-saved-${this.revisions.size + 1}`;
    const parent = this.settings ? this.revision : null;
    this.settings = this.result(request);
    this.revision = revision;
    const summary = (request.body as { _summary?: unknown })._summary;
    this.revisions.set(revision, { parent, summary: typeof summary === "string" ? summary : "" });
    return revision;
  }

  private seat(handle: string): ChartSeat | undefined {
    return this.seats.find((s) => s.handle === handle);
  }

  private unit(key: string): ChartUnit | undefined {
    return this.units.find((u) => u.key === key);
  }

  /** Applies one batch, all of it or none: a refusal names the operation it stopped at. */
  applyBatch(operations: readonly BatchOperation[]): void {
    const saved = {
      units: clone(this.units),
      seats: clone(this.seats),
      manages: clone(this.manages),
    };
    try {
      operations.forEach((op, index) => this.applyOperation(op, index));
    } catch (err) {
      this.units = saved.units;
      this.seats = saved.seats;
      this.manages = saved.manages;
      throw err;
    }
  }

  private applyOperation(op: BatchOperation, index: number): void {
    const { kind, id } = op.object;
    const missing = () =>
      new BatchRefusal(index, "object_absent", `The chart holds no ${kind} ${id}.`);
    const held = () => (kind === "seat" ? this.seat(id) : this.unit(id)) !== undefined;
    switch (op.kind) {
      case "create_unit":
        if (held()) throw new BatchRefusal(index, "address_taken", `The key ${id} is taken.`);
        this.units.push({
          key: id,
          ...(op.parent ? { parent: op.parent } : {}),
          ...(op.lead ? { lead: op.lead } : {}),
        });
        return;
      case "create_seat":
        if (held()) throw new BatchRefusal(index, "address_taken", `The handle ${id} is taken.`);
        this.seats.push({
          handle: id,
          ...(op.seat_kind && op.seat_kind !== "agent" ? { kind: op.seat_kind } : {}),
          ...(op.parent ? { unit: op.parent } : {}),
        });
        return;
      case "move": {
        if (kind === "seat") {
          const seat = this.seat(id);
          if (!seat) throw missing();
          if (op.parent) seat.unit = op.parent;
          else delete seat.unit;
        } else {
          const unit = this.unit(id);
          if (!unit) throw missing();
          if (op.parent) unit.parent = op.parent;
          else delete unit.parent;
        }
        return;
      }
      case "set_kind": {
        const seat = this.seat(id);
        if (!seat) throw missing();
        if (op.seat_kind && op.seat_kind !== "agent") seat.kind = op.seat_kind;
        else delete seat.kind;
        return;
      }
      case "set_lead": {
        const unit = this.unit(id);
        if (!unit) throw missing();
        if (op.lead) unit.lead = op.lead;
        else delete unit.lead;
        return;
      }
      case "set_manages":
        if (!this.seat(id)) throw missing();
        if (op.manages && op.manages.length > 0) this.manages[id] = [...op.manages];
        else delete this.manages[id];
        return;
      case "rename":
        this.rename(kind, id, op.to ?? "", index);
        return;
      case "remove":
        if (kind === "seat") {
          if (!this.seat(id)) throw missing();
          this.seats = this.seats.filter((s) => s.handle !== id);
          delete this.manages[id];
        } else {
          if (!this.unit(id)) throw missing();
          if (this.seats.some((s) => s.unit === id) || this.units.some((u) => u.parent === id)) {
            throw new BatchRefusal(
              index,
              "unit_not_empty",
              `The unit ${id} still holds something.`,
            );
          }
          this.units = this.units.filter((u) => u.key !== id);
        }
        return;
      default:
        throw new BatchRefusal(index, "unknown_operation", `No operation ${op.kind}.`);
    }
  }

  /** A rename, carrying every reference to the object with it, as the chart's record does. */
  private rename(kind: "seat" | "unit", from: string, to: string, index: number): void {
    const moved = (list: readonly string[] | undefined) =>
      (list ?? []).map((entry) => (entry === from ? to : entry));
    for (const [holder, list] of Object.entries(this.manages)) this.manages[holder] = moved(list);
    if (kind === "seat") {
      const seat = this.seat(from);
      if (!seat) throw new BatchRefusal(index, "object_absent", `The chart holds no seat ${from}.`);
      if (this.seat(to))
        throw new BatchRefusal(index, "address_taken", `The handle ${to} is taken.`);
      seat.origin_handle = seat.origin_handle ?? from;
      seat.former_handles = [...(seat.former_handles ?? []), from];
      seat.handle = to;
      if (this.manages[from]) {
        this.manages[to] = this.manages[from]!;
        delete this.manages[from];
      }
      for (const unit of this.units) if (unit.lead === from) unit.lead = to;
      return;
    }
    const unit = this.unit(from);
    if (!unit) throw new BatchRefusal(index, "object_absent", `The chart holds no unit ${from}.`);
    if (this.unit(to)) throw new BatchRefusal(index, "address_taken", `The key ${to} is taken.`);
    unit.origin_key = unit.origin_key ?? from;
    unit.former_keys = [...(unit.former_keys ?? []), from];
    unit.key = to;
    for (const child of this.units) if (child.parent === from) child.parent = to;
    for (const seat of this.seats) if (seat.unit === from) seat.unit = to;
  }

  /** Writes one object's content: full post-state, the runtime half only when stated. */
  writeContent(kind: "seat" | "unit", address: string, body: Record<string, unknown>): boolean {
    if (kind === "seat") {
      const seat = this.seat(address);
      if (!seat) return false;
      const email = body.email === REDACTED ? seat.email : body.email;
      const next: ChartSeat = {
        handle: seat.handle,
        ...(seat.kind ? { kind: seat.kind } : {}),
        ...(seat.unit ? { unit: seat.unit } : {}),
        ...contentOf({ ...body, email }, SEAT_CONTENT),
        ...(seat.former_handles ? { former_handles: seat.former_handles } : {}),
        ...(seat.origin_handle ? { origin_handle: seat.origin_handle } : {}),
      };
      const runtime = body.clear_runtime
        ? undefined
        : "runtime" in body
          ? restoreMasked(body.runtime, seat.runtime)
          : seat.runtime;
      if (runtime !== undefined) next.runtime = runtime as ChartSeat["runtime"];
      this.seats[this.seats.indexOf(seat)] = next;
      return true;
    }
    const unit = this.unit(address);
    if (!unit) return false;
    const next: ChartUnit = {
      key: unit.key,
      ...(unit.parent ? { parent: unit.parent } : {}),
      ...(unit.lead ? { lead: unit.lead } : {}),
      ...contentOf(body, UNIT_CONTENT),
      ...(unit.former_keys ? { former_keys: unit.former_keys } : {}),
      ...(unit.origin_key ? { origin_key: unit.origin_key } : {}),
    };
    const runtime = body.clear_runtime
      ? undefined
      : "runtime" in body
        ? restoreMasked(body.runtime, unit.runtime)
        : unit.runtime;
    if (runtime !== undefined) next.runtime = runtime as ChartUnit["runtime"];
    this.units[this.units.indexOf(unit)] = next;
    return true;
  }

  /** A chart write's answer, recorded in the ledger under its operation id. */
  private chartWrite(request: SentRequest): Response {
    const opId = request.headers["Idempotency-Key"] ?? "";
    const seen = this.ledger.get(opId);
    if (seen) return json(seen.body, seen.status);
    const body = request.body as Record<string, unknown>;
    let answer: { status: number; body: unknown };
    if (request.path === "/chart/batch") {
      try {
        this.applyBatch(body.operations as BatchOperation[]);
        answer = this.applied(opId);
      } catch (err) {
        if (!(err instanceof BatchRefusal)) throw err;
        answer = {
          status: 422,
          body: { error: "refused", detail: err.message, index: err.index, rule: err.rule },
        };
      }
    } else {
      const [, , collection, raw] = request.path.split("/");
      const kind = collection === "seats" ? "seat" : "unit";
      const address = decodeURIComponent(raw ?? "");
      answer = this.writeContent(kind, address, body)
        ? this.applied(opId)
        : { status: 404, body: { error: "not_found", detail: `No ${kind} ${address}.` } };
    }
    this.ledger.set(opId, answer);
    return json(answer.body, answer.status);
  }

  private applied(opId: string): { status: number; body: unknown } {
    this.position += 1;
    return {
      status: 200,
      body: { outcome: "applied", position: `${CHART_LOG}@1:${this.position}`, op_id: opId },
    };
  }

  /** The default answer: the two surfaces as the reference describes them. */
  answer(request: SentRequest): Response {
    if (request.method === "GET" && request.path === "/config") {
      return this.settings
        ? json(this.settings, 200, { ETag: `"${this.revision}"` })
        : json({ error: "no_active_revision" }, 404);
    }
    if (request.method === "GET" && request.path === "/chart") {
      return json(this.chart(request.query.get("runtime") === "true"));
    }
    if (request.method === "GET" && request.path.startsWith("/config/revisions/")) {
      const id = decodeURIComponent(request.path.slice("/config/revisions/".length));
      const revision = this.revisions.get(id);
      return revision
        ? json({
            revision_id: id,
            parent_revision_id: revision.parent ?? "",
            summary: revision.summary,
            source: "api",
            created_by: "operator",
            created_at: "2026-09-13T12:00:00Z",
            payload: {},
          })
        : json({ error: "not_found" }, 404);
    }
    if (request.path === "/config" && (request.method === "PATCH" || request.method === "PUT")) {
      // THE CONFIGURATION CARRIES NO CHART, and refuses one by name.
      const body = request.body as Record<string, unknown>;
      if ("roles" in body || "units" in body) {
        return json(
          { error: "validation_error", detail: "The org chart is written through /chart." },
          400,
        );
      }
      const expected = request.headers["If-Match"];
      if (request.method === "PATCH" && expected !== `"${this.revision}"`) {
        return json({ error: "revision_advanced", current_revision_id: this.revision }, 409);
      }
      if (request.method === "PUT" && request.headers["If-None-Match"] === "*" && this.settings) {
        return json({ error: "already_configured", current_revision_id: this.revision }, 412);
      }
      if (request.query.get("dry_run") === "true") {
        return json({
          valid: true,
          base_revision_id: this.settings ? this.revision : "",
          warnings: [],
        });
      }
      const revision = this.commit(request);
      return json({ revision_id: revision, epoch: this.revisions.size + 1, warnings: [] }, 201);
    }
    if (request.path.startsWith("/chart/") && request.method !== "GET") {
      return this.chartWrite(request);
    }
    return json({});
  }

  /** Installs the engine as `fetch`. */
  install(): void {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(String(input));
        const headers = { ...((init?.headers ?? {}) as Record<string, string>) };
        const text = typeof init?.body === "string" ? init.body : undefined;
        const request: SentRequest = {
          method: init?.method ?? "GET",
          path: url.pathname,
          query: url.searchParams,
          headers,
          body: text === undefined ? undefined : JSON.parse(text),
        };
        this.requests.push(request);
        if (init?.signal?.aborted) throw new DOMException("aborted", "AbortError");
        const scripted = await this.script(request, this);
        return scripted ?? this.answer(request);
      }),
    );
  }
}

/** A refusal carrying located problems, as a settings validation error answers. */
export function refusal(problems: ConfigProblem[], status = 400): Response {
  return json(
    {
      error: "validation_error",
      detail: problems.map((p) => p.message).join("\n"),
      hint: "Fix the fields named above.",
      problems,
    },
    status,
  );
}

/** A stand-in view: every seat with its problem count and two edits. */
export function FakeView() {
  const api = useBuilder();
  return (
    <div>
      <p>{api.readOnly ? "read only" : "editable"}</p>
      <button type="button" onClick={() => api.selection.select(COMPANY_KEY)}>
        Select the company
      </button>
      <button
        type="button"
        onClick={() =>
          api.dispatch({
            type: "record",
            intent: { type: "updateCompany", set: [{ path: ["name"], value: "Acme Labs" }] },
          })
        }
      >
        Rename the company
      </button>
      <button
        type="button"
        onClick={() =>
          api.dispatch({
            type: "record",
            intent: {
              type: "addSeat",
              // Minted in the handler, as every view mints a new node's key.
              key: "new:analyst",
              placement: { parent: COMPANY_KEY },
              data: { handle: "analyst", name: "Analyst", goal: "Analyse" },
            },
          })
        }
      >
        Add an analyst
      </button>
      <ul aria-label="Units">
        {[...allUnits(api.state.draft)].map(({ unit }) => (
          <li key={unit.key}>
            <span>{unit.data.name}</span>
            <button type="button" onClick={() => api.selection.select(unit.key)}>
              {`Select ${unit.data.name}`}
            </button>
            <button
              type="button"
              onClick={() =>
                api.dispatch({
                  type: "record",
                  intent: { type: "renameUnit", target: unit.key, name: `${unit.data.name} Two` },
                })
              }
            >
              {`Rename ${unit.data.name}`}
            </button>
            <button
              type="button"
              onClick={() =>
                api.dispatch({
                  type: "record",
                  intent: {
                    type: "updateUnit",
                    target: unit.key,
                    set: [{ path: ["key"], value: `${unit.data.key}-two` }],
                  },
                })
              }
            >
              {`Readdress ${unit.data.name}`}
            </button>
          </li>
        ))}
      </ul>
      <ul aria-label="Seats">
        {[...allSeats(api.state.draft)].map(({ seat }) => (
          <li key={seat.key}>
            <span>{seat.data.name}</span>{" "}
            <span data-testid={`problems ${seat.data.name}`}>
              {api.problemsFor(seat.key).length}
            </span>
            <button type="button" onClick={() => api.selection.select(seat.key)}>
              {`Select ${seat.data.name}`}
            </button>
            <button
              type="button"
              onClick={() =>
                api.dispatch({
                  type: "record",
                  intent: {
                    type: "updateSeat",
                    target: seat.key,
                    set: [{ path: ["goal"], value: `${seat.data.goal ?? ""} and more` }],
                  },
                })
              }
            >
              {`Edit ${seat.data.name}`}
            </button>
            <button
              type="button"
              onClick={() =>
                api.dispatch({ type: "record", intent: { type: "remove", target: seat.key } })
              }
            >
              {`Remove ${seat.data.name}`}
            </button>
            <button
              type="button"
              onClick={() =>
                api.dispatch({
                  type: "record",
                  intent: { type: "move", target: seat.key, to: { parent: COMPANY_KEY } },
                })
              }
            >
              {`Move ${seat.data.name} to the top`}
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * The canvas stand-in, which draws whichever chart the Builder hands it.
 *
 * IT DRAWS THE CHROME TOO, because the real canvas does: the fullscreen
 * toggle and the switch between the two charts sit in the chart's own corner
 * rather than in the page toolbar, so a Builder suite that did not render
 * them could not reach either.
 */
export function FakeCanvas({
  chart,
  chrome = {},
  about = null,
  adding = null,
}: {
  chart: ChartKind;
  chrome?: { controls?: ReactNode; switcher?: ReactNode };
  about?: string | null;
  /** An add the lens handed the chart to draw itself, rather than opening a dialog. */
  adding?: { parent: string | null; kind?: string; opening: number; onClose: () => void } | null;
}) {
  return (
    <div>
      <p>{`Drawing the ${chart} chart`}</p>
      {about !== null && <p>{`About ${about}`}</p>}
      {adding !== null && (
        <p>{`Adding ${adding.kind ?? "something"} to ${adding.parent ?? "the company"}`}</p>
      )}
      {chrome.controls}
      {chrome.switcher}
      <FakeView />
    </div>
  );
}

/**
 * The lens's own surfaces with the two views stood in: the suites exercise
 * the Builder rather than a view, and the dialogs they open are the real ones.
 */
export const fakeSurfaces: BuilderSurfaces = {
  ...builderSurfaces,
  canvas: FakeCanvas,
  table: FakeView,
};

/** Mounts the Builder lens against the scripted engine. */
export function mountBuilder({
  engine,
  org = null,
  connected = true,
  hash = "#/company?lens=builder&view=visualization",
  surfaces = fakeSurfaces,
  storage,
  keys,
  query = () => null,
  wrap = (tree) => tree,
}: {
  engine: Engine;
  org?: OrgProjection | null;
  connected?: boolean;
  hash?: string;
  surfaces?: BuilderSurfaces;
  storage?: DraftStorage | null;
  /** Where the lens mints keys and write ids; the browser's random source otherwise. */
  keys?: KeySource;
  /** What the socket's query channel answers, by name. */
  query?: (what: string) => unknown;
  /** A provider the lens reads from, which the application frame supplies. */
  wrap?: (tree: ReactNode) => ReactNode;
}) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = hash;
  engine.install();
  const store = new Store();
  if (connected) store.applyHealth({ status: "ok" });
  if (org) store.applyOrg(org);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(query(what));
  const view = render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        {wrap(<Builder surfaces={surfaces} storage={storage} {...(keys ? { keys } : {})} />)}
      </Router>
    </ClientContext.Provider>,
  );
  return { store, socket, view };
}
