/**
 * The person shell's sidebar: where a person goes, and the few things they
 * keep.
 *
 * # What is on it, and in what order
 *
 * The order is the argument (`nav.ts`): YOU first — Hand off, which is what a
 * person does from anywhere, then their day, what reached them and what is
 * theirs — then the COMPANY under its own name: its work, its team and what
 * it knows. Then the reader's FAVOURITES — the views they pinned and the pages
 * they starred, one list, because both are "a place I keep" — and at the foot
 * the door to the console, for an admin, and who this browser is.
 *
 * Nothing about the machine. The engine's health, the running turns and the
 * console's sections are the console's sidebar (`console/ConsoleSidebar.tsx`),
 * and the only trace of the engine here is a line that says the connection is
 * in trouble, drawn only while it is — a person's screen is not a status page.
 *
 * # Projects live under Work
 *
 * The Work row carries a disclosure and the reader's own projects nest under
 * it (`projects.ts` says which), then "All projects". It is open while the
 * route is inside Work and closed everywhere else, until the reader presses
 * the chevron — which this browser then remembers, as a preference and never
 * in the address. The rows are the reader's, not the company's: a sidebar is
 * where a person goes, not a second list of the company's things.
 *
 * # Every figure says what it counts
 *
 * The kit's `NavItem` takes a figure only with the words that name it. The
 * Inbox's BADGE is how many things are waiting on the reader — the one accent
 * fill in the chrome. My work and Work carry no figure: what is on a person
 * is the Queue's tab, and a count that sits beside a row on every screen is a
 * mark a reader learns to skip.
 *
 * # The admin door is an admin's
 *
 * Drawn for a reader the engine says is an admin, and for nobody else — a
 * member has no console to open, and a locked door on every screen of theirs
 * would be a row about somebody else's job. Its code is the console's own
 * chunk (`console/AdminDoor.tsx`), so a member's browser never downloads the
 * read it makes.
 */

import { useMemo, useSyncExternalStore } from "react";
import { AppShell, BrandLockup, Button, Kbd, SidebarNav, useAppShell } from "@crewlethq/ui";
import { ListFilterGlyph, SendGlyph, StarGlyph } from "@crewlethq/icons/glyphs";
import { href, samePath, useRoute } from "~/app/router.tsx";
import { workspaceOf, workspaceRow, workspacesOf, type WorkspaceRow } from "~/app/nav.ts";
import { glyphFor } from "~/ui/glyph.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { viewRun } from "~/lib/work.ts";
import { routeProject } from "../palette/hits.ts";
import { useOpenNewTask } from "../newTask.ts";
import { useStarred } from "~/lib/starred.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { capsOf, keyRow, useKeymap } from "../keymap.ts";
import { inboxFigure, useInboxCounts, waitingWords } from "~/lib/useInboxCounts.ts";
import { RailBoundary } from "../boundaries.tsx";
import { lazyScreen } from "../lazyScreen.ts";
import { ConnectionLine } from "./ConnectionLine.tsx";
import { NavRow } from "./NavRow.tsx";
import { UserBlock } from "./UserBlock.tsx";
import { yourProjects } from "./projects.ts";

/** How many projects the sidebar asks for: the engine's own page of them. */
const PROJECTS_PAGE = 200;

/** The door to the console, out of the console's chunk — see the file's doc. */
const AdminDoor = lazyScreen("console", (module) => module.AdminDoor);

