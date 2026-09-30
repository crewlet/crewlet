/**
 * Review and save: the review states the change and its consequences, the
 * save sends the chart's writes and the settings' in the order the engine
 * takes them, each under the operation id a retry resends, and every answer
 * that is not a plain success leads somewhere that cannot lose or duplicate
 * the work.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { DRAFT_STORAGE_KEY } from "./model/persistence.ts";
import { clearSavedChanges } from "./savedChanges.ts";
import { company, Engine, json, mountBuilder, type SentRequest } from "./testkit.tsx";
import { toastHost, toastText } from "~/testing.tsx";

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  clearSavedChanges();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
  clearSavedChanges();
  location.hash = "#/";
});

/** A settings write, never a dry run. */
const isSettingsWrite = (r: SentRequest) =>
  r.path === "/config" && (r.method === "PATCH" || r.method === "PUT") && !r.query.has("dry_run");

/** A content write of one seat. */
const isSeatWrite = (handle: string) => (r: SentRequest) =>
  r.method === "PATCH" && r.path === `/chart/seats/${handle}`;

/** The Builder's one polite live region. */
const liveRegion = () => document.querySelector("[data-live-region]")!;

/** Presses a stand-in view's button, and waits for the check of what it did. */
async function act_(name: string, said: string) {
  fireEvent.click(screen.getByRole("button", { name }));
  await waitFor(() => expect(liveRegion().textContent).toContain(said));
  await screen.findByText("No problems");
}

/** Opens the review once the check is clean. */
async function openReview() {
  await screen.findByText("No problems");
  fireEvent.click(screen.getByRole("button", { name: "Review and save" }));
  return screen.findByRole("dialog", { name: "Review and save" });
}

/** Opens the builder, edits the CEO, and opens the review. */
async function reviewEdit(engine: Engine) {
  mountBuilder({ engine });
  await screen.findByText("No problems");
  await act_("Edit CEO", "Edited CEO: goal.");
  return openReview();
}

