/**
 * Every REST path the dashboard reaches, routed to the part of the demo
 * engine that answers it.
 *
 * The paths are the ones `vite.config.ts` proxies to a running engine, which
 * `src/protocol/proxy.test.ts` holds to exactly what the source calls: the act
 * transport, the sign-in surface and `/health`, the identity directory, the
 * configuration surface, the setup surface, the sealed store,
 * the degraded snapshot, and the four operator gestures (a backup, a broker
 * member removed, a retention gate, a file download). A path outside them is
 * answered as the engine's own router answers one, and logged.
 */

import type { DemoEngine } from "./engine.ts";
import { handleAct } from "./acts.ts";
import { handleArtifacts } from "./artifacts.ts";
import { handleAuth, isAuthPath } from "./auth.ts";
import { handleIam, isIamPath } from "./iam.ts";
import { handleConfig } from "./config.ts";
import { handleSetup } from "./setup.ts";
import { actorOf } from "./principal.ts";
import { seal } from "./refs.ts";
import { DemoRefusal, LOG, notModelled } from "./refusal.ts";

/** One request, as the fetch shim hands it over. */
export interface RestRequest {
  method: string;
  /** The path alone, without its query. */
  path: string;
  query: URLSearchParams;
  headers: Headers;
  /** The body as sent; null for none. */
  body: string | null;
}

/** One answer: a status and either a JSON body or text, plus the headers that matter. */
export interface RestReply {
  status: number;
  body?: unknown;
  text?: string;
  headers?: Record<string, string>;
}

/** An operator gesture the demo does not perform, refused in a sentence. */
function gesture(what: string, request: RestRequest): RestReply {
  console.warn(LOG, `${request.method} ${request.path} is not modelled; it was refused.`);
  return { status: 409, body: { error: "not_modelled", detail: notModelled(what) } };
}

function secrets(engine: DemoEngine, request: RestRequest): RestReply {
  const c = engine.company;
  if (request.path === "/secrets") {
    return request.method === "GET"
      ? { status: 200, body: { secrets: c.secrets } }
      : { status: 405, body: { error: "method_not_allowed" } };
  }
  const name = decodeURIComponent(request.path.slice("/secrets/".length));
  if (!/^[A-Z][A-Z0-9_]*$/.test(name)) {
    return {
      status: 400,
      body: {
        error: "invalid_name",
        detail: `"${name}" is not a secret name: use capitals, digits and underscores.`,
      },
    };
  }
  if (request.method === "PUT") {
    return {
      status: 200,
      body: seal(c, name, engine.now().toISOString(), actorOf(c), "dashboard"),
    };
  }
  if (request.method === "DELETE") {
    const before = c.secrets.length;
    c.secrets = c.secrets.filter((s) => s.name !== name);
    return before === c.secrets.length
      ? { status: 404, body: { error: "not_found", detail: `There is no secret ${name}.` } }
      : { status: 204 };
  }
  return { status: 405, body: { error: "method_not_allowed" } };
}

/**
 * Route one request, and tell whoever keeps the company that it changed when
 * a write succeeded: ONE place, so no modelled write can forget to be kept.
 */
export function handleRest(engine: DemoEngine, request: RestRequest): RestReply {
  const reply = route(engine, request);
  if (request.method !== "GET" && reply.status < 400) engine.committed();
  return reply;
}

function route(engine: DemoEngine, request: RestRequest): RestReply {
  const { path, method } = request;
  if (path.startsWith("/operator/act/")) {
    return handleAct(engine, decodeURIComponent(path.slice("/operator/act/".length)), request);
  }
  if (isAuthPath(path)) return handleAuth(engine, request);
  if (isIamPath(path)) return handleIam(engine, request);
  if (path === "/config" || path.startsWith("/config/")) return handleConfig(engine, request);
  if (path === "/setup/integrations" || path.startsWith("/setup/integrations/"))
    return handleSetup(engine, request);
  if (path === "/secrets" || path.startsWith("/secrets/")) return secrets(engine, request);
  if (path === "/artifacts" || path.startsWith("/artifacts/")) {
    // A READ THE ENGINE REFUSES is answered as its router answers one, not thrown.
    try {
      return handleArtifacts(engine, request);
    } catch (err) {
      if (!(err instanceof DemoRefusal)) throw err;
      return { status: 404, body: { error: err.code, detail: err.detail ?? "" } };
    }
  }
  if (path === "/stream/snapshot" && method === "GET")
    return { status: 200, body: engine.snapshot() };
  // The socket's refusal probe: a plain GET of the socket's path answers 426
  // for an accepted credential.
  if (path === "/ws/stream") return { status: 426, body: { error: "upgrade_required" } };
  if (path === "/backup" && method === "POST") return gesture("Taking a backup", request);
  if (path.startsWith("/fleet/broker/") && method === "POST")
    return gesture("Removing a broker member", request);
  if (path.startsWith("/work/retention/") && method === "POST")
    return gesture("A retention gate", request);
  if (path.startsWith("/work/files/") && method === "GET") {
    const address = decodeURIComponent(path.slice("/work/files/".length));
    const file = engine.company.work.files.find((f) => `${f.project}/${f.path}` === address);
    return file
      ? {
          status: 200,
          text: `${file.path} (a demo file: its bytes are not part of the prototype)\n`,
          headers: { "Content-Type": file.content_type ?? "text/plain" },
        }
      : { status: 404, body: { error: "not_found", detail: `There is no file ${address}.` } };
  }
  console.warn(LOG, `${method} ${path} is not a path the demo engine serves; it was answered 404.`);
  return {
    status: 404,
    body: { error: "no_route", detail: `${method} ${path} is not served by the demo engine.` },
  };
}
