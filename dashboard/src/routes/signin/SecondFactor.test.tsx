/**
 * The two dialogs a person manages their proof in, opened from the sidebar's
 * user block.
 *
 * A NEW SET OF RECOVERY CODES RETIRES THE OLD ONE, so the dialog issues one
 * only when asked — never on opening — and an answer nobody can confirm is
 * not called harmless: it may have stored a set this page will never see.
 */

import { cleanup, fireEvent, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { RecoveryCodesDialog } from "./SecondFactor.tsx";
import { sessionRestored } from "~/protocol/index.ts";

type Answer = { status: number; body: unknown };

function engine(routes: Record<string, Answer>): { method: string; path: string }[] {
  const sent: { method: string; path: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      const method = (init?.method ?? "GET").toUpperCase();
      sent.push({ method, path: url.pathname });
      const answer = routes[`${method} ${url.pathname}`];
      return new Response(JSON.stringify(answer?.body ?? { error: "no_route" }), {
        status: answer?.status ?? 404,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

function dialog() {
  render(
    <ToastProvider>
      <LayerHost>
        <RecoveryCodesDialog onClose={() => {}} />
      </LayerHost>
    </ToastProvider>,
  );
}

beforeEach(() => {
  Element.prototype.scrollIntoView = () => {};
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionRestored();
});

describe("recovery codes", () => {
  test("are issued only when asked, and shown the once", async () => {
    const sent = engine({
      "POST /auth/totp/recovery": { status: 200, body: { codes: ["a1b2-c3d4", "e5f6-g7h8"] } },
    });
    dialog();
    await screen.findByRole("dialog", { name: "Recovery codes" });
    expect(sent.filter((s) => s.path === "/auth/totp/recovery")).toEqual([]);

    fireEvent.click(screen.getByRole("button", { name: "Issue new codes" }));
    expect(await screen.findByText("a1b2-c3d4")).toBeDefined();
    expect(screen.getByText("e5f6-g7h8")).toBeDefined();
    expect(sent.filter((s) => s.path === "/auth/totp/recovery")).toHaveLength(1);
  });

  test("a set nobody can confirm was stored is not called harmless", async () => {
    engine({
      "POST /auth/totp/recovery": {
        status: 503,
        body: { error: "unavailable", message: "This node cannot answer that right now." },
      },
    });
    dialog();
    fireEvent.click(await screen.findByRole("button", { name: "Issue new codes" }));
    expect(await screen.findByText(/It is not known whether a new set was stored/)).toBeDefined();
    expect(screen.queryByText(/still works/)).toBeNull();
  });
});
