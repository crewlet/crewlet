/**
 * A removal states the ENTRY'S tag, read over the real transport.
 *
 * The engine decides `DELETE /config/llm-providers/{key}` on the entry's own
 * tag (`"llm-providers-<digest>"`, `configapi.entityTag`), never the
 * document's (the revision id `GET /config` answers): sent the document's, it
 * answers 412 `version_moved`, and the dialog refused every removal as a
 * colleague's change. Nothing here is mocked but `fetch`, which answers as
 * `configapi` does, the two tags apart, so the dialog's own reads and
 * requests decide which one it sends.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Router } from "~/app/router.tsx";
import { apiKeyEntry, modelsData, modelsDoc, seat } from "~/routes/settings/models/fixtures.ts";
import { RemoveDialog } from "./RemoveDialog.tsx";

/** The two tags as the engine writes them: a revision id, and a kind and a digest. */
const DOCUMENT_TAG = '"0b8f7c1e-9a3d-4f61-8e2a-5c7d9e1f3a20"';
const ENTRY_TAG = '"llm-providers-3f9a1c0d7e2b4a6f8c1d5e9b7a3f2c10"';

const REFERENCES = [
  {
    at: { seat: "backend-engineer", job: "executor" },
    segments: ["roles", 0, "llm"],
    chain: ["sonnet"],
  },
];

interface Call {
  readonly method: string;
  readonly path: string;
  readonly ifMatch: string | null;
  readonly body: { repoint?: unknown[] } | null;
}

const json = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });

/** `configapi`'s answers to the requests a removal makes. */
function engine(): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      const parsed = new URL(String(url));
      const headers = new Headers(init.headers);
      const call: Call = {
        method: init.method ?? "GET",
        path: parsed.pathname,
        ifMatch: headers.get("If-Match"),
        body: init.body ? (JSON.parse(init.body as string) as Call["body"]) : null,
      };
      calls.push(call);
      if (call.method === "GET" && call.path === "/config") {
        return json(modelsDoc({}), 200, { ETag: DOCUMENT_TAG });
      }
      if (call.method === "GET" && call.path === "/config/llm-providers/sonnet") {
        return json({ harness: "claude-code", model: "sonnet" }, 200, { ETag: ENTRY_TAG });
      }
      if (call.method === "DELETE" && call.path === "/config/llm-providers/sonnet") {
        if (call.ifMatch !== ENTRY_TAG) {
          return json(
            { error: "version_moved", detail: "llm-providers sonnet changed since it was read" },
            412,
          );
        }
        if ((call.body?.repoint ?? []).length < REFERENCES.length) {
          return json(
            {
              error: "validation_error",
              detail: "configapi: sonnet is named by backend-engineer's coding",
              problems: [],
              references: REFERENCES,
            },
            422,
          );
        }
        return parsed.searchParams.get("dry_run") === "true"
          ? json({ valid: true, base_revision_id: "rev-1", warnings: [], derived: null }, 200)
          : json({ revision_id: "rev-2", epoch: 3, warnings: [], derived: null }, 201);
      }
      return json({ error: "no_route" }, 404);
    }),
  );
  return calls;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.location.hash = "";
});

test("every removal request states the entry's tag, and the removal goes through", async () => {
  const calls = engine();
  const data = modelsData({
    entries: [apiKeyEntry({ key: "sonnet" }), apiKeyEntry({ key: "default", harness: "codex" })],
    doc: modelsDoc({}),
    seats: [seat("backend-engineer", "Backend Engineer")],
    assistant: { enabled: false, key: null },
  });
  const onDone = vi.fn();
  render(
    <Router>
      <RemoveDialog model={data.models[0]!} data={data} onClose={() => {}} onDone={onDone} />
    </Router>,
  );
  await screen.findByText("Ready to remove");
  fireEvent.change(screen.getByRole("textbox"), { target: { value: "sonnet" } });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Remove model" }));
  });
  const removals = calls.filter((c) => c.method === "DELETE");
  expect(removals.length).toBeGreaterThanOrEqual(3);
  for (const removal of removals) expect(removal.ifMatch).toBe(ENTRY_TAG);
  expect(onDone).toHaveBeenCalled();
});

/**
 * A PROXY'S 403 OR 413 IS NO REFUSAL OF THE ENGINE'S. The removal itself met
 * by one, in the engine's own envelope but without its mark, may have landed
 * behind it, so the dialog says nobody knows rather than "Not removed".
 */
test.each([403, 413])(
  "a removal a proxy answers %i says nobody knows whether it landed",
  async (status) => {
    const calls = engine();
    const real = globalThis.fetch;
    vi.stubGlobal("fetch", async (url: string, init: RequestInit = {}) => {
      const parsed = new URL(String(url));
      if ((init.method ?? "GET") === "DELETE" && parsed.searchParams.get("dry_run") !== "true") {
        return new Response(JSON.stringify({ error: "forbidden", detail: "blocked by policy" }), {
          status,
          headers: { "Content-Type": "application/json" },
        });
      }
      return real(url, init);
    });
    const data = modelsData({
      entries: [apiKeyEntry({ key: "sonnet" }), apiKeyEntry({ key: "default", harness: "codex" })],
      doc: modelsDoc({}),
      seats: [seat("backend-engineer", "Backend Engineer")],
      assistant: { enabled: false, key: null },
    });
    const onDone = vi.fn();
    render(
      <Router>
        <RemoveDialog model={data.models[0]!} data={data} onClose={() => {}} onDone={onDone} />
      </Router>,
    );
    await screen.findByText("Ready to remove");
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "sonnet" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Remove model" }));
    });
    expect(screen.queryByText("Not removed")).toBeNull();
    const title = await screen.findByText("Not known whether it was removed");
    expect(title.closest(".crewlet-callout")?.textContent).toContain(
      "The engine did not answer. sonnet may or may not have been removed.",
    );
    expect(onDone).not.toHaveBeenCalled();
    expect(calls.length).toBeGreaterThan(0);
  },
);
