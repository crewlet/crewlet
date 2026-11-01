/**
 * Asking an agent from a create page: a real task, filed to the chosen seat in
 * the chosen project, whose description carries the brief and the kind; an
 * artifact already made is linked to it after it is filed, never before; and
 * the who-builds-it block offers the agents as their characters with the
 * project following the agent.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { useEffect, type ReactNode } from "react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { FrameReadings } from "~/app/Shell.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { reloadForTest } from "~/lib/prefs.ts";
import { LiveSocket, Store, type WorkProjectRow } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";
import {
  artifactTaskBody,
  notAttached,
  useFileArtifactTask,
  type ArtifactTaskRequest,
  type FiledArtifactTask,
} from "./fileTask.ts";
import { WhoBuildsIt, useWhoBuildsIt, type WhoBuildsItState } from "./WhoBuildsIt.tsx";
import { afterMicrotasks } from "~/test/settle.ts";

const linked: { id: string; workItem: string; after?: string }[] = [];
let linkAnswer: { kind: "done"; body: Record<string, unknown> } | { kind: "refused"; text: string };

vi.mock("~/lib/keyedWrite.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/keyedWrite.ts")>("~/lib/keyedWrite.ts");
  return {
    ...actual,
    keyedWrite: vi.fn(
      async (req: { path: string; body?: { work_item?: string; after?: string } }) => {
        linked.push({
          id: req.path.split("/")[2] ?? "",
          workItem: req.body?.work_item ?? "",
          ...(req.body?.after ? { after: req.body.after } : {}),
        });
        return linkAnswer;
      },
    ),
  };
});

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet", "artifacts"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["create_work_item"],
  project: "OPS",
};

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Agent PM", handle: "pm", kind: "agent", goal: "Own the roadmap" },
    { name: "Agent Designer", handle: "designer", kind: "agent", goal: "Brand and UX design" },
  ],
  units: [],
};

const project = (key: string): WorkProjectRow =>
  ({ key, name: key, unit: { resolved: true }, lead: { handle: "jane" } }) as WorkProjectRow;

let posted: { tool: string; args: Record<string, unknown> }[];

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  posted = [];
  linked.length = 0;
  linkAnswer = { kind: "done", body: {} };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const tool = decodeURIComponent(String(url).split("/operator/act/")[1] ?? "");
      posted.push({ tool, args: (JSON.parse(init.body as string) as { args: never }).args });
      return new Response(
        JSON.stringify({
          tool,
          outcome: "applied",
          position: "CREWLET_TRACKER_LOG@1:9",
          receipt: { id: "0f0f", item: "DES-4", key: "DES-4" },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  reloadForTest();
  location.hash = "#/";
});

function mount(children: ReactNode, viewer: Record<string, unknown> = JANE) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  store.applyOrg(withDerived(ORG) as never);
  const socket = new LiveSocket(store);
  socket.query = ((what: string) => {
    const answers: Record<string, unknown> = {
      viewer,
      work_projects: { projects: [project("DES"), project("OPS")] },
      inbox: { handle: "jane", notices: [], unread: 0, primary: 0 },
    };
    return Promise.resolve(answers[what] ?? {});
  }) as typeof socket.query;
  return render(
    <ToastProvider>
      <LayerHost>
        <ClientContext.Provider value={{ store, socket }}>
          <FrameReadings>
            <Router>{children}</Router>
          </FrameReadings>
        </ClientContext.Provider>
      </LayerHost>
    </ToastProvider>,
  );
}

async function settle() {
  await afterMicrotasks();
}

/**
 * A page's ask as a control draws it: the action, held with the hook's reason
 * for a reader who cannot file, as every create page passes `held` to its
 * action's `disabledReason`.
 */
function Filer({
  request,
  onFiled,
}: {
  request: ArtifactTaskRequest;
  onFiled: (filed: FiledArtifactTask | null) => void;
}) {
  const { fileArtifactTask, held } = useFileArtifactTask();
  return (
    <>
      <button type="button" onClick={() => void fileArtifactTask(request).then(onFiled)}>
        File
      </button>
      {held && <p role="note">{held}</p>}
    </>
  );
}

async function file(
  request: ArtifactTaskRequest,
  viewer: Record<string, unknown> = JANE,
): Promise<FiledArtifactTask | null> {
  let filed: FiledArtifactTask | null = null;
  mount(<Filer request={request} onFiled={(f) => (filed = f)} />, viewer);
  await settle();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "File" }));
  });
  await settle();
  return filed;
}

// EACH KIND WITH ITS OWN ARTICLE, as it is said: "an HTML artifact".
test.each([
  ["html", "an HTML artifact (kind `html`)"],
  ["doc", "a Doc artifact (kind `doc`)"],
  ["slides", "a Slides artifact (kind `slides`)"],
  ["dashboard", "a Dashboard artifact (kind `dashboard`)"],
  ["sheet", "a Sheet artifact (kind `sheet`)"],
  ["design", "a Design artifact (kind `design`)"],
] as const)("a new %s is asked for with its article", (kind, phrase) => {
  expect(artifactTaskBody(kind, "Brief.")).toBe(`Brief.\n\nPublish it as ${phrase} for this task.`);
});

