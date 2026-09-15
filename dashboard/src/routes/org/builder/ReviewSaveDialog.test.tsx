/**
 * Review and save: the review states the change and its consequences, the
 * save sends exactly the checked write with its preconditions and a signed
 * summary, and every answer that is not a plain success leads somewhere that
 * cannot lose or duplicate the work.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
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

    expect(
      await screen.findByText("Saved. The engine is applying it.", {
        selector: ".crewlet-toast__message",
      }),
    ).toBeDefined();
    // Said once: the toast's host is a live region of its own, and the
    // Builder's region repeating it had a screen reader read it twice.
    await new Promise((r) => setTimeout(r, 150));
    // INSIDE THE BUILDER'S OWN LAYER HOST, not at the document root: a
    // fullscreen canvas renders only its own subtree, so a toast portalled
    // past it is a Save that reports nothing.
    const toaster = document.querySelector(".crewlet-toaster")!;
    expect(toaster.textContent).toContain("Saved.");
    expect(toaster.closest(".org-builder")).not.toBeNull();
    expect(document.querySelector(".org-builder-live")!.textContent).not.toContain("Saved.");
    // The stored revision is loaded, and the kept draft is gone with the work saved.
    await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r-saved"'));
    expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  // THE SAVED REVISION IS READ BACK, NEVER OVER WORK. The save's own answer
  // keys the base, so the lens is editable while that read is out, and an
  // edit made then already stands on the saved revision.
  test("an edit made while the saved revision is read back is kept", async () => {
    const engine = new Engine(company());
    const dialog = await reviewEdit(engine);
    let readBack: () => void = () => {};
    engine.script = (r, e) =>
      r.method === "GET" && r.path === "/config" && engine.requests.some(isWrite)
        ? new Promise<Response>((resolve) => {
            readBack = () => resolve(e.answer(r));
          })
        : null;
    fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
    await screen.findByText("Saved. The engine is applying it.", {
      selector: ".crewlet-toast__message",
    });
    await screen.findByText("editable");
    fireEvent.click(screen.getByRole("button", { name: "Edit Designer" }));
    await waitFor(() =>
      expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain("Design and more"),
    );

    engine.script = () => null;
    act(() => readBack());
    await new Promise((resolve) => setTimeout(resolve, 50));
    // Still in the draft, kept, and checked against the saved revision.
    expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toContain("Design and more");
    await waitFor(() => {
      const last = engine.checks().at(-1)!;
      expect(last.headers["If-Match"]).toBe('"r-saved"');
      expect(JSON.stringify(last.body)).toContain("Design and more");
    });
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
    // Refused, so nothing was stored: the kept log carries no save to settle.
    expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toBeUndefined();
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
    expect(
      await screen.findByText("Saved. The engine is applying it.", {
        selector: ".crewlet-toast__message",
      }),
    ).toBeDefined();
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
    expect(
      await screen.findByText("Saved. The engine is applying it.", {
        selector: ".crewlet-toast__message",
      }),
    ).toBeDefined();
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

  // LEFT BEFORE THE ANSWER CAME, AND THE ANSWER NEVER CAME. The save may have
  // landed, and its kept log offered as an update onto its own revision would
  // replay every operation a second time; so the next visit settles it first.
  describe("after the lens was left", () => {
    /** Saves an edit of the CEO, leaves the lens while the save is out, and loses its answer. */
    async function loseTheAnswer(engine: Engine, { lands }: { lands: boolean }) {
      let offline = false;
      let lose: () => void = () => {};
      engine.script = (r, e) => {
        if (offline && r.method === "GET") return Promise.reject(new TypeError("offline"));
        if (!isWrite(r)) return null;
        if (lands) e.commit(r);
        return new Promise<Response>((_, reject) => {
          lose = () => {
            offline = true;
            reject(new TypeError("network connection was lost"));
          };
        });
      };
      const dialog = await reviewEdit(engine);
      fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(engine.requests.filter(isWrite)).toHaveLength(1));
      // Marked before it went, so a reload now would find it too.
      expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toMatch(/^[0-9a-f]+$/);
      cleanup();
      const asked = engine.sent("GET").length;
      lose();
      // Settling from the unmounted lens could not reach the engine either.
      await waitFor(() => expect(engine.sent("GET").length).toBeGreaterThan(asked));
      await new Promise((resolve) => setTimeout(resolve, 20));
      expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toMatch(/^[0-9a-f]+$/);
      engine.script = () => null;
    }

    test("a save that landed is found on the next visit, and its log is not offered", async () => {
      const engine = new Engine(company());
      await loseTheAnswer(engine, { lands: true });
      expect(engine.revision).toBe("r-saved");

      mountBuilder({ engine });
      expect(
        await screen.findByText(
          "The last save from this tab was stored. The engine is applying it.",
        ),
      ).toBeDefined();
      const checked = engine.checks().length;
      await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull());
      expect(engine.sent("GET", "/config/revisions/r-saved").length).toBeGreaterThan(0);
      // The lens read the saved revision when it opened, so it stands on it
      // already and nothing is read or checked over again.
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(engine.checks()).toHaveLength(checked);
      // Neither offered back nor replayed: nothing more was written.
      expect(screen.queryByRole("button", { name: "Keep the draft" })).toBeNull();
      expect(screen.queryByRole("dialog", { name: "Update my draft and review" })).toBeNull();
      expect(engine.requests.filter(isWrite)).toHaveLength(1);
    });

    // LEFT AND OPENED AGAIN WHILE THE SAVE IS STILL OUT. The engine may not
    // have stored it yet, so settling it from the history then would find
    // nothing, give the log back, and have it saved or updated a second time
    // once the first attempt landed. The page waits for its own save instead.
    test("a save still out when the lens opens again is waited for, never settled beside it", async () => {
      const engine = new Engine(company());
      let answerWrite: () => void = () => {};
      engine.script = (r, e) =>
        isWrite(r)
          ? new Promise<Response>((resolve) => {
              answerWrite = () => resolve(e.answer(r));
            })
          : null;
      const dialog = await reviewEdit(engine);
      fireEvent.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(engine.requests.filter(isWrite)).toHaveLength(1));
      cleanup();

      mountBuilder({ engine });
      await screen.findByText("No problems");
      await new Promise((resolve) => setTimeout(resolve, 50));
      // Nothing restored or offered, and nothing asked of the history yet.
      expect(screen.getByText("read only")).toBeDefined();
      expect(screen.queryByRole("button", { name: "Keep the draft" })).toBeNull();
      expect(engine.requests.some((r) => r.path.startsWith("/config/revisions/"))).toBe(false);

      // Stored only now, and answered to the page that has left.
      act(() => answerWrite());
      expect(
        await screen.findByText(
          "The last save from this tab was stored. The engine is applying it.",
        ),
      ).toBeDefined();
      await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull());
      await waitFor(() => expect(screen.getByText("editable")).toBeDefined());
      expect(screen.queryByRole("dialog", { name: "Update my draft and review" })).toBeNull();
      expect(engine.requests.filter(isWrite)).toHaveLength(1);
      // Standing on the saved revision, with nothing left to carry onto it.
      await waitFor(() => expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r-saved"'));
      expect(JSON.stringify(engine.checks().at(-1)!.body)).not.toContain("Lead and more");
    });

    // THIS VISIT NEVER HELD THE SAVED DOCUMENT. It stands on whatever it read,
    // here a colleague's revision built on the save, and settling the save
    // must not relabel that document with the save's revision: every check
    // then named a revision the document was not, and met a conflict.
    test("a save that landed under a later revision leaves the lens on the later one", async () => {
      const engine = new Engine(company());
      await loseTheAnswer(engine, { lands: true });
      engine.commit({
        method: "PATCH",
        path: "/config",
        query: new URLSearchParams(),
        headers: {},
        body: { mission: "Somebody else's", _summary: "A colleague's save" },
      });
      expect(engine.revision).toBe("r-saved-2");

      mountBuilder({ engine });
      expect(
        await screen.findByText(
          "The last save from this tab was stored. The engine is applying it.",
        ),
      ).toBeDefined();
      await screen.findByText("No problems");
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(engine.checks().map((c) => c.headers["If-Match"])).not.toContain('"r-saved"');
      expect(engine.checks().at(-1)!.headers["If-Match"]).toBe('"r-saved-2"');
      expect(screen.queryByText("The configuration changed")).toBeNull();
    });

    // Given back as any kept draft is: here restored at once, because this
    // page kept it (after a reload it would be offered to keep or discard).
    test("a save that did not land gives its log back, and nothing was stored", async () => {
      const engine = new Engine(company());
      await loseTheAnswer(engine, { lands: false });
      expect(engine.revision).toBe("r1");
      const before = engine.checks().length;

      mountBuilder({ engine });
      await waitFor(() =>
        expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toBeUndefined(),
      );
      await waitFor(() =>
        expect(
          engine
            .checks()
            .slice(before)
            .some((c) => JSON.stringify(c.body).includes("Lead and more")),
        ).toBe(true),
      );
      expect(engine.revisions.size).toBe(0);
      expect(engine.requests.filter(isWrite)).toHaveLength(1);
    });

    // Not landed, and somebody else saved since: the log is a kept draft of
    // an older revision, updated onto the newer one like any other, never
    // treated as this visit's own save that met a conflict.
    test("a save that did not land while the company moved on is updated as a kept draft", async () => {
      const engine = new Engine(company());
      await loseTheAnswer(engine, { lands: false });
      engine.commit({
        method: "PATCH",
        path: "/config",
        query: new URLSearchParams(),
        headers: {},
        body: { mission: "Somebody else's", _summary: "A colleague's save" },
      });

      mountBuilder({ engine });
      const update = await screen.findByRole("dialog", { name: "Restore the kept draft" });
      expect(within(update).getByText("Every change still applies.")).toBeDefined();
      expect(JSON.parse(sessionStorage.getItem(DRAFT_STORAGE_KEY)!).write).toBeUndefined();
      const before = engine.checks().length;
      fireEvent.click(within(update).getByRole("button", { name: "Restore the draft" }));
      // Replayed onto the colleague's revision, and checked there.
      await waitFor(() => {
        const after = engine.checks().slice(before);
        expect(after.some((c) => JSON.stringify(c.body).includes("Lead and more"))).toBe(true);
        expect(after.every((c) => c.headers["If-Match"] === '"r-saved"')).toBe(true);
      });
      expect(engine.requests.filter(isWrite)).toHaveLength(1);
      // Nothing of this visit was being saved, so nothing goes on to a review
      // or to a second update of a draft it never had on screen.
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(screen.queryByRole("dialog")).toBeNull();
    });

    test("while the engine still cannot say, the lens waits and edits nothing", async () => {
      const engine = new Engine(company());
      await loseTheAnswer(engine, { lands: true });
      engine.script = (r) =>
        r.path.startsWith("/config/revisions/") ? json({ error: "internal" }, 500) : null;

      mountBuilder({ engine });
      expect(
        await screen.findByText(
          "The engine did not confirm whether the last save was stored. Editing is paused until it does.",
        ),
      ).toBeDefined();
      expect(screen.getByText("read only")).toBeDefined();
      // There is no draft on screen to review, only the save to settle.
      expect(screen.queryByRole("button", { name: "Open the review" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Keep the draft" })).toBeNull();

      engine.script = () => null;
      fireEvent.click(screen.getByRole("button", { name: "Check again" }));
      expect(
        await screen.findByText(
          "The last save from this tab was stored. The engine is applying it.",
        ),
      ).toBeDefined();
      await waitFor(() => expect(sessionStorage.getItem(DRAFT_STORAGE_KEY)).toBeNull());
    });
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
    expect(
      await screen.findByText("Saved. The engine is applying it.", {
        selector: ".crewlet-toast__message",
      }),
    ).toBeDefined();
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
    // And for many: every agent seat onboards again under the new id.
    expect(
      within(dialog).getByText(
        "3 seats onboard again because the company is renamed: CEO, Designer, Dev.",
      ),
    ).toBeDefined();
    const save = within(dialog).getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    // The consequence as the engine keys it: the agent id moves, the handle
    // (and with it the mailbox) does not.
    fireEvent.click(
      within(dialog).getByRole("checkbox", {
        name: "An agent seat's id is derived from the company name and its handle, so renaming the company gives every agent seat a new id: each seat's diary and onboarding progress stay under the old id and are no longer read, and every agent seat onboards again. Handles, mailboxes and episodes are unchanged.",
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
        "1 seat onboards again because a unit it belongs to is renamed: Dev.",
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