describe("the save", () => {
  test("writes a changed seat's whole content under its operation id, then reads the chart back", async () => {
    const engine = new Engine(company());
    const dialog = await reviewEdit(engine);
    expect(within(dialog).getByText("Edits CEO: goal.")).toBeDefined();
    // The chart records who made each change, and no settings are written, so
    // there is no summary to ask for.
    expect(within(dialog).queryByLabelText("Audit summary")).toBeNull();
    const before = engine.requests.length;
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));

    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    const sent = engine.requests.slice(before);
    const write = sent.find(isSeatWrite("ceo"))!;
    // FULL POST-STATE, the runtime half left out because it did not change.
    expect(write.body).toEqual({
      unit: "",
      name: "CEO",
      email: "",
      backstory: "",
      goal: "Lead and more",
      responsibilities: [],
      behavioral_guidelines: [],
      project: "",
      space: "",
    });
    expect(write.headers["Idempotency-Key"]).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\.builder-save\.1$/,
    );
    // The chart was read once more before the first write, and nothing else was written.
    expect(sent.slice(0, sent.indexOf(write)).some((r) => r.path === "/chart")).toBe(true);
    expect(engine.chartWrites()).toEqual([write]);
    expect(engine.requests.filter(isSettingsWrite)).toHaveLength(0);
    expect(engine.seats.find((s) => s.handle === "ceo")!.goal).toBe("Lead and more");

    // Said once: the toast's host is a live region of its own, and the
    // Builder's region repeating it had a screen reader read it twice.
    await new Promise((r) => setTimeout(r, 150));
    // INSIDE THE BUILDER'S OWN LAYER HOST, not at the document root: a
    // fullscreen canvas renders only its own subtree, so a toast portalled
    // past it is a Save that reports nothing.
    const toaster = toastHost()!;
    expect(toaster.textContent).toContain("Saved.");
    expect(toaster.closest(".org-builder")).not.toBeNull();
    expect(liveRegion().textContent).not.toContain("Saved.");
    // What it wrote is read back and stood on: the next check is clean, and
    // the kept draft is gone with the work saved.
    await waitFor(() => expect(screen.getByText("editable")).toBeDefined());
    expect(await screen.findByText("No problems")).toBeDefined();
    expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  test("the settings are written after the chart, conditional on the base, with a signed summary", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    await act_("Edit CEO", "Edited CEO: goal.");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const dialog = await openReview();
    fireEvent.click(within(dialog).getByRole("checkbox"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));

    await waitFor(() => expect(engine.requests.filter(isSettingsWrite)).toHaveLength(1));
    const settings = engine.requests.filter(isSettingsWrite)[0]!;
    expect(settings.method).toBe("PATCH");
    expect(settings.query.toString()).toBe("");
    expect(settings.headers["If-Match"]).toBe('"r1"');
    expect(settings.headers["Content-Type"]).toBe("application/merge-patch+json");
    const { _summary, ...patch } = settings.body as { _summary: string };
    expect(patch).toEqual({ name: "Acme Labs" });
    expect(_summary).toMatch(
      / \(write [0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\)$/,
    );
    // THE CHART FIRST: a settings revision that names the company's new name
    // is what re-onboards every seat, so it lands last.
    const seat = engine.requests.findIndex(isSeatWrite("ceo"));
    expect(seat).toBeGreaterThan(-1);
    expect(seat).toBeLessThan(engine.requests.indexOf(settings));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    expect(engine.revision).toBe("r-saved");
  });

  test("a seat added in the draft is created by a batch, filled in, and stays selected", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    await act_("Add an analyst", "Analyst");
    fireEvent.click(screen.getByRole("button", { name: "Select Analyst" }));
    const dialog = await openReview();
    expect(within(dialog).getByText("Adds the seat Analyst.")).toBeDefined();
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));

    const [batch, content] = engine.chartWrites();
    expect(batch!.path).toBe("/chart/batch");
    expect(batch!.body).toEqual({
      operations: [
        { kind: "create_seat", object: { kind: "seat", id: "analyst" }, seat_kind: "agent" },
      ],
    });
    // A CREATED SEAT IS ALWAYS FILLED IN: the chart places no seat no content
    // record has filled.
    expect(content!.path).toBe("/chart/seats/analyst");
    expect(content!.body).toMatchObject({ name: "Analyst", goal: "Analyse" });
    expect(engine.seats.find((s) => s.handle === "analyst")).toMatchObject({ goal: "Analyse" });
    // Keyed by the handle the chart now holds it under, and still selected.
    await waitFor(() => expect(screen.getByText("editable")).toBeDefined());
    expect(location.hash).toContain("seat=analyst");
    expect(screen.getByRole("button", { name: "Analyst" }).getAttribute("aria-haspopup")).toBe(
      "menu",
    );
  });

  test("a removal is a batch of its own, sent after the structure", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    await act_("Remove Designer", "Designer");
    const dialog = await openReview();
    expect(within(dialog).getByText("Removes the seat Designer.")).toBeDefined();
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    expect(engine.chartWrites().map((r) => r.body)).toEqual([
      { operations: [{ kind: "remove", object: { kind: "seat", id: "designer" } }] },
    ]);
    expect(engine.seats.map((s) => s.handle)).not.toContain("designer");
  });

  test("a chart somebody else changed first leads into update my draft, then back to the review", async () => {
    const engine = new Engine(company());
    const dialog = await reviewEdit(engine);
    engine.seats.find((s) => s.handle === "designer")!.goal = "Design things";
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));

    const update = await screen.findByRole("dialog", { name: "Update my draft and review" });
    // Found by the read before the first write: nothing of this save was sent.
    expect(engine.chartWrites()).toHaveLength(0);
    fireEvent.click(within(update).getByRole("button", { name: "Update my draft" }));
    const review = await screen.findByRole("dialog", { name: "Review and save" });
    expect(within(review).getByText("Edits CEO: goal.")).toBeDefined();
    fireEvent.click(within(review).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    // Written over the colleague's rows, never over the colleague's change.
    expect(engine.seats.find((s) => s.handle === "designer")!.goal).toBe("Design things");
    expect(engine.seats.find((s) => s.handle === "ceo")!.goal).toBe("Lead and more");
  });

  test("a write refused on authority names the grant and the fields, and what landed stays landed", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      isSeatWrite("designer")(r)
        ? json(
            {
              error: "unauthorized",
              reason: "no_grant",
              grants: ["config:write"],
              fields: ["project"],
              detail: "refused",
            },
            403,
          )
        : null;
    mountBuilder({ engine });
    await screen.findByText("No problems");
    await act_("Edit CEO", "Edited CEO: goal.");
    await act_("Edit Designer", "Edited Designer: goal.");
    const dialog = await openReview();
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));

    expect(
      await within(dialog).findByText(
        /^Saving the seat Designer needs config:write, which the credential you presented does not carry\. It changes project, which takes it\./,
      ),
    ).toBeDefined();
    expect(
      within(dialog).getByText(
        /What was written before it stays written; the draft now holds only what is left to save\./,
      ),
    ).toBeDefined();
    // The CEO's write landed before the refusal, and the Designer's did not.
    expect(engine.seats.find((s) => s.handle === "ceo")!.goal).toBe("Lead and more");
    expect(engine.seats.find((s) => s.handle === "designer")!.goal).toBe("Design");
    // Refused, so nothing is left unknown: the kept log carries no save to settle.
    await waitFor(() =>
      expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toBeUndefined(),
    );
  });

  test("a batch refused names the operation it refused", async () => {
    const engine = new Engine(company());
    engine.script = (r) =>
      r.path === "/chart/batch"
        ? json(
            {
              error: "refused",
              detail: "The handle analyst is reserved.",
              index: 0,
              rule: "reserved",
            },
            422,
          )
        : null;
    mountBuilder({ engine });
    await screen.findByText("No problems");
    await act_("Add an analyst", "Analyst");
    const dialog = await openReview();
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(
      await within(dialog).findByText(
        "The engine refused the change to the chart's structure (Analyst): The handle analyst is reserved.",
      ),
    ).toBeDefined();
    expect(engine.seats.map((s) => s.handle)).not.toContain("analyst");
  });
});

