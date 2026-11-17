/**
 * An artifact a reference names is read once per tab, whatever number of
 * references name it; a read that failed to answer is asked again by the
 * next reference, and an answer about the artifact stands.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { InertWebSocket } from "~/test/client.tsx";
import { settled } from "~/test/settle.ts";
import { useArtifactHeadline } from "./heads.ts";

afterEach(cleanup);

function Said({ id }: { id: string }) {
  const read = useArtifactHeadline(id);
  return (
    <span>
      {read === undefined ? "reading" : read === null ? "none" : `${read.kind} ${read.title}`}
    </span>
  );
}

function client(answer: (id: string) => Promise<unknown>) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  const store = new Store();
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  socket.query = ((what: string, params?: Record<string, unknown>) => {
    asked.push(`${what} ${String(params?.["id"])}`);
    return answer(String(params?.["id"]));
  }) as typeof socket.query;
  return { value: { store, socket }, asked };
}

test("many references to one artifact ask once and all draw its answer", async () => {
  const { value, asked } = client(() => Promise.resolve({ kind: "doc", title: "Plan" }));
  render(
    <ClientContext.Provider value={value}>
      <Said id="a" />
      <Said id="a" />
      <Said id="a" />
    </ClientContext.Provider>,
  );
  await settled(() => asked.length);
  expect(asked).toEqual(["artifact a"]);
  expect(screen.getAllByText("doc Plan")).toHaveLength(3);
});

test("a failure to answer is asked again by the next reference; an answer about the artifact is not", async () => {
  let unavailable = true;
  const { value, asked } = client((id) =>
    id === "gone"
      ? Promise.reject(new Error("not_found"))
      : unavailable
        ? Promise.reject(new Error("unavailable"))
        : Promise.resolve({ kind: "sheet", title: "Budget" }),
  );
  const first = render(
    <ClientContext.Provider value={value}>
      <Said id="a" />
      <Said id="gone" />
    </ClientContext.Provider>,
  );
  await settled(() => asked.length);
  expect(screen.getAllByText("none")).toHaveLength(2);
  first.unmount();
  unavailable = false;
  render(
    <ClientContext.Provider value={value}>
      <Said id="a" />
      <Said id="gone" />
    </ClientContext.Provider>,
  );
  await settled(() => asked.length);
  expect(asked).toEqual(["artifact a", "artifact gone", "artifact a"]);
  expect(screen.getByText("sheet Budget")).toBeTruthy();
});
