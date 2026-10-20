/**
 * The page bridge's READS: a document, a page of a collection, a query and
 * the schema, made to the engine as the signed-in viewer through the
 * datastore's own questions (`artifact_data_*`), which change nothing and
 * join no room. Shared by the page host (`host.ts`), which answers every call
 * an open page makes, and the preview host (`previewHost.ts`), which answers
 * these and nothing else.
 *
 * NOTHING ARRIVING ON THE PORT IS TRUSTED: every parameter is checked here
 * before it is asked, and the engine decides each read again against the
 * viewer's share and the collection's rules.
 */

import { QueryError, type DataDoc, type LiveSocket } from "~/protocol/index.ts";
import { STATE_COLLECTION } from "~/contract/artifacts.ts";
import type { BridgeDoc, BridgeErrorCode } from "~/runtime/bridge.ts";

/** A refusal a call is answered with. */
export class Refusal extends Error {
  constructor(
    readonly code: BridgeErrorCode,
    message: string,
  ) {
    super(message);
  }
}

/** The engine's query grammar's operators, and the page contract's names for the two it spells longer. */
const QUERY_OPS: Readonly<Record<string, string>> = {
  eq: "eq",
  ne: "ne",
  lt: "lt",
  lte: "le",
  gt: "gt",
  gte: "ge",
  in: "in",
  contains: "contains",
  prefix: "prefix",
};

/** A page's query (`{where: [[field, op, value]], order: [[field, dir]], limit, cursor}`) in the engine's grammar. */
export function engineQuery(q: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const where = q["where"];
  if (where !== undefined) {
    if (!Array.isArray(where))
      throw new Refusal("invalid", "where is a list of [field, op, value]");
    out["where"] = where.map((clause) => {
      if (!Array.isArray(clause) || clause.length !== 3 || typeof clause[0] !== "string") {
        throw new Refusal("invalid", "each where clause is [field, op, value]");
      }
      const op = typeof clause[1] === "string" ? QUERY_OPS[clause[1]] : undefined;
      if (!op) {
        throw new Refusal(
          "invalid",
          `op ${String(clause[1])} is not one of ${Object.keys(QUERY_OPS).join(", ")}`,
        );
      }
      if (op === "in") {
        if (!Array.isArray(clause[2]))
          throw new Refusal("invalid", "an `in` clause takes a list of values");
        return { path: clause[0], op, values: clause[2] };
      }
      return { path: clause[0], op, value: clause[2] };
    });
  }
  const order = q["order"];
  if (order !== undefined) {
    // ONE FIELD, as the engine's field index orders by; the document id is
    // always the tie-break, so the order is total and a cursor exact.
    if (!Array.isArray(order) || order.length > 1) {
      throw new Refusal("invalid", 'order names one [field, "asc" | "desc"] in contract 1.0');
    }
    const [first] = order as unknown[];
    if (first !== undefined) {
      if (
        !Array.isArray(first) ||
        typeof first[0] !== "string" ||
        (first[1] !== "asc" && first[1] !== "desc")
      ) {
        throw new Refusal("invalid", 'order names one [field, "asc" | "desc"]');
      }
      out["order_by"] = { path: first[0], ...(first[1] === "desc" ? { desc: true } : {}) };
    }
  }
  if (q["limit"] !== undefined) {
    if (typeof q["limit"] !== "number" || !Number.isInteger(q["limit"])) {
      throw new Refusal("invalid", "limit is a whole number");
    }
    out["limit"] = q["limit"];
  }
  if (q["cursor"] !== undefined) {
    if (typeof q["cursor"] !== "string")
      throw new Refusal("invalid", "cursor is the one an answer gave");
    out["after"] = q["cursor"];
  }
  return out;
}

/** A document as a page reads it. */
export function bridgeDoc(
  doc: DataDoc,
  fields: Record<string, unknown> = doc.fields ?? {},
  pending = false,
): BridgeDoc {
  return {
    id: doc.id,
    data: fields,
    version: doc.version,
    createdBy: doc.created_by.name,
    createdAt: doc.created_at,
    updatedBy: doc.updated_by.name,
    updatedAt: doc.updated_at,
    pending,
  };
}

/** A query error as the refusal a page is answered with. */
export function refusalOf(err: unknown): Refusal {
  if (err instanceof Refusal) return err;
  if (err instanceof QueryError) {
    switch (err.message) {
      case "not_found":
        return new Refusal("not_found", "there is no such collection, document or asset");
      case "unauthorized":
        return new Refusal("forbidden", "your share does not allow this");
      case "bad_params":
        return new Refusal("invalid", err.detail ?? "the call's arguments were refused");
      default:
        return new Refusal("unavailable", "the engine could not be reached; nothing changed");
    }
  }
  return new Refusal("unavailable", "the engine could not be reached; nothing changed");
}

export const str = (v: unknown, what: string): string => {
  if (typeof v !== "string" || v === "")
    throw new Refusal("invalid", `${what} is a non-empty string`);
  return v;
};

/** What a read is made with: the artifact the frame is bound to, and the viewer's socket. */
export interface ReadContext {
  artifact: string;
  socket: LiveSocket;
}

/** The read methods every host answers the same way. */
export type ReadMethod = "data.get" | "data.list" | "data.query";

/** Answers one read: a document (null when there is none), or a page of documents. */
export async function readData(
  method: ReadMethod,
  p: Record<string, unknown>,
  { artifact, socket }: ReadContext,
): Promise<unknown> {
  switch (method) {
    case "data.get": {
      try {
        const doc = await socket.query("artifact_data_doc", {
          id: artifact,
          collection: str(p["collection"], "collection"),
          doc: str(p["id"], "id"),
        });
        return doc.deleted ? null : bridgeDoc(doc);
      } catch (err) {
        if (err instanceof QueryError && err.message === "not_found") return null;
        throw err;
      }
    }
    case "data.list": {
      const page = await socket.query("artifact_data", {
        id: artifact,
        collection: str(p["collection"], "collection"),
        ...(typeof p["cursor"] === "string" ? { cursor: p["cursor"] } : {}),
        ...(typeof p["limit"] === "number" ? { limit: p["limit"] } : {}),
      });
      return {
        docs: page.docs.filter((d) => !d.deleted).map((d) => bridgeDoc(d)),
        ...(page.after ? { cursor: page.after } : {}),
      };
    }
    case "data.query": {
      const q = p["query"];
      if (typeof q !== "object" || q === null || Array.isArray(q))
        throw new Refusal("invalid", "query is an object");
      const page = await socket.query("artifact_data_query", {
        id: artifact,
        collection: str(p["collection"], "collection"),
        q: JSON.stringify(engineQuery(q as Record<string, unknown>)),
      });
      return {
        docs: page.docs.filter((d) => !d.deleted).map((d) => bridgeDoc(d)),
        ...(page.after ? { cursor: page.after } : {}),
      };
    }
  }
}

/**
 * The collections a page may read, from the engine's schema question: every
 * one but the private per-viewer state collection, which only `state.*`
 * reaches.
 */
export async function readSchema({ artifact, socket }: ReadContext): Promise<unknown[]> {
  const schema = await socket.query("artifact_data_schema", { id: artifact });
  return schema.collections.filter((c) => c.name !== STATE_COLLECTION);
}

/** The artifact's asset store as a page lists it. */
export async function readAssets({ artifact, socket }: ReadContext): Promise<unknown[]> {
  const answer = await socket.query("artifact_assets", { id: artifact });
  return answer.assets.map((a) => ({
    id: a.id,
    name: a.name,
    contentType: a.content_type,
    size: a.object.size,
  }));
}
