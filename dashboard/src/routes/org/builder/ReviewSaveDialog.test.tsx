/**
 * Review and save: the review states the change and its consequences, the
 * save sends exactly the checked write with its preconditions and a signed
 * summary, and every answer that is not a plain success leads somewhere that
 * cannot lose or duplicate the work.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { DRAFT_STORAGE_KEY } from "./model/persistence.ts";
import { company, Engine, json, mountBuilder, refusal, type SentRequest } from "./testkit.tsx";

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  location.hash = "#/";
});

const isWrite = (r: SentRequest) =>
  r.path === "/config" && (r.method === "PATCH" || r.method === "PUT") && !r.query.has("dry_run");

/** Opens the builder, edits the CEO, and opens the review once the check is clean. */
async function reviewEdit(engine: Engine) {
  mountBuilder({ engine });
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
  await waitFor(() => expect(engine.checks()).toHaveLength(2));
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
  return screen.findByRole("dialog", { name: "Review and save" });
}

describe("the save", () => {
  test("sends the checked merge patch with If-Match and a signed summary, then loads the revision", async () => {
    const engine = new Engine(company());
    const dialog = await reviewEdit(engine);
    expect(within(dialog).getByText("Edits CEO: goal.")).toBeDefined();
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));

    await waitFor(() => expect(engine.requests.filter(isWrite)).toHaveLength(1));
    const write = engine.requests.filter(isWrite)[0]!;
    expect(write.method).toBe("PATCH");
    expect(write.query.toString()).toBe("");
    expect(write.headers["If-Match"]).toBe('"r1"');
    expect(write.headers["Content-Type"]).toBe("application/merge-patch+json");
    const expected = company();
    expected.roles![0]!.goal = "Lead and more";
    const { _summary, ...patch } = write.body as { _summary: string };
    expect(patch).toEqual({ roles: expected.roles });
    expect(_summary).toMatch(/^Edited 1 seat \(write [0-9a-f]{32}\)$/);

    expect(await screen.findByText("Saved. The engine is applying it.")).toBeDefined();
    // The stored revision is loaded, and the kept draft is gone with the work saved.
    await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r-saved"'));
    expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("a revision saved first by somebody else leads into update my draft, then back to the review", async () => {
    const engine = new Engine(company());
    const dialog = await reviewEdit(engine);
    const next = company();
    next.roles![1]!.goal = "Design things";
    engine.document = next;
    engine.revision = "r2";
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));

    const update = await screen.findByRole("dialog", { name: "Update my draft and review" });
    fireEvent.click(within(update).getByRole("button", { name: "Update my draft" }));
    const review = await screen.findByRole("dialog", { name: "Review and save" });
    expect(within(review).getByText("Edits CEO: goal.")).toBeDefined();
    await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r2"'));
  });

  test("a refusal with problems places them on the seats they name and keeps Save disabled", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      isWrite(r)
        ? refusal([
            {
              path: "roles[1].goal",
              segments: ["roles", 1, "goal"],
              kind: "invalid",
              message: "roles[1].goal: too long",
            },
          ])
        : null;
    const dialog = await reviewEdit(engine);
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));

    expect(
      await within(dialog).findByText(
        "The engine refused the save. The problems it found are marked on the chart; fix them, then save again.",
      ),
    ).toBeDefined();
    expect(screen.getByTestId("problems Designer").textContent).toBe("1");
    expect(screen.getByText("1 problem")).toBeDefined();
    expect(
      (within(dialog).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
});