describe("a write whose answer never arrives", () => {
  test("is sent again under the same operation id, which the engine answers rather than writes twice", async () => {
    const engine = new Engine(company());
    let lost = true;
    engine.script = (r, e) => {
      if (!isSeatWrite("ceo")(r) || !lost) return null;
      lost = false;
      // Written, and the answer lost on the way back.
      e.answer(r);
      return Promise.reject(new TypeError("network connection was lost"));
    };
    const dialog = await reviewEdit(engine);
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(
      await within(dialog).findByText(/^Whether the seat CEO was written could not be confirmed\./),
    ).toBeDefined();
    // Nothing can be edited while the outcome is unknown, and the kept log
    // is marked with the save.
    expect(screen.getByText("read only")).toBeDefined();
    expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    );

    fireEvent.click(within(dialog).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    const writes = engine.requests.filter(isSeatWrite("ceo"));
    expect(writes).toHaveLength(2);
    expect(writes[1]!.headers["Idempotency-Key"]).toBe(writes[0]!.headers["Idempotency-Key"]);
    // The ledger holds the write once.
    expect(engine.ledger.size).toBe(1);
    await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull());
  });

  test("a settings write whose answer was lost is found in the revision history", async () => {
    const engine = new Engine(company());
    engine.script = (r, e) => {
      if (isSettingsWrite(r)) {
        e.commit(r);
        return Promise.reject(new TypeError("network connection was lost"));
      }
      return null;
    };
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const dialog = await openReview();
    fireEvent.click(within(dialog).getByRole("checkbox"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    expect(engine.sent("GET", "/config/revisions/r-saved")).toHaveLength(1);
    expect(engine.requests.filter(isSettingsWrite)).toHaveLength(1);
  });

  test("a settings write that did not land is offered again, and nothing was stored twice", async () => {
    const engine = new Engine(company());
    let first = true;
    engine.script = (r) => {
      if (isSettingsWrite(r) && first) {
        first = false;
        return Promise.reject(new TypeError("network connection was lost"));
      }
      return null;
    };
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const dialog = await openReview();
    fireEvent.click(within(dialog).getByRole("checkbox"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(
      await within(dialog).findByText(
        /The settings did not reach the engine, and nothing was stored\./,
      ),
    ).toBeDefined();
    fireEvent.click(within(dialog).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    expect(engine.revisions.size).toBe(1);
  });

  test("a save that lands after the lens was left still clears the kept draft", async () => {
    const engine = new Engine(company());
    let answer: () => void = () => {};
    engine.script = (r, e) => {
      if (!isSeatWrite("ceo")(r)) return null;
      const response = e.answer(r);
      return new Promise<Response>((resolve) => {
        answer = () => resolve(response);
      });
    };
    const dialog = await reviewEdit(engine);
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(engine.requests.filter(isSeatWrite("ceo"))).toHaveLength(1));

    cleanup();
    answer();
    // A kept log of a saved draft would be offered for replay onto its own rows.
    await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull());
  });

  // LEFT BEFORE THE ANSWER CAME, AND THE ANSWER NEVER CAME. The save may have
  // landed, and its kept log is carried onto whatever did, never offered as
  // Keep and never replayed as though nothing had.
  describe("after the lens was left", () => {
    /** Saves an edit of the CEO, leaves the lens while the write is out, and loses its answer. */
    async function loseTheAnswer(engine: Engine, { lands }: { lands: boolean }) {
      let lose: () => void = () => {};
      engine.script = (r, e) => {
        if (!isSeatWrite("ceo")(r)) return null;
        if (lands) e.answer(r);
        return new Promise<Response>((_, reject) => {
          lose = () => reject(new TypeError("network connection was lost"));
        });
      };
      const dialog = await reviewEdit(engine);
      fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(engine.requests.filter(isSeatWrite("ceo"))).toHaveLength(1));
      // Marked before it went, so a reload now would find it too.
      expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toMatch(
        /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
      );
      cleanup();
      lose();
      await new Promise((resolve) => setTimeout(resolve, 20));
      expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toMatch(
        /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
      );
      engine.script = () => null;
    }

    test("a write that landed is carried onto, and nothing is written twice", async () => {
      const engine = new Engine(company());
      await loseTheAnswer(engine, { lands: true });
      expect(engine.seats.find((s) => s.handle === "ceo")!.goal).toBe("Lead and more");

      mountBuilder({ engine });
      // CARRIED ONTO WHAT LANDED, never offered as Keep: the replay finds the
      // edit already in the rows and says so.
      const restore = await screen.findByRole("dialog", { name: "Restore the kept draft" });
      expect(within(restore).getByText(/It is saved already\./)).toBeDefined();
      expect(screen.queryByRole("button", { name: "Keep the draft" })).toBeNull();
      fireEvent.click(within(restore).getByRole("button", { name: "Restore the draft" }));
      await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull());
      await screen.findByText("No problems");
      // Nothing left to save, and nothing written twice.
      expect(
        (screen.getByRole("button", { name: "Review and save" }) as HTMLButtonElement).disabled,
      ).toBe(true);
      expect(engine.requests.filter(isSeatWrite("ceo"))).toHaveLength(1);
    });

    // Given back as any kept draft is: restored at once, because this page
    // kept it (after a reload it would be offered to keep or discard).
    test("a write that did not land gives its log back, and nothing was written", async () => {
      const engine = new Engine(company());
      await loseTheAnswer(engine, { lands: false });
      expect(engine.seats.find((s) => s.handle === "ceo")!.goal).toBe("Lead");

      mountBuilder({ engine });
      await waitFor(() =>
        expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toBeUndefined(),
      );
      await screen.findByText("No problems");
      await waitFor(() =>
        expect(
          (screen.getByRole("button", { name: "Review and save" }) as HTMLButtonElement).disabled,
        ).toBe(false),
      );
      expect(engine.ledger.size).toBe(0);
    });

    // LEFT AND OPENED AGAIN WHILE THE SAVE IS STILL OUT. Deciding beside it
    // would carry the log onto a chart that is still changing under it, so
    // the page waits for its own save instead.
    test("a save still out when the lens opens again is waited for, never decided beside it", async () => {
      const engine = new Engine(company());
      let answerWrite: () => void = () => {};
      engine.script = (r, e) =>
        isSeatWrite("ceo")(r)
          ? new Promise<Response>((resolve) => {
              answerWrite = () => resolve(e.answer(r));
            })
          : null;
      const dialog = await reviewEdit(engine);
      fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(engine.requests.filter(isSeatWrite("ceo"))).toHaveLength(1));
      cleanup();

      mountBuilder({ engine });
      await screen.findByText("No problems");
      await new Promise((resolve) => setTimeout(resolve, 50));
      // Nothing restored or offered yet.
      expect(screen.getByText("read only")).toBeDefined();
      expect(screen.queryByRole("button", { name: "Keep the draft" })).toBeNull();

      // Written only now, and answered to the page that has left.
      act(() => answerWrite());
      await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull());
      await waitFor(() => expect(screen.getByText("editable")).toBeDefined());
      expect(screen.queryByRole("dialog", { name: "Restore the kept draft" })).toBeNull();
      expect(engine.requests.filter(isSeatWrite("ceo"))).toHaveLength(1);
    });
  });
});

