/**
 * The chat screen's suites, against the engine as the dashboard reaches it.
 *
 * EVERY CHAT WRITE IS A CHANGE THROUGH THE ACT TRANSPORT (ADR-0024), so a
 * suite stubs the one route every press reaches — `POST /operator/act/{tool}`
 * — and asserts on the tool and the arguments it was sent, rather than mocking
 * a module of the screen's: a mock of the screen's own write helper is a suite
 * that agrees with the screen about a route the engine may not serve. The
 * reads are the socket's, answered from a fixture per question.
 *
 * WHAT A PRESS NEEDS TO BE SENT AT ALL is the frame's: a viewer bound to a
 * seat whose `acts` name the tool (`lib/useWriteAccess.ts`), a connected
 * socket, and a toast host for what a press reports. [mountChat] supplies all
 * three — and the frame's one read of the session, which the screen names
 * its reader from — so a case that wants one missing says so by overriding
 * it.
 */

import { render } from "@testing-library/react";
import { AppAnnouncer } from "~/app/announcer.tsx";
import type { ReactNode } from "react";
import { vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { Store, type QueryName } from "~/protocol/index.ts";
import type { OrgServices } from "~/contract/services.ts";
import { healthFrame } from "./health.ts";
import { withDerived } from "./org.ts";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";

/** Every chat change the dashboard makes, as the engine serves it to a person. */
const CHAT_ACTS = [
  "post_message",
  "reply_in_thread",
  "edit_message",
  "delete_message",
  "react_to_message",
  "join_channel",
  "leave_channel",
  "create_channel",
  "open_dm",
  "update_channel",
  "add_channel_members",
  "mark_chat_read",
];

/**
 * A person signed in as `ada.lovelace` and bound to the seat `ada`, holding the
 * Chatlet app — and nothing else, so a case that passes is not passing on admin —
 * who may make every chat change.
 */
export const ADA = {
  login: "ada.lovelace",
  owner: "ada",
  handle: "ada",
  name: "Ada",
  kind: "human",
  grants: ["chatlet"],
  acts: CHAT_ACTS,
  project: "",
  config_writer: false,
  config_managed_by: [],
  chat_vendors: [],
};

/** What a company running Crewlet's own four services publishes as `services`. */
export const NATIVE_SERVICES: OrgServices = {
  tracker: "worklet",
  knowledge: "doclet",
  chat: "chatlet",
  code: "native",
};

/** What `GET /auth/session` answers for [ADA]: the person, and the seat they hold. */
export const ADA_SESSION = {
  person: "0b3c9b7e-8f7a-4c1d-9d2e-ada0ada0ada0",
  name: "Ada Lovelace",
  login: "ada.lovelace",
  seat: "ada",
  kind: "person",
  grants: ["chatlet"],
  status: "ok",
};

/** One press the engine was sent: the tool, its operation key, its arguments. */
export interface Pressed {
  tool: string;
  /** The `Idempotency-Key` it carried — the press's identity, which a Retry
   *  sends again. */
  opId: string;
  args: Record<string, unknown>;
}

/** What the engine answers a press with: a status, a JSON body, and the
 *  `Retry-After` a node's own `503` carries where waiting can change it. */
export interface ActReply {
  status: number;
  body: unknown;
  retryAfter?: number;
}

/** An `applied` answer, the receipt beside it. */
export function applied(tool: string, receipt: Record<string, unknown> = {}): ActReply {
  return {
    status: 200,
    body: { tool, outcome: "applied", position: "CREWLET_CHAT_LOG@1:41", receipt },
  };
}

/** A `pending` answer: durable, and this node has not applied it. */
export function pending(tool: string, receipt: Record<string, unknown> = {}): ActReply {
  return {
    status: 200,
    body: { tool, outcome: "pending", position: "CREWLET_CHAT_LOG@1:41", receipt },
  };
}

/** A tool's own `unknown`: it may have landed and it may not. */
export function unknown(tool: string): ActReply {
  return {
    status: 503,
    body: {
      tool,
      error: "unavailable",
      outcome: "unknown",
      detail: `${tool}: whether this landed is unknown`,
    },
  };
}

/** A refusal, in the class the tool gave it, with its own sentence. */
export function refused(tool: string, error: string, detail: string, status = 422): ActReply {
  return { status, body: { tool, error, detail } };
}

/**
 * A refusal the NODE made and said waiting would change: an `unavailable`
 * 503 carrying the `Retry-After` the engine writes beside one — the only
 * refusal a press offers "Try again" on (`protocol/act.ts`).
 */
export function refusedForNow(tool: string, detail: string, retryAfter = 1): ActReply {
  return { status: 503, body: { tool, error: "unavailable", detail }, retryAfter };
}

/**
 * The act route, answering each press with `answer(tool)` and recording it.
 * An answer may be a promise, for a case that holds a press unanswered while
 * it presses something else. The frame's session read is answered as
 * [ADA_SESSION].
 *
 * Every other request a screen makes is a test's mistake rather than an
 * answer, so it fails loudly.
 */
export function actEngine(
  answer: (tool: string) => ActReply | Promise<ActReply> = (tool) => applied(tool),
) {
  const presses: Pressed[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      if (String(url).endsWith("/auth/session")) {
        return new Response(JSON.stringify(ADA_SESSION), {
          status: 200,
          headers: { ...ENGINE_HEADERS, "Content-Type": "application/json" },
        });
      }
      const [, tool = ""] = String(url).split("/operator/act/");
      if (tool === "") throw new Error(`a chat suite reached ${String(url)}, which is no press`);
      const body = JSON.parse(String(init.body ?? "{}")) as { args: Record<string, unknown> };
      const name = decodeURIComponent(tool);
      presses.push({
        tool: name,
        opId: new Headers(init.headers).get("Idempotency-Key") ?? "",
        args: body.args,
      });
      const reply = await answer(name);
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: {
          ...ENGINE_HEADERS,
          "Content-Type": "application/json",
          ...(reply.retryAfter === undefined ? {} : { "Retry-After": String(reply.retryAfter) }),
        },
      });
    }),
  );
  return presses;
}

