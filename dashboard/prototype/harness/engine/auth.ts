/**
 * The sign-in surface, `/auth`, and the unguarded `/health`, as the demo
 * engine answers them (`internal/api/authapi`, `internal/api/health.go`).
 *
 * WHO THE BROWSER IS IS THE COMPANY'S PRINCIPAL (`engine/principal.ts`), so
 * `GET /auth/session` and the `viewer` question can never disagree about the
 * reader. The gestures that would change who the browser is (a sign-in, a
 * sign-out, a token exchange, a second factor) are refused out loud: the demo
 * holds one reader, chosen by the scenario, and a sign-out that reloaded into
 * a sign-in form nothing could complete would leave the prototype unusable.
 */

import type { AuthConfig } from "~/protocol/types.ts";
import type { DemoEngine } from "./engine.ts";
import { sessionOf } from "./principal.ts";
import { LOG, notModelled } from "./refusal.ts";
import type { RestReply, RestRequest } from "./rest.ts";

/** The password floor (`iam.MinPasswordChars`) and the second-factor posture's default. */
const AUTH_CONFIG: AuthConfig = { min_password_length: 12, second_factor: "required" };

/** Whether `path` is one this module answers. */
export function isAuthPath(path: string): boolean {
  return path === "/health" || path === "/auth" || path.startsWith("/auth/");
}

export function handleAuth(engine: DemoEngine, request: RestRequest): RestReply {
  const { method, path } = request;
  if (method === "GET" && path === "/health") return { status: 200, body: engine.health() };
  if (method === "GET" && path === "/auth/session") {
    return { status: 200, body: sessionOf(engine.company, engine.now()) };
  }
  if (method === "GET" && path === "/auth/config") return { status: 200, body: AUTH_CONFIG };
  console.warn(LOG, `${method} ${path} is not modelled; it was refused.`);
  return {
    status: 409,
    body: {
      error: "not_modelled",
      detail: notModelled("Signing in, signing out and changing how you sign in"),
    },
  };
}
