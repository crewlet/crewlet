/**
 * The file-storage card on Settings › Nodes: what it says of the store and of
 * the collector's last passes.
 */

import { afterEach, describe, expect, test } from "vitest";

import { cleanup, render, screen } from "@testing-library/react";

import { auditSummary, backendSummary, collectSummary, FileStorage } from "./FileStorage.tsx";
import { OBJECTS_STATES } from "~/contract/fleet.ts";
import { Router } from "~/app/router.tsx";
import { engineFile } from "~/test/engineFiles.ts";
import type { FleetObjects, ReportedObjects } from "~/protocol/types.ts";

/**
 * THE CARD RENDERS THE ENGINE'S OWN ANSWERS: every block below is read from
 * the golden the Go renderer writes (`internal/api/testdata/objects_answer.json`),
 * so a fixture typed here cannot agree with the card while the engine sends
 * something else.
 */
const golden = engineFile<{ fleet: Record<string, FleetObjects> }>(
  "internal/api/testdata/objects_answer.json",
);
function block(name: string): FleetObjects {
  const b = golden.fleet[name];
  if (!b) throw new Error(`the golden has no block named ${name}`);
  return structuredClone(b);
}
function reported(name: string): ReportedObjects {
  const b = block(name);
  if (b.state !== "reported") throw new Error(`${name} is not a report`);
  return b;
}

describe("file storage", () => {
  afterEach(cleanup);
  const now = Date.parse("2026-09-01T12:05:00Z");

  function renderCard(objects?: FleetObjects) {
    // IN A ROUTER, because the node that ran the passes links to its page.
    return render(
      <Router>
        <FileStorage objects={objects} now={now} />
      </Router>,
    );
  }

  test("every state the golden carries is one the contract names", () => {
    for (const b of Object.values(golden.fleet)) {
      expect(OBJECTS_STATES).toContain(b.state);
    }
    // And every state is exercised, so a new one fails here until the card
    // draws it.
    expect(new Set(Object.values(golden.fleet).map((b) => b.state))).toEqual(
      new Set(OBJECTS_STATES),
    );
  });

  test("a report names the store, the node that ran it, and both passes", () => {
    renderCard(block("reported"));
    expect(screen.getByText("S3 bucket https://s3.example.com/files/acme/")).toBeTruthy();
    expect(screen.getByText("data-a")).toBeTruthy();
    expect(screen.getByText(/deleted 12 chunks no file names, of 1,840 stored/)).toBeTruthy();
    expect(screen.getByText("1,828 named, none missing")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  test("missing chunks are an alert naming them, to restore first", () => {
    const r = reported("missing");
    renderCard(r);
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain(
      "1 chunk the company's files are made of is not in the store",
    );
    for (const h of r.audit?.missing_chunks ?? []) {
      expect(alert.textContent).toContain(h);
    }
    expect(
      screen.getByText("the fleet's own NATS bucket, replicated at stream.replicas"),
    ).toBeTruthy();
  });

  test("a collection that could not read the whole estate says it deleted nothing, and why", () => {
    renderCard(block("skipped"));
    expect(screen.getByText(/^deleted nothing: a record this node could not apply/)).toBeTruthy();
    // No audit has ended: the row says so rather than drawing a zero.
    expect(screen.getByText("Not run yet")).toBeTruthy();
  });

  test("a record the store would not give up is not a fleet with no collection", () => {
    renderCard(block("unavailable"));
    expect(screen.getByRole("alert").textContent).toContain("could not be read");
    cleanup();
    renderCard(block("not_yet"));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("No collection yet")).toBeTruthy();
  });

  test("a node that reads no record draws no card", () => {
    const { container } = renderCard(undefined);
    expect(container.textContent).toBe("");
  });

  test("an incomplete audit's count is a floor, and a failed pass says what stopped it", () => {
    expect(auditSummary({ at: "", completed: false, referenced: 10, missing: 2 })).toBe(
      "10 named so far, at least 2 missing",
    );
    expect(
      collectSummary({
        at: "",
        completed: false,
        listed: 0,
        aged: 0,
        deleted: 0,
        referenced: 0,
        refreshed: 0,
        error: "the bucket did not answer",
      }),
    ).toBe("stopped: the bucket did not answer");
    expect(backendSummary("gcs:x")).toBe("gcs:x");
  });
});