export function PersonSidebar({ onSetToken }: { onSetToken: () => void }) {
  const route = useRoute();
  // THE FRAME'S READINGS, not the sidebar's own: see `FrameReadings`.
  const viewer = useViewer();

  // BELOW THE SHELL BREAKPOINT THE SIDEBAR IS A DRAWER, and Mod+\ opens it —
  // the reader's way to it without reaching for the toggle in the bar. Only
  // there: above the breakpoint the sidebar is always on screen and there is
  // nothing to open. It OPENS rather than toggles, because an open drawer is
  // a modal layer that owns the keyboard (`lib/keys.ts` stands aside for
  // one), and the drawer's own Escape is what closes it.
  const shell = useAppShell();
  useKeymap({ drawer: { run: shell.openDrawer, when: shell.narrow } });
  const org = useOrg();
  const openNewTask = useOpenNewTask();
  const inbox = useInboxCounts();
  const here = workspaceOf(route.path);
  const at = route.path.join("/");
  const index = useMemo(() => indexOrg(org), [org]);
  const seatName = index.byHandle.get(viewer.handle)?.name ?? "";

  const row = (ws: WorkspaceRow) => (
    <NavRow
      key={ws.key}
      label={ws.label}
      icon={ws.icon}
      path={ws.path}
      current={here === ws.key}
      {...(ws.key === "inbox" && inbox.waiting !== null && inbox.waiting > 0
        ? // THE FIGURE'S ONE NAME, which the Inbox's own line says too: never
          // "unread", which on that screen is every unread notice it lists.
          { badge: { value: inboxFigure(inbox), label: waitingWords(inbox) } }
        : {})}
    />
  );
  const rows = workspacesOf("person");

  return (
    <AppShell.Rail
      header={
        <div className="side-head">
          <BrandLockup
            // THE COMPANY IS THE TITLE, beside the product's mark — the lockup
            // the approved design draws ("Nimbus", no second line). On this
            // screen the company is the subject, one browser is one company,
            // and the product is in the mark and the tab. "Crewlet" stands in
            // only while no company has been sent.
            name={org?.name || "Crewlet"}
            href={href(["today"])}
            // A LITERAL PATH under the dashboard's own `base`, which the dev
            // server serves itself — written out so `protocol/proxy.test.ts`
            // can read it and hold it against the proxy table.
            mark={<img src="/static/dashboard/crewlet-icon.svg" alt="" />}
          />
        </div>
      }
      footer={
        <>
          <ConnectionLine onSetToken={onSetToken} />
          {viewer.admin && (
            <RailBoundary label="Admin console" resetKey={at}>
              <AdminDoor />
            </RailBoundary>
          )}
          <RailBoundary label="You" resetKey={at}>
            <UserBlock viewer={viewer} seatName={seatName} onSetToken={onSetToken} />
          </RailBoundary>
        </>
      }
    >
      {/* THE ONE CREATE THE CHROME CARRIES, at the top because handing work
          off is what a person does from anywhere. It files into the project
          on screen when there is one — the sheet says where before anything is
          sent. NOT GATED HERE: the sheet's own Create is the write control,
          and it says why a reader who cannot file cannot. */}
      <div className="side-handoff">
        <Button
          className="side-handoff-button"
          variant="primary"
          size="small"
          leadingIcon={<SendGlyph size="sm" />}
          onClick={() => openNewTask({ project: routeProject(route.path) || undefined })}
          aria-keyshortcuts="C"
        >
          <span className="side-handoff-label">Hand off</span>
          <Kbd keys={capsOf(keyRow("handoff").presses[0]!)} subtle />
        </Button>
      </div>
      <SidebarNav label="Navigation">
        <SidebarNav.Group>{rows.filter((ws) => ws.place === "you").map(row)}</SidebarNav.Group>
        {/* THE COMPANY'S OWN NAME heads its group: on this screen the company
            is the subject, and "Workspace" was a word about the product. */}
        <SidebarNav.Group label={org?.name || "Company"}>
          {rows
            .filter((ws) => ws.place === "company")
            .map((ws) =>
              ws.key === "work" ? (
                <RailBoundary key={ws.key} label="Work" resetKey={at}>
                  <WorkRow row={ws} path={route.path} here={here === "work"} />
                </RailBoundary>
              ) : (
                row(ws)
              ),
            )}
        </SidebarNav.Group>
        {/* EACH LIVE SECTION HAS ITS OWN BOUNDARY: it draws rows the engine
            or the browser's storage sent, and a malformed one must cost that
            list and never the navigation above it. */}
        <RailBoundary label="Favourites" resetKey={at}>
          <Favourites
            path={route.path}
            viewKey={route.query.get("view") ?? ""}
            viewer={viewer.handle}
          />
        </RailBoundary>
      </SidebarNav>
    </AppShell.Rail>
  );
}

/**
 * The Work row, and the reader's projects under it.
 *
 * OPEN WHILE THE ROUTE IS INSIDE WORK, closed elsewhere — until the reader
 * presses the chevron, after which their choice holds everywhere, in this
 * browser ([useWorkOpen]). Only ONE row is current: the deepest that matches,
 * so on a project's task the project's row is marked and Work is not.
 */
function WorkRow({ row, path, here }: { row: WorkspaceRow; path: string[]; here: boolean }) {
  const viewer = useViewer();
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const [chosen, choose] = useWorkOpen();
  const open = chosen ?? here;
  // THE TWO READS ARE ASKED ONLY WHILE THE ROWS ARE DRAWN: a closed row
  // shows no project, and a poll for rows nobody can see is a cost the reader
  // pays on every screen.
  const projects = useQuery(
    "work_projects",
    { limit: PROJECTS_PAGE },
    { enabled: open, pollMs: 120_000 },
  );
  // WHERE THE READER HOLDS OPEN WORK, one column per project — the board's own
  // grouping over their own queue, so "a project I am working in" is the
  // tracker's answer rather than a page of rows counted here.
  const held = useQuery(
    "work_items",
    viewer.handle
      ? {
          container: "workspace",
          assignee: viewer.handle,
          subtasks: "separate",
          group_by: "project",
          group_limit: 1,
        }
      : undefined,
    { enabled: open && viewer.handle !== "", pollMs: 120_000 },
  );
  const holding = useMemo(
    () => new Set((held.data?.groups ?? []).filter((g) => g.count > 0).map((g) => g.key)),
    [held.data],
  );
  const mine = useMemo(
    () =>
      yourProjects({
        projects: projects.data?.projects ?? [],
        handle: viewer.handle,
        units: (index.byHandle.get(viewer.handle)?.unitChain ?? []).map((u) => u.name),
        line: viewer.line,
        holding,
      }),
    [projects.data, index, viewer.handle, viewer.line, holding],
  );
  // THE PROJECT A READER IS INSIDE — its page, or an item filed in it — by
  // the route's own resolver (`routeProject`), the one the New task sheet
  // asks too.
  const inside = routeProject(path);
  const all = samePath(path, ["work", "projects"]);
  const nested = open && (mine.some((p) => p.key === inside) || all);
  return (
    <NavRow
      label={row.label}
      icon={row.icon}
      path={row.path}
      current={here && !nested}
      expanded={open}
      onExpandedChange={choose}
      expandLabel={open ? "Hide projects" : "Show projects"}
    >
      {open && (
        <>
          {mine.map((p) => (
            <NavRow
              key={p.key}
              label={
                <span className="side-project">
                  <span className="side-project-name truncate">{p.name || p.key}</span>
                  <span className="side-project-key">{p.key}</span>
                </span>
              }
              path={["work", p.key]}
              current={inside === p.key}
            />
          ))}
          <NavRow label="All projects" path={["work", "projects"]} current={all} />
        </>
      )}
    </NavRow>
  );
}

