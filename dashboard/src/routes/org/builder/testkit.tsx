/**
 * Fixtures for the Builder's component suites. Imported by tests only.
 *
 * A SCRIPTED ENGINE BEHIND A STUBBED FETCH, in the Secrets suite's idiom: the
 * Builder runs its real transport, its real clock and real session storage,
 * so what a suite asserts is the request that left the page and what the page
 * did with the answer. The engine's derivation of a sent document is the
 * model's fixture derivation (`model/testkit.ts`), which is fixture data for
 * ASCII names and not a port of the engine.
 *
 * The views are stand-ins that read and act only through `BuilderContext`,
 * exactly as the real canvas and outline do, so the suites exercise the
 * Builder rather than any one view.
 */

import { render } from "@testing-library/react";
import { vi } from "vitest";
import type { ReactNode } from "react";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import {
  LiveSocket,
  Store,
  type CompanyDocument,
  type ConfigProblem,
  type OrgProjection,
} from "~/protocol/index.ts";
import { Builder, type BuilderSurfaces } from "./Builder.tsx";
import { useBuilder, type ChartKind } from "./BuilderContext.tsx";
import { allSeats, allUnits } from "./model/draft.ts";
import { COMPANY_KEY, type KeySource } from "./model/keys.ts";
import type { DraftStorage } from "./model/persistence.ts";
import { fixtureDerived } from "./model/testkit.ts";
import { builderSurfaces } from "./surfaces.ts";

export class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
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

/** A small company with a model provider, two root seats and a unit. */
export function company(): CompanyDocument {
  return {
    name: "Acme",
    providers: { llm: { default: { type: "anthropic", model: "claude-sonnet-5" } } },
    roles: [
      { name: "CEO", goal: "Lead", manages: ["Engineering"] },
      { name: "Designer", goal: "Design" },
    ],
    units: [{ name: "Engineering", type: "department", lead: "Dev", roles: [{ name: "Dev" }] }],
  };
}

/**
 * A merge patch applied to a document, as RFC 7396 defines it. The builder
 * sends one; the scripted engine has to hold the result to derive it.
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

/** The scripted engine: the active revision, and every request the page sent. */
export class Engine {
  document: CompanyDocument | null;
  revision: string;
  readonly requests: SentRequest[] = [];
  /** Every revision a write stored: its parent and its audit summary. */
  readonly revisions = new Map<string, { parent: string | null; summary: string }>();
  script: Script = () => null;

  constructor(document: CompanyDocument | null, revision = "r1") {
    this.document = document;
    this.revision = revision;
  }

  /** The requests with this method and path, in order. */
  sent(method: string, path = "/config"): SentRequest[] {
    return this.requests.filter((r) => r.method === method && r.path === path);
  }

  /** The dry runs, in order. */
  checks(): SentRequest[] {
    return this.requests.filter((r) => r.query.get("dry_run") === "true");
  }

  /** The document a write or dry run would produce. */
  result(request: SentRequest): CompanyDocument {
    const body = { ...(request.body as Record<string, unknown>) };
    delete body._summary;
    return (
      request.method === "PUT" ? body : mergePatch(this.document ?? {}, body)
    ) as CompanyDocument;
  }

  /** Stores a write as the engine does, making it the active revision, and names it. */
  commit(request: SentRequest): string {
    const revision = this.revisions.size === 0 ? "r-saved" : `r-saved-${this.revisions.size + 1}`;
    const parent = this.document ? this.revision : null;
    this.document = this.result(request);
    this.revision = revision;
    const summary = (request.body as { _summary?: unknown })._summary;
    this.revisions.set(revision, { parent, summary: typeof summary === "string" ? summary : "" });
    return revision;
  }

  /** The default answer: the configuration surface as the spec describes it. */
  answer(request: SentRequest): Response {
    if (request.method === "GET" && request.path === "/config") {
      return this.document
        ? json(this.document, 200, { ETag: `"${this.revision}"` })
        : json({ error: "no_active_revision" }, 404);
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
      const expected = request.headers["If-Match"];
      if (request.method === "PATCH" && expected !== `"${this.revision}"`) {
        return json({ error: "revision_advanced", current_revision_id: this.revision }, 409);
      }
      if (request.method === "PUT" && request.headers["If-None-Match"] === "*" && this.document) {
        return json({ error: "already_configured", current_revision_id: this.revision }, 412);
      }
      const result = this.result(request);
      const derived = fixtureDerived(result);
      if (request.query.get("dry_run") === "true") {
        return json({
          valid: true,
          base_revision_id: this.document ? this.revision : "",
          warnings: [],
          derived,
        });
      }
      const revision = this.commit(request);
      return json(
        { revision_id: revision, epoch: this.revisions.size + 1, warnings: [], derived },
        201,
      );
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

/** A refusal carrying located problems, as a validation error answers. */
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
              placement: { parent: COMPANY_KEY, after: null },
              data: { name: "Analyst", goal: "Analyse" },
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
          </li>
        ))}
      </ul>
    </div>
  );
}

/** The canvas stand-in, which draws whichever chart the Builder hands it. */
export function FakeCanvas({ chart }: { chart: ChartKind }) {
  return (
    <div>
      <p>{`Drawing the ${chart} chart`}</p>
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
  hash = "#/org?lens=builder&view=visualization",
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
