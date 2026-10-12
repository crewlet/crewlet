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
 * three, so a case that wants one missing says so by overriding it.
 */

import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import { vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { Store, type QueryName } from "~/protocol/index.ts";
import { healthFrame } from "./health.ts";

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
  "mark_chat_read",
];

/** A person whose token is bound to the seat `ada`, who may make every chat change. */
export const ADA = {
  operator_id: "U0ADA",
  operator: true,
  handle: "ada",
  name: "Ada",
  kind: "human",
  acts: CHAT_ACTS,
};

/** One press the engine was sent. */
export interface Pressed {
  tool: string;
  request_id: string;
  args: Record<string, unknown>;
}

/** What the engine answers a press with: a status and a JSON body. */
export interface ActReply {
  status: number;
  body: unknown;
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
 * The act route, answering each press with `answer(tool)` and recording it.
 * An answer may be a promise, for a case that holds a press unanswered while
 * it presses something else.
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
      const [, tool = ""] = String(url).split("/operator/act/");
      if (tool === "") throw new Error(`a chat suite reached ${String(url)}, which is no press`);
      const body = JSON.parse(String(init.body ?? "{}")) as Omit<Pressed, "tool">;
      const name = decodeURIComponent(tool);
      presses.push({ tool: name, request_id: body.request_id, args: body.args });
      const reply = await answer(name);
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { "Content-Type": "application/json" },
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
 * answers turn on a question's parameters), the viewer read every screen
 * shares, a toast host and the router.
 */
export function mountChat(
  children: ReactNode,
  {
    answers = {},
    answer,
    viewer = ADA,
    connected = true,
    strict = false,
  }: {
    answers?: Partial<Record<QueryName, unknown>>;
    answer?: Answerer;
    viewer?: Record<string, unknown>;
    connected?: boolean;
    /** Under `StrictMode` at the ROOT, as `main.tsx` renders the dashboard —
     *  the one place React's development-only extra setup and cleanup of
     *  every effect happens; nested under these providers it does not. */
    strict?: boolean;
  } = {},
) {
  const store = new Store();
  if (connected) store.applyHealth(healthFrame({ nodes: 1 }));
  const socket = {
    query: vi.fn(async (what: string, params?: Record<string, unknown>) => {
      if (what === "viewer") return viewer;
      if (answer) return answer(what, params);
      return answers[what as QueryName] ?? {};
    }),
    focus: vi.fn(),
  };
  const view = render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket } as never}>
          <ViewerProvider>
            <Router>{children}</Router>
          </ViewerProvider>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
    { reactStrictMode: strict },
  );
  return { store, socket, view };
}