/**
 * Whether the reader chose to show the Work row's projects: true, false, or
 * null where they never pressed the chevron and the route decides.
 *
 * A PREFERENCE OF THIS BROWSER, never the address: which rows a sidebar shows
 * is not a place, and a link pasted to a colleague should not open their
 * sidebar the way the sender left theirs. Every storage access is guarded —
 * a private window throws on the accessor.
 */
function useWorkOpen(): [boolean | null, (open: boolean) => void] {
  const value = useSyncExternalStore(subscribeWorkOpen, readWorkOpen, () => null);
  return [value, writeWorkOpen];
}

const workOpenListeners = new Set<() => void>();

function subscribeWorkOpen(listener: () => void): () => void {
  workOpenListeners.add(listener);
  return () => workOpenListeners.delete(listener);
}

function readWorkOpen(): boolean | null {
  try {
    const stored = localStorage.getItem(STORAGE_KEYS.workProjects);
    return stored === "open" ? true : stored === "closed" ? false : null;
  } catch {
    return null;
  }
}

function writeWorkOpen(open: boolean): void {
  try {
    localStorage.setItem(STORAGE_KEYS.workProjects, open ? "open" : "closed");
  } catch {
    // A browser that keeps nothing still toggles for this page's life.
  }
  for (const listener of workOpenListeners) listener();
}

/**
 * The places this reader keeps: the views they pinned, then the pages they
 * starred. ONE LIST, because both are the same gesture — "keep this one
 * click away" — and two groups of it was a distinction a reader had to learn.
 *
 * PINS ARE ASKED WITH THE VIEWER, and across every container
 * (`work_saved_views`): a pin is a person's, and a view pinned from a project
 * board lives in that project. A pin RUNS the view, with the count the engine
 * ran beside it; a view that no longer compiles carries a refusal instead,
 * and draws no figure rather than a wrong one.
 *
 * DRAWN ONLY WHEN THERE IS ONE. There is nothing to say about a reader who has
 * kept nothing; the controls that fill it are the star in the page header and
 * the pin on a view.
 */
function Favourites({
  path,
  viewKey,
  viewer,
}: {
  path: string[];
  /** The `view=` the reader is on, which is what marks a pinned row current. */
  viewKey: string;
  viewer: string;
}) {
  const views = useQuery("work_saved_views", viewer ? { viewer, counts: true } : undefined, {
    enabled: viewer !== "",
    pollMs: 120_000,
  });
  const stars = useStarred();
  const pinned = (views.data?.views ?? []).filter((v) => v.pinned && v.id);
  if (pinned.length === 0 && stars.length === 0) return null;
  return (
    <SidebarNav.Group label="Favourites">
      {pinned.map((v) => {
        const run = viewRun(v);
        return (
          <NavRow
            key={v.id}
            label={v.name}
            glyph={<ListFilterGlyph size="sm" />}
            lead={v.container.kind === "project" ? v.container.id : undefined}
            path={run.path}
            query={run.query}
            current={samePath(path, run.path) && viewKey === v.key}
            count={
              v.count === undefined
                ? undefined
                : { value: v.count_capped ? `${v.count}+` : v.count, label: "tasks" }
            }
          />
        );
      })}
      {stars.map((s) => {
        // THE WORKSPACE'S OWN GLYPH, so a starred page reads as a page and a
        // starred task as a task; a star kept under a workspace this build
        // no longer has is still a star.
        const icon = workspaceRow(s.workspace as WorkspaceRow["key"])?.icon;
        const Glyph = icon ? glyphFor(icon) : undefined;
        return (
          <NavRow
            key={s.path.join("/")}
            label={s.label}
            glyph={Glyph ? <Glyph size="sm" /> : <StarGlyph size="sm" />}
            path={s.path}
            current={samePath(path, s.path)}
          />
        );
      })}
    </SidebarNav.Group>
  );
}