describe("a save whose answer never arrives", () => {
  test("is found in the revision history and treated as saved", async () => {
    const engine = new Engine(company());
    engine.script = (r, e) => {
      if (isWrite(r)) {
        e.commit(r);
        return Promise.reject(new TypeError("network connection was lost"));
      }
      return null;
    };
    const dialog = await reviewEdit(engine);
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Saved. The engine is applying it.")).toBeDefined();
    expect(engine.sent("GET", "/config/revisions/r-saved")).toHaveLength(1);
    expect(engine.requests.filter(isWrite)).toHaveLength(1);
    // Found by its parent, so what it changed is still that parent's diff.
    expect(screen.getByRole("link", { name: "View changes" }).getAttribute("href")).toBe(
      "#/config?lens=diff&revision=r-saved&against=r1",
    );
  });

  test("that did not land is offered again, and nothing was stored twice", async () => {
    const engine = new Engine(company());
    let first = true;
    engine.script = (r) => {
      if (isWrite(r) && first) {
        first = false;
        return Promise.reject(new TypeError("network connection was lost"));
      }
      return null;
    };
    const dialog = await reviewEdit(engine);
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(
      await within(dialog).findByText(
        "The save did not reach the engine, and nothing was stored. Save again once the engine is reachable.",
      ),
    ).toBeDefined();
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Saved. The engine is applying it.")).toBeDefined();
    expect(engine.revisions.size).toBe(1);
  });

  test("a save that lands after the lens was left still clears the kept draft", async () => {
    const engine = new Engine(company());
    let answer: (response: Response) => void = () => {};
    engine.script = (r, e) => {
      if (!isWrite(r)) return null;
      const revision = e.commit(r);
      return new Promise<Response>((resolve) => {
        answer = () => resolve(json({ revision_id: revision, epoch: 2, warnings: [] }, 201));
      });
    };
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Edit CEO" }));
    await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).not.toBeNull());
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
    const dialog = await screen.findByRole("dialog", { name: "Review and save" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(engine.requests.filter(isWrite)).toHaveLength(1));

    cleanup();
    answer(new Response());
    // A kept log of a saved draft would be offered for replay onto its own revision.
    await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull());
  });

  test("a second press is recognized as the same write rather than replayed as a conflict", async () => {
    const engine = new Engine(company());
    let history = false;
    engine.script = (r, e) => {
      if (isWrite(r) && e.revision === "r1") {
        e.commit(r);
        return Promise.reject(new TypeError("network connection was lost"));
      }
      if (r.path.startsWith("/config/revisions/") && !history) {
        return json({ error: "internal" }, 500);
      }
      return null;
    };
    const dialog = await reviewEdit(engine);
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(
      await within(dialog).findByText(/Whether the save was stored could not be confirmed/),
    ).toBeDefined();
    // Nothing can be edited while the outcome is unknown.
    expect(screen.getByText("read only")).toBeDefined();

    history = true;
    fireEvent.click(within(dialog).getByRole("button", { name: "Save again" }));
    expect(await screen.findByText("Saved. The engine is applying it.")).toBeDefined();
    const writes = engine.requests.filter(isWrite);
    expect(writes).toHaveLength(2);
    // The same write id, which is how the 409 of the second press was
    // recognized as the operator's own first save.
    expect((writes[1]!.body as { _summary: string })._summary).toBe(
      (writes[0]!.body as { _summary: string })._summary,
    );
    expect(screen.queryByRole("dialog", { name: "Update my draft and review" })).toBeNull();
  });
});

describe("the review", () => {
  test("a company rename cannot be saved until its consequence is acknowledged", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
    const dialog = await screen.findByRole("dialog", { name: "Review and save" });
    expect(within(dialog).getByText("Renames the company from Acme to Acme Labs.")).toBeDefined();
    const save = within(dialog).getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.click(
      within(dialog).getByRole("checkbox", {
        name: "Renaming the company gives every seat a new identity: memory, inboxes and onboarding start over.",
      }),
    );
    expect(save.disabled).toBe(false);
  });

  // COUNTED SENTENCES READ FOR ONE as well as for many: "1 seat onboard
  // again" is the kind of copy an operator reads as a draft nobody proofread.
  test("a consequence about a single seat agrees with its count", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename Engineering" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(2));
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
    const dialog = await screen.findByRole("dialog", { name: "Review and save" });
    expect(
      within(dialog).getByText(
        "1 seat onboards again because a unit they belong to is renamed: Dev.",
      ),
    ).toBeDefined();
  });

  test("an empty audit summary cannot be saved", async () => {
    const engine = new Engine(company());
    const dialog = await reviewEdit(engine);
    fireEvent.change(within(dialog).getByLabelText("Audit summary"), { target: { value: "  " } });
    expect(
      (within(dialog).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
});
