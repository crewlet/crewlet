/**
 * The page bridge's wire, written once for both ends: the runtime a page runs
 * (`runtime/runtime.ts`, the third build target, served on the artifact
 * origin) and the host that embedded it (the dashboard's artifact viewer).
 *
 * BELOW THIS, THE HANDSHAKE IS THE DESIGN SYSTEM'S (`@crewlethq/ui`'s
 * `FrameProtocol`): the page says hello carrying the nonce its frame was
 * minted with, and the host transfers one `MessagePort` to the window it
 * checked. Everything here travels on that port, which no other window can
 * address, and the host binds every call to the one artifact and version it
 * embedded: a page never names its own artifact, and never holds a
 * credential.
 *
 * EVERYTHING ARRIVING FROM THE OTHER END IS UNTRUSTED. Each reader returns
 * `null` for anything that is not exactly the shape it names; nothing is
 * cast. A value inside a document is preserved as the JSON it is (ADR-0006);
 * only the envelope is held to a shape.
 *
 * No React and no DOM at module scope: the runtime build imports this, and it
 * is small enough to sit in a page before any of the page's own scripts.
 */

// RELATIVE, like every contract import outside the app: this module is also
// built into the runtime alone, where the `~` alias does not exist.
import { ARTIFACT_CAPABILITIES, ARTIFACT_ROLES } from "../contract/artifacts.ts";
import {
  ARTIFACT_COMMENT_ANCHOR_NAME,
  ARTIFACT_COMMENT_CROP_MAX_BYTES,
  ARTIFACT_COMMENT_QUOTE_MAX_BYTES,
} from "../contract/artifactsworkspace.ts";
import { PRESENCE_ACTIVITIES } from "../contract/presence.ts";

/** The bridge's message names on the port, beside the handshake's. */
export const BRIDGE_MESSAGE = {
  /** Host to page, first on the port: what the page may do and who is viewing. */
  init: "bridge.init",
  /** Page to host: one call. */
  call: "bridge.call",
  /** Host to page: one call's answer. */
  result: "bridge.result",
  /** Host to page: something the page subscribed to moved. */
  event: "bridge.event",
} as const;

/** The capabilities a version declares, as the runtime names them (`artifacts.Capability`). */
export type RuntimeCapability = (typeof ARTIFACT_CAPABILITIES)[number];

/** Every method a page may call, and the capability each needs (none for `openLink`, `print` and
 *  the comment layer's four, which only report what the page already draws). */
export const BRIDGE_METHODS = {
  "identity.me": "identity",
  "identity.people": "identity",
  "data.schema": "db",
  "data.get": "db",
  "data.list": "db",
  "data.query": "db",
  "data.subscribe": "db",
  "data.unsubscribe": "db",
  "data.write": "db",
  "data.undo": "db",
  "data.redo": "db",
  "state.get": "state",
  "state.set": "state",
  "state.delete": "state",
  "state.list": "state",
  "presence.set": "presence",
  "assets.list": "assets",
  openLink: null,
  print: null,
  "comments.layout": null,
  "comments.pick": null,
  "comments.crop": null,
  "comments.escape": null,
} as const satisfies Record<string, RuntimeCapability | null>;

export type BridgeMethod = keyof typeof BRIDGE_METHODS;

/** The refusal codes a call rejects with. A page treats an unknown code as a generic failure. */
export type BridgeErrorCode =
  | "capability_not_declared"
  | "forbidden"
  | "not_found"
  | "invalid"
  | "rate_limited"
  | "unavailable";

/** An agent's face, as the chart draws it: one of the character family and its node hue. */
export interface BridgeAvatar {
  character: string;
  hue: string;
}

/**
 * Somebody a page may name, as the chart the viewer's tab holds says them:
 * display facts only, never a token, a login or an address.
 */
export interface BridgePerson {
  /** The seat handle: the value a person column stores. */
  seat: string;
  kind: "human" | "agent";
  displayName: string;
  /** An agent's face; a person has none. */
  avatar?: BridgeAvatar;
}

/** Who is viewing, as a page is told. */
export interface BridgeViewer extends BridgePerson {
  role: (typeof ARTIFACT_ROLES)[number];
}

/** How the host's link to the engine stands. */
export type BridgeConnection = "live" | "reconnecting" | "offline";

/** Host to page, first: the contract, what the version declared, who is viewing and how the link stands. */
export interface BridgeInit {
  type: typeof BRIDGE_MESSAGE.init;
  contract: string;
  capabilities: RuntimeCapability[];
  viewer: BridgeViewer | null;
  connection: BridgeConnection;
}

