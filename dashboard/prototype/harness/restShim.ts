/**
 * `window.fetch`, answered by the demo engine for every path the dashboard
 * calls.
 *
 * The dashboard reaches the engine over REST through one transport
 * (`src/protocol/rest.ts`, plus the degraded snapshot in `api.ts`), which
 * calls `fetch(location.origin + path)` with a bearer token and a JSON body.
 * This shim takes that call, hands the method, path, query, headers and body
 * to [DemoEngine.rest], and returns its answer as a real `Response`, so the
 * transport's own status, entity-tag, `Retry-After` and refusal handling all
 * run as they do against an engine.
 *
 * A path the engine does not serve is answered as the engine's router
 * answers one, a 404 with its own `no_route` code, and logged to the console
 * naming the path. A request for another origin is passed to the browser's
 * own fetch untouched.
 */

import { ANSWER_MS } from "./demoClient.ts";
import type { DemoEngine } from "./engine/engine.ts";

/** The path of a URL the dashboard addressed to its engine, or null for any other. */
function enginePath(url: string): string | null {
  // `location.origin` is the string "null" in an opaque-origin frame, and the
  // transport prefixes it all the same: "null/config" is the engine's /config.
  if (url.startsWith(location.origin)) return url.slice(location.origin.length) || "/";
  if (url.startsWith("/")) return url;
  return null;
}

/** Wait as the network would, giving up as an aborted request does. */
function settle(signal: AbortSignal | null | undefined): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const timer = setTimeout(resolve, ANSWER_MS);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        reject(signal.reason);
      },
      { once: true },
    );
  });
}

/**
 * Answer the dashboard's REST calls from `engine` until the returned function
 * is called, which puts the browser's own fetch back.
 */
export function installRestShim(engine: DemoEngine): () => void {
  const original = globalThis.fetch;
  const shim: typeof fetch = async (input, init) => {
    const request = input instanceof Request ? input : null;
    const url = request ? request.url : input instanceof URL ? input.href : String(input);
    const address = enginePath(url);
    if (address === null) return original(input, init);

    const method = (init?.method ?? request?.method ?? "GET").toUpperCase();
    const headers = new Headers(init?.headers ?? request?.headers);
    const body = typeof init?.body === "string" ? init.body : request ? await request.text() : null;
    await settle(init?.signal ?? request?.signal);

    const at = address.indexOf("?");
    const path = at < 0 ? address : address.slice(0, at);
    const query = new URLSearchParams(at < 0 ? "" : address.slice(at + 1));
    const reply = engine.rest({ method, path, query, headers, body });
    const out = new Headers(reply.headers ?? {});
    let payload: string | null = null;
    if (reply.text !== undefined) {
      payload = reply.text;
      if (!out.has("Content-Type")) out.set("Content-Type", "text/plain; charset=utf-8");
    } else if (reply.body !== undefined) {
      payload = JSON.stringify(reply.body);
      out.set("Content-Type", "application/json");
    }
    // A 204 and a 304 carry no body, and a Response refuses one.
    const empty = reply.status === 204 || reply.status === 304;
    return new Response(empty ? null : payload, { status: reply.status, headers: out });
  };
  globalThis.fetch = shim;
  return () => {
    globalThis.fetch = original;
  };
}
