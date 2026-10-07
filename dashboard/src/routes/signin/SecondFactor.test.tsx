/**
 * Setting up an authenticator, and the two dialogs a person manages their
 * proof in from their Account page.
 *
 * THE SEED IS DRAWN TO SCAN: the QR code is the seed's own `otpauth://` URI,
 * read back off the drawing by a real decoder, with the key to type and the
 * link to open beside it — and nothing is drawn before the engine has
 * answered a seed, or after the enrolment it was for.
 *
 * A NEW SET OF RECOVERY CODES RETIRES THE OLD ONE, so the dialog issues one
 * only when asked — never on opening — and an answer nobody can confirm is
 * not called harmless: it may have stored a set this page will never see.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import {
  AuthenticatorDialog,
  RecoveryCodesDialog,
  SecondFactorSetup,
  groupedKey,
} from "./SecondFactor.tsx";
import { sessionRestored } from "~/protocol/index.ts";
import { scan } from "~/test/qr.ts";

type Answer = { status: number; body: unknown };

/**
 * A stub engine. A route answers its one answer, or each of a list in turn —
 * the last again once the list is spent — and an answer may be a promise, so
 * a case can hold the engine's reply back and look at the screen meanwhile.
 */
function engine(
  routes: Record<string, Answer | Promise<Answer> | (Answer | Promise<Answer>)[]>,
): { method: string; path: string }[] {
  const sent: { method: string; path: string }[] = [];
  const queues = new Map(
    Object.entries(routes).map(([k, v]) => [k, Array.isArray(v) ? [...v] : [v]] as const),
  );
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), "http://engine.test");
      const method = (init?.method ?? "GET").toUpperCase();
      sent.push({ method, path: url.pathname });
      const queue = queues.get(`${method} ${url.pathname}`);
      const answer = queue && (await (queue.length > 1 ? queue.shift()! : queue[0]!));
      return new Response(JSON.stringify(answer?.body ?? { error: "no_route" }), {
        status: answer?.status ?? 404,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

function dialog(held = true) {
  render(
    <ToastProvider>
      <LayerHost>
        <RecoveryCodesDialog held={held} onClose={() => {}} />
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

  // A FIRST SET RETIRES NOTHING, and says nothing about one. The CONTROL is a
  // person who held a set. Mutation: say "earlier codes" whatever was held and
  // the first set speaks of codes its person never had.
  test.each([
    { held: false, earlier: false },
    { held: true, earlier: true },
  ])(
    "a set issued with held=$held speaks of earlier codes: $earlier",
    async ({ held, earlier }) => {
      engine({ "POST /auth/totp/recovery": { status: 200, body: { codes: ["a1b2-c3d4"] } } });
      dialog(held);
      await screen.findByRole("dialog", { name: "Recovery codes" });
      expect(screen.queryByText(/retires the one you hold/) !== null).toBe(earlier);
      fireEvent.click(screen.getByRole("button", { name: "Issue new codes" }));
      await screen.findByText("a1b2-c3d4");
      expect(screen.queryByText(/earlier codes no longer work/) !== null).toBe(earlier);
    },
  );

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

describe("setting up an authenticator", () => {
  const SECRET = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP";
  const URI = `otpauth://totp/crewlet.example.com:jane.doe?algorithm=SHA1&digits=6&issuer=crewlet.example.com&period=30&secret=${SECRET}`;
  const SEED: Answer = { status: 200, body: { secret: SECRET, uri: URI } };
  const QR = { name: "QR code for your authenticator app" };

  function setup() {
    render(
      <ToastProvider>
        <LayerHost>
          <SecondFactorSetup onEnrolled={() => {}} />
        </LayerHost>
      </ToastProvider>,
    );
  }

  test("draws nothing before the engine answers a seed, then the seed's own URI to scan", async () => {
    let answer!: (a: Answer) => void;
    const sent = engine({ "POST /auth/totp": new Promise<Answer>((r) => (answer = r)) });
    setup();
    await waitFor(() => expect(sent.map((s) => s.path)).toEqual(["/auth/totp"]));
    // ASKED AND NOT ANSWERED: no code, no key and no link stand in for a seed.
    expect(screen.queryByRole("img", QR)).toBeNull();
    expect(screen.queryByText(groupedKey(SECRET))).toBeNull();
    expect(screen.queryByRole("link", { name: "open it in the app" })).toBeNull();

    answer(SEED);
    // EXACTLY THE URI THE ENGINE ANSWERED, read back off the drawing by a
    // decoder: not the key (which a code of the secret alone would carry),
    // not a URI composed here.
    expect(scan(await screen.findByRole("img", QR))).toBe(URI);
  });

  test("keeps the key to type and the link to open beside the code, for whoever cannot scan", async () => {
    engine({ "POST /auth/totp": SEED });
    setup();
    await screen.findByRole("img", QR);
    expect(screen.getByText(groupedKey(SECRET))).toBeDefined();
    expect(screen.getByRole("link", { name: "open it in the app" }).getAttribute("href")).toBe(URI);
    expect(screen.getByRole("button", { name: "Copy key" })).toBeDefined();
  });

  test("takes the code off the screen once the enrolment it was for has landed", async () => {
    const sent = engine({
      "POST /auth/totp": [SEED, { status: 200, body: { status: "enrolled" } }],
    });
    // THE LAYER HOST FIRST, as the app mounts it long before a dialog opens:
    // mounted in the same render, its node arrives a render late and the
    // dialog's body moves into it, mounting the setup — and asking a seed —
    // twice.
    const tree = (open: boolean) => (
      <ToastProvider>
        <LayerHost>{open && <AuthenticatorDialog codes onClose={() => {}} />}</LayerHost>
      </ToastProvider>
    );
    const { rerender } = render(tree(false));
    rerender(tree(true));
    expect(scan(await screen.findByRole("img", QR))).toBe(URI);
    expect(sent.filter((s) => s.path === "/auth/totp")).toHaveLength(1);
    fireEvent.change(screen.getByLabelText(/six-digit code/i), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "Confirm" }));
    await screen.findByText(/Your authenticator is set up/);
    expect(screen.queryByRole("img", QR)).toBeNull();
    expect(screen.queryByText(groupedKey(SECRET))).toBeNull();
  });
});