/** One document as a page reads it. */
export interface BridgeDoc {
  id: string;
  data: Record<string, unknown>;
  version: number;
  createdBy: string;
  createdAt: string;
  updatedBy: string;
  updatedAt: string;
  /** A write this page made on it is not decided yet. */
  pending: boolean;
}

/** Page to host: one call. */
export interface BridgeCall {
  type: typeof BRIDGE_MESSAGE.call;
  id: number;
  method: BridgeMethod;
  params: Record<string, unknown>;
}

/** Host to page: one call's answer. */
export type BridgeResult =
  | { type: typeof BRIDGE_MESSAGE.result; id: number; ok: true; value: unknown }
  | {
      type: typeof BRIDGE_MESSAGE.result;
      id: number;
      ok: false;
      error: { code: BridgeErrorCode; message: string };
    };

/** Host to page: a subscription moved. */
export type BridgeEvent =
  | {
      type: typeof BRIDGE_MESSAGE.event;
      topic: "data";
      subscription: number;
      upsert: BridgeDoc[];
      remove: string[];
    }
  | {
      type: typeof BRIDGE_MESSAGE.event;
      topic: "presence";
      roster: BridgeRoster;
    }
  | { type: typeof BRIDGE_MESSAGE.event; topic: "connection"; state: BridgeConnection }
  | { type: typeof BRIDGE_MESSAGE.event; topic: "identity"; viewer: BridgeViewer | null }
  | ({ type: typeof BRIDGE_MESSAGE.event; topic: "comments" } & CommentRequest);

// ---- the comment layer -------------------------------------------------------

/** How the host draws comments over the page: not at all, its pins, or a reader placing one. */
export type CommentMode = "off" | "show" | "place";

const COMMENT_MODES: readonly CommentMode[] = ["off", "show", "place"];

/** A name a page declares a comment anchor by (`commentanchor.NamePattern`). */
export const COMMENT_ANCHOR_NAME = new RegExp(ARTIFACT_COMMENT_ANCHOR_NAME);

/** The most of an anchor's words a pick carries (`artifacts.MaxCommentQuoteBytes`). */

/** The largest picture of a part a page answers (`artifacts.MaxCommentCropBytes`). */

/** The longest side of that picture, in pixels (`artifacts.MaxCommentCropSide`). */

/** The most anchors one layout reports: far past any page a reader comments on part by part. */
export const COMMENT_LAYOUT_MAX_ANCHORS = 2000;

/** A point on the page, as fractions (0..1) of its document's width and height. */
export interface CommentPoint {
  x: number;
  y: number;
}

/** What the host asks about: a declared anchor, or a point. */
export type CommentTarget = { name: string } | { point: CommentPoint };

/** Host to page, on the `comments` topic: the mode, a part to bring into view, or a part to draw. */
export type CommentRequest =
  { mode: CommentMode } | { reveal: CommentTarget } | { capture: { id: number } & CommentTarget };