describe("the review", () => {
  test("a company rename cannot be saved until its consequence is acknowledged", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const dialog = await openReview();
    expect(within(dialog).getByText("Renames the company from Acme to Acme Labs.")).toBeDefined();
    // And for many: every agent seat onboards again under the new id.
    expect(
      within(dialog).getByText(
        "3 seats onboard again because the company is renamed: CEO, Designer, Dev.",
      ),
    ).toBeDefined();
    const save = within(dialog).getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.click(within(dialog).getByRole("checkbox"));
    expect(save.disabled).toBe(false);
  });

  // COUNTED SENTENCES READ FOR ONE as well as for many: "1 seat onboard
  // again" is the kind of copy an operator reads as a draft nobody proofread.
  test("a consequence about a single seat agrees with its count", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    await act_("Move Dev to the top", "Dev");
    const dialog = await openReview();
    expect(
      within(dialog).getByText("1 seat onboards again because it moves to another unit: Dev."),
    ).toBeDefined();
  });

  // A NEW ADDRESS IS NOT A NEW SEAT: the review says what keeps working.
  test("a new address says the seat keeps its identity and its old address", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    await act_("Readdress Engineering", "Engineering");
    const dialog = await openReview();
    expect(
      within(dialog).getByText(
        "Engineering is addressed as engineering-two instead of engineering. It keeps its identity, and engineering goes on reaching it until something else takes that address.",
      ),
    ).toBeDefined();
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(toastText()).toContain("Saved. The engine is applying it."));
    // The rename, and the CEO's `manages:` list as the draft holds it: stated
    // whole, which is what the rename's own cascade leaves, so the plan never
    // has to predict which entries the chart moves.
    expect(engine.chartWrites()[0]!.body).toEqual({
      operations: [
        { kind: "rename", object: { kind: "unit", id: "engineering" }, to: "engineering-two" },
        {
          kind: "set_manages",
          object: { kind: "seat", id: "ceo" },
          manages: ["engineering-two"],
        },
      ],
    });
    // The seats in it and the lead's `manages` follow the unit to its new key.
    expect(engine.seats.find((s) => s.handle === "dev")!.unit).toBe("engineering-two");
    expect(engine.manages.ceo).toEqual(["engineering-two"]);
  });

  test("an empty audit summary cannot be saved where the settings are written", async () => {
    const engine = new Engine(company());
    mountBuilder({ engine });
    await screen.findByText("No problems");
    fireEvent.click(screen.getByRole("button", { name: "Rename the company" }));
    await waitFor(() => expect(engine.checks()).toHaveLength(1));
    const dialog = await openReview();
    fireEvent.click(within(dialog).getByRole("checkbox"));
    fireEvent.change(within(dialog).getByLabelText("Audit summary"), { target: { value: "  " } });
    expect(
      (within(dialog).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
});