/** A suite's own answer to a question, for one that turns on its parameters. */
type Answerer = (what: string, params?: Record<string, unknown>) => Promise<unknown>;

/**
 * Mount a piece of the chat screen as the frame would: a connected socket
 * whose questions answer from `answers` (or from `answer`, for a suite whose
 * answers turn on a question's parameters), the session and the viewer read
 * every screen shares, a toast host and the router — and, where `services` is
 * given, an org projection publishing them, which a piece that waits for the
 * engine to say the company runs Chatlet (`lib/services.ts`) reads.
 */
export function mountChat(
  children: ReactNode,
  {
    answers = {},
    answer,
    viewer = ADA,
    connected = true,
    strict = false,
    services,
  }: {
    answers?: Partial<Record<QueryName, unknown>>;
    answer?: Answerer;
    /** What the viewer read answers, or the error it fails with. */
    viewer?: Record<string, unknown> | Error;
    connected?: boolean;
    /** Under `StrictMode` at the ROOT, as `main.tsx` renders the dashboard —
     *  the one place React's development-only extra setup and cleanup of
     *  every effect happens; nested under these providers it does not. */
    strict?: boolean;
    /** What the company runs, published on an org projection; none by default. */
    services?: OrgServices;
  } = {},
) {
  const store = new Store();
  if (connected) store.applyHealth(healthFrame({ nodes: 1 }));
  if (services) store.applyOrg(withDerived({ name: "Acme", roles: [], services }));
  const socket = {
    query: vi.fn(async (what: string, params?: Record<string, unknown>) => {
      if (what === "viewer") {
        if (viewer instanceof Error) throw viewer;
        return viewer;
      }
      if (answer) return answer(what, params);
      return answers[what as QueryName] ?? {};
    }),
    focus: vi.fn(),
  };
  const view = render(
    <ToastProvider>
      <LayerHost>
        <AppAnnouncer />
        <ClientContext.Provider value={{ store, socket } as never}>
          <SessionReading>
            <ViewerProvider>
              <Router>{children}</Router>
            </ViewerProvider>
          </SessionReading>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
    { reactStrictMode: strict },
  );
  return { store, socket, view };
}