/** One declared anchor as the page draws it, in CSS pixels of its document. */
export interface CommentAnchorBox {
  name: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

/** Page to host (`comments.layout`): where its anchors are, and how much of the document is in view. */
export interface CommentLayout {
  anchors: CommentAnchorBox[];
  doc: { w: number; h: number };
  scroll: { x: number; y: number };
  viewport: { w: number; h: number };
  /** While placing: the innermost anchor under the reader's pointer, which the host outlines. */
  hover?: string;
}

/** Page to host (`comments.pick`): a click while placing, on an anchor or a point. */
export interface CommentPick {
  name: string | null;
  /** The anchor's words, at most [ARTIFACT_COMMENT_QUOTE_MAX_BYTES]; empty on a point. */
  quote: string;
  point: CommentPoint;
}

/** Page to host (`comments.crop`): the picture asked for by `capture`, or null where none was drawn. */
export interface CommentCrop {
  id: number;
  image: string | null;
}

/** One member of the room, as a page reads it: the session, and the person the chart says holds it. */
export interface BridgeMember extends BridgePerson {
  session: string;
  activity: (typeof PRESENCE_ACTIVITIES)[number];
  focus: { version?: number; region?: string; collection?: string; doc?: string; field?: string };
  at: string;
}

/** Who is in the room. */
export interface BridgeRoster {
  members: BridgeMember[];
  counts?: { human: number; agent: number };
  truncated: boolean;
}

/** A write's outcome, as a page handles it: an answer, not an exception, for every verdict the engine decided. */
export interface BridgeWriteOutcome {
  verdict: "applied" | "conflict" | "invalid" | "forbidden" | "quota" | "gone" | "pending";
  seq?: number;
  reason?: string;
  conflict?: BridgeDoc[];
}

// ---- readers -------------------------------------------------------------

const record = (data: unknown): Record<string, unknown> | null =>
  typeof data === "object" && data !== null && !Array.isArray(data)
    ? (data as Record<string, unknown>)
    : null;

const isCapability = (v: unknown): v is RuntimeCapability =>
  typeof v === "string" && (ARTIFACT_CAPABILITIES as readonly string[]).includes(v);

/**
 * The capabilities the artifact origin wrote on the runtime's own script
 * element (`data-capabilities`, space separated): the frame's grant, known from
 * the page's first script, before the host's handshake says it again. Only the
 * names a version may declare are read; anything else in the attribute is
 * left out.
 */
export function readDeclaredCapabilities(attribute: string | null): RuntimeCapability[] {
  if (!attribute) return [];
  return [...new Set(attribute.split(/\s+/).filter(isCapability))];
}

const isMethod = (v: unknown): v is BridgeMethod =>
  typeof v === "string" && Object.hasOwn(BRIDGE_METHODS, v);

const CONNECTIONS: readonly BridgeConnection[] = ["live", "reconnecting", "offline"];

/** The longest call id either end accepts, so a counter is never a float. */
const MAX_CALL_ID = Number.MAX_SAFE_INTEGER;

/** Reads a call, or null for anything that is not exactly one. */
export function readCall(data: unknown): BridgeCall | null {
  const m = record(data);
  if (!m || m["type"] !== BRIDGE_MESSAGE.call) return null;
  const { id, method } = m;
  const params = m["params"] === undefined ? {} : record(m["params"]);
  if (typeof id !== "number" || !Number.isInteger(id) || id < 1 || id > MAX_CALL_ID) return null;
  if (!isMethod(method) || !params) return null;
  return { type: BRIDGE_MESSAGE.call, id, method, params };
}

/** Reads a person, or null. An avatar that is not exactly one is left off rather than refusing the person. */
export function readPerson(data: unknown): BridgePerson | null {
  const v = record(data);
  if (!v) return null;
  const { seat, kind, displayName } = v;
  if (typeof seat !== "string" || typeof displayName !== "string") return null;
  if (kind !== "human" && kind !== "agent") return null;
  const person: BridgePerson = { seat, kind, displayName };
  const avatar = record(v["avatar"]);
  if (avatar && typeof avatar["character"] === "string" && typeof avatar["hue"] === "string") {
    person.avatar = { character: avatar["character"], hue: avatar["hue"] };
  }
  return person;
}

/** Reads a viewer, or null. */
export function readViewer(data: unknown): BridgeViewer | null {
  const person = readPerson(data);
  const role = record(data)?.["role"];
  if (!person) return null;
  if (typeof role !== "string" || !(ARTIFACT_ROLES as readonly string[]).includes(role))
    return null;
  return { ...person, role: role as BridgeViewer["role"] };
}

/** Reads the init message, or null. */
export function readInit(data: unknown): BridgeInit | null {
  const m = record(data);
  if (!m || m["type"] !== BRIDGE_MESSAGE.init) return null;
  const { contract, capabilities, connection } = m;
  if (typeof contract !== "string" || !Array.isArray(capabilities)) return null;
  if (!capabilities.every(isCapability)) return null;
  if (typeof connection !== "string" || !(CONNECTIONS as readonly string[]).includes(connection))
    return null;
  const viewer = m["viewer"] === null ? null : readViewer(m["viewer"]);
  if (m["viewer"] !== null && viewer === null) return null;
  return {
    type: BRIDGE_MESSAGE.init,
    contract,
    capabilities: [...capabilities],
    viewer,
    connection: connection as BridgeConnection,
  };
}

/** Reads a call's answer, or null. */
export function readResult(data: unknown): BridgeResult | null {
  const m = record(data);
  if (!m || m["type"] !== BRIDGE_MESSAGE.result) return null;
  const { id, ok } = m;
  if (typeof id !== "number" || !Number.isInteger(id)) return null;
  if (ok === true) return { type: BRIDGE_MESSAGE.result, id, ok: true, value: m["value"] };
  const error = record(m["error"]);
  if (ok !== false || !error) return null;
  const { code, message } = error;
  if (typeof code !== "string" || typeof message !== "string") return null;
  return {
    type: BRIDGE_MESSAGE.result,
    id,
    ok: false,
    error: { code: code as BridgeErrorCode, message },
  };
}

/** Reads one document a page is handed, or null. */
export function readDoc(data: unknown): BridgeDoc | null {
  const d = record(data);
  if (!d) return null;
  const fields = record(d["data"]);
  const { id, version, createdBy, createdAt, updatedBy, updatedAt, pending } = d;
  if (typeof id !== "string" || !fields || typeof version !== "number") return null;
  if (typeof createdBy !== "string" || typeof createdAt !== "string") return null;
  if (typeof updatedBy !== "string" || typeof updatedAt !== "string") return null;
  return {
    id,
    data: fields,
    version,
    createdBy,
    createdAt,
    updatedBy,
    updatedAt,
    pending: pending === true,
  };
}

/** Reads a member of the room, or null. */
function readMember(data: unknown): BridgeMember | null {
  const m = record(data);
  const person = readPerson(data);
  if (!m || !person) return null;
  const { session, activity, at } = m;
  const focus = record(m["focus"]) ?? {};
  if (typeof session !== "string" || typeof at !== "string") return null;
  if (
    typeof activity !== "string" ||
    !(PRESENCE_ACTIVITIES as readonly string[]).includes(activity)
  ) {
    return null;
  }
  const out: BridgeMember["focus"] = {};
  if (typeof focus["version"] === "number") out.version = focus["version"];
  for (const key of ["region", "collection", "doc", "field"] as const) {
    if (typeof focus[key] === "string") out[key] = focus[key];
  }
  return { ...person, session, activity: activity as BridgeMember["activity"], focus: out, at };
}

/** Reads an event, or null. */
export function readEvent(data: unknown): BridgeEvent | null {
  const m = record(data);
  if (!m || m["type"] !== BRIDGE_MESSAGE.event) return null;
  switch (m["topic"]) {
    case "data": {
      const { subscription, upsert, remove } = m;
      if (typeof subscription !== "number" || !Array.isArray(upsert) || !Array.isArray(remove))
        return null;
      const docs = upsert.map(readDoc);
      if (docs.some((d) => d === null) || !remove.every((r) => typeof r === "string")) return null;
      return {
        type: BRIDGE_MESSAGE.event,
        topic: "data",
        subscription,
        upsert: docs as BridgeDoc[],
        remove: [...(remove as string[])],
      };
    }
    case "presence": {
      const roster = record(m["roster"]);
      if (!roster || !Array.isArray(roster["members"])) return null;
      const members = roster["members"].map(readMember);
      if (members.some((x) => x === null)) return null;
      const counts = record(roster["counts"]);
      const out: BridgeRoster = {
        members: members as BridgeMember[],
        truncated: roster["truncated"] === true,
      };
      if (counts && typeof counts["human"] === "number" && typeof counts["agent"] === "number") {
        out.counts = { human: counts["human"], agent: counts["agent"] };
      }
      return { type: BRIDGE_MESSAGE.event, topic: "presence", roster: out };
    }
    case "connection": {
      const state = m["state"];
      if (typeof state !== "string" || !(CONNECTIONS as readonly string[]).includes(state))
        return null;
      return { type: BRIDGE_MESSAGE.event, topic: "connection", state: state as BridgeConnection };
    }
    case "identity": {
      const viewer = m["viewer"] === null ? null : readViewer(m["viewer"]);
      if (m["viewer"] !== null && viewer === null) return null;
      return { type: BRIDGE_MESSAGE.event, topic: "identity", viewer };
    }
    case "comments": {
      const request = readCommentRequest(m);
      return request ? { type: BRIDGE_MESSAGE.event, topic: "comments", ...request } : null;
    }
  }
  return null;
}

// ---- the comment layer's readers -------------------------------------------

/** Whether a value is a name a page may declare. */
export function isAnchorName(v: unknown): v is string {
  return typeof v === "string" && COMMENT_ANCHOR_NAME.test(v);
}

const finite = (v: unknown): v is number => typeof v === "number" && Number.isFinite(v);

/** A fraction of the document, held to 0..1 rather than refused: a click on the edge is still on the page. */
const fraction = (v: number) => Math.min(1, Math.max(0, v));

/** Reads a point as fractions of the document, clamped to it, or null. */
export function readCommentPoint(data: unknown): CommentPoint | null {
  const p = record(data);
  if (!p || !finite(p["x"]) || !finite(p["y"])) return null;
  return { x: fraction(p["x"]), y: fraction(p["y"]) };
}

/** Reads what a request names: a declared name, or a point. */
function readTarget(data: Record<string, unknown>): CommentTarget | null {
  if (data["name"] !== undefined) return isAnchorName(data["name"]) ? { name: data["name"] } : null;
  const point = readCommentPoint(data["point"]);
  return point ? { point } : null;
}

/** Reads the host's request on the `comments` topic, or null. */
export function readCommentRequest(data: Record<string, unknown>): CommentRequest | null {
  if (data["mode"] !== undefined) {
    const mode = data["mode"];
    return typeof mode === "string" && (COMMENT_MODES as readonly string[]).includes(mode)
      ? { mode: mode as CommentMode }
      : null;
  }
  const reveal = record(data["reveal"]);
  if (reveal) {
    const target = readTarget(reveal);
    return target ? { reveal: target } : null;
  }
  const capture = record(data["capture"]);
  if (capture) {
    const id = capture["id"];
    const target = readTarget(capture);
    if (typeof id !== "number" || !Number.isInteger(id) || id < 1 || !target) return null;
    return { capture: { id, ...target } };
  }
  return null;
}

/** The UTF-8 length of a string, as the engine counts a quote. */
export function utf8Length(s: string): number {
  return new TextEncoder().encode(s).length;
}

/** A page's layout, or null for anything that is not exactly one. */
export function readCommentLayout(data: unknown): CommentLayout | null {
  const m = record(data);
  if (!m || !Array.isArray(m["anchors"])) return null;
  const doc = record(m["doc"]);
  const scroll = record(m["scroll"]);
  const viewport = record(m["viewport"]);
  if (!doc || !scroll || !viewport) return null;
  if (!finite(doc["w"]) || !finite(doc["h"]) || doc["w"] <= 0 || doc["h"] <= 0) return null;
  if (!finite(scroll["x"]) || !finite(scroll["y"])) return null;
  if (!finite(viewport["w"]) || !finite(viewport["h"]) || viewport["w"] < 0 || viewport["h"] < 0)
    return null;
  if (m["anchors"].length > COMMENT_LAYOUT_MAX_ANCHORS) return null;
  const anchors: CommentAnchorBox[] = [];
  for (const raw of m["anchors"]) {
    const a = record(raw);
    if (!a || !isAnchorName(a["name"])) return null;
    const { x, y, w, h } = a;
    if (!finite(x) || !finite(y) || !finite(w) || !finite(h) || w < 0 || h < 0) return null;
    anchors.push({ name: a["name"], x, y, w, h });
  }
  const out: CommentLayout = {
    anchors,
    doc: { w: doc["w"], h: doc["h"] },
    scroll: { x: scroll["x"], y: scroll["y"] },
    viewport: { w: viewport["w"], h: viewport["h"] },
  };
  if (m["hover"] !== undefined && m["hover"] !== null) {
    if (!isAnchorName(m["hover"])) return null;
    out.hover = m["hover"];
  }
  return out;
}

/** A pick, or null. A quote past the bound is refused, never cut here: the page cuts its own. */
export function readCommentPick(data: unknown): CommentPick | null {
  const m = record(data);
  if (!m) return null;
  const name = m["name"] ?? null;
  if (name !== null && !isAnchorName(name)) return null;
  const quote = m["quote"] ?? "";
  if (typeof quote !== "string" || utf8Length(quote) > ARTIFACT_COMMENT_QUOTE_MAX_BYTES)
    return null;
  const point = readCommentPoint(m["point"]);
  if (!point) return null;
  return { name, quote, point };
}

/** The picture types a crop may be, as the engine reads them by their bytes. */
const CROP_IMAGE = /^data:image\/(?:png|jpeg|webp);base64,([A-Za-z0-9+/]+={0,2})$/;

/** Whether a data URL is a picture the engine would keep: its type, and its bytes within the bound. */
export function isCropImage(v: unknown): v is string {
  if (typeof v !== "string") return false;
  const m = CROP_IMAGE.exec(v);
  if (!m) return false;
  const b64 = m[1]!;
  const bytes = (b64.length * 3) / 4 - (b64.endsWith("==") ? 2 : b64.endsWith("=") ? 1 : 0);
  return bytes <= ARTIFACT_COMMENT_CROP_MAX_BYTES;
}

/** A crop's answer, or null; a picture the engine would not keep reads as none, so nobody waits. */
export function readCommentCrop(data: unknown): CommentCrop | null {
  const m = record(data);
  const id = m?.["id"];
  if (!m || typeof id !== "number" || !Number.isInteger(id) || id < 1) return null;
  return { id, image: isCropImage(m["image"]) ? m["image"] : null };
}

/** The capability a method needs, or null for one every page may call. */
export function capabilityOf(method: BridgeMethod): RuntimeCapability | null {
  return BRIDGE_METHODS[method];
}