test("the description is the brief, then what to publish and where", () => {
  expect(artifactTaskBody("design", "  Three onboarding screens.  ")).toBe(
    "Three onboarding screens.\n\nPublish it as a Design artifact (kind `design`) for this task.",
  );
  expect(artifactTaskBody("dashboard", "Weekly signups.", "a1")).toBe(
    "Weekly signups.\n\nBuild it in the Dashboard artifact already linked to this task (artifact a1), and publish your work as new versions of it.",
  );
});

// WHERE A NEW ONE IS FILED, by the id a rename cannot change and never by its
// path, which would tell whoever reads the task the names in a private
// folder; an artifact already made was filed when it was made.
test("the description names the folder a new artifact is filed in by its id alone", () => {
  expect(artifactTaskBody("doc", "Notes.", undefined, "f1")).toBe(
    "Notes.\n\nPublish it as a Doc artifact (kind `doc`) for this task. File it in the folder whose id is f1 (`folder` on `create_artifact`).",
  );
  expect(artifactTaskBody("design", "Screens.", "a1", "f1")).not.toContain("f1");
});

// A REAL TASK, as the person: the tracker's own create, filed to the seat in
// the project, and nothing linked when no artifact was named.
test("files one task to the agent in the project, and returns its address and key", async () => {
  const filed = await file({
    kind: "design",
    title: " Onboarding screens ",
    brief: "Three screens.",
    seat: "designer",
    project: "DES",
  });
  expect(posted).toEqual([
    {
      tool: "create_work_item",
      args: {
        title: "Onboarding screens",
        project: "DES",
        assignee: "designer",
        body: artifactTaskBody("design", "Three screens."),
      },
    },
  ]);
  expect(filed).toEqual({ item: "DES-4", key: "DES-4", linked: null });
  expect(linked).toEqual([]);
});

test("a task with no project is filed in the person's own", async () => {
  await file({ kind: "html", title: "Calculator", brief: "A loan calculator.", seat: "pm" });
  expect(posted[0]?.args.project).toBe("OPS");
});

test("an unattached artifact is named by its kind's word", () => {
  expect(notAttached("html")).toBe(
    "The task was filed, but the HTML page could not be attached to it. Attach it from the task.",
  );
  expect(notAttached("slides")).toContain("the slides could not be attached");
});

// THE TASK FIRST, THEN THE LINK: the artifact names the key the create minted.
test("an artifact already made is linked to the new task by its key", async () => {
  const filed = await file({
    kind: "dashboard",
    title: "Signups",
    brief: "Weekly signups.",
    seat: "pm",
    project: "DES",
    artifact: "a1",
  });
  // AT THE CREATE'S POSITION, so the engine reads the task the create made.
  expect(linked).toEqual([{ id: "a1", workItem: "DES-4", after: "CREWLET_TRACKER_LOG@1:9" }]);
  expect(filed?.linked).toBe(true);
});

// A LINK THAT DID NOT LAND IS SAID IN WORDS, never the request's detail: the
// sandbox's answer for a route it does not serve named the method and path.
test("a link the engine refused is said in words, and the filed task is still returned", async () => {
  linkAnswer = {
    kind: "refused",
    text: "This node serves nothing at POST /artifacts/a2/work-items.",
  };
  const filed = await file({
    kind: "design",
    title: "Screens",
    brief: "Two screens.",
    seat: "designer",
    artifact: "a2",
  });
  expect(filed).toEqual({
    item: "DES-4",
    key: "DES-4",
    linked:
      "The task was filed, but the design could not be attached to it. Attach it from the task.",
  });
  expect(String(filed!.linked)).not.toMatch(/POST|\/artifacts/);
});

// THE ASK IS HELD, NEVER HIDDEN, for every reader who cannot file: the hook
// says why, for the page's action to say it, and a press sends nothing.
test.each([
  {
    who: "anonymous",
    viewer: { login: "", grants: [], handle: "", owner: "", name: "", acts: [] },
    reason: WRITE_REASONS.anonymous,
  },
  {
    who: "not served the create",
    viewer: { ...JANE, acts: ["update_work_item"] },
    reason: WRITE_REASONS.not_served,
  },
])("a reader who is $who is told why, and nothing is filed", async ({ viewer, reason }) => {
  const filed = await file(
    { kind: "design", title: "Screens", brief: "Two.", seat: "designer", artifact: "a3" },
    viewer,
  );
  expect(screen.getByRole("note").textContent).toBe(reason);
  expect(filed).toBeNull();
  expect(posted).toEqual([]);
  expect(linked).toEqual([]);
});

function Who({ onState }: { onState: (who: WhoBuildsItState) => void }) {
  const who = useWhoBuildsIt("design");
  useEffect(() => {
    onState(who);
  });
  return <WhoBuildsIt who={who} />;
}

// THE AGENTS AS THEIR CHARACTERS, the one whose work the kind is first and
// chosen, and the project the reader's own until they pick one.
test("who builds it offers the agents, the suggested one first and chosen", async () => {
  let state: WhoBuildsItState | null = null;
  mount(<Who onState={(who) => (state = who)} />);
  await settle();
  const radios = screen.getAllByRole("radio") as HTMLInputElement[];
  expect(radios.map((r) => r.value)).toEqual(["designer", "pm"]);
  expect(radios[0]!.checked).toBe(true);
  expect(screen.getByText("Suggested")).toBeTruthy();
  expect(state!.seat).toBe("designer");
  expect(state!.project).toBe("OPS");
  fireEvent.click(radios[1]!);
  await settle();
  expect(state!.seat).toBe("pm");
});
