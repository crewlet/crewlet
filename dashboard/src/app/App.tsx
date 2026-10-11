/**
 * Route dispatch: what `app/routes.ts` resolved, as a component.
 *
 * THE TABLE IS NOT HERE. Which paths are pages, which segment is a key and
 * which an id, what a bare `#/admin/live/traces` is — all of that is
 * `routes.ts`'s `resolve`, a pure function the breadcrumb, the stars, the
 * recents, the palette and the tests call too. This file is the one place a
 * resolved screen becomes a component, and a `switch` over the resolved
 * union is exhaustive: a screen added to the union and not drawn here is a
 * compile error rather than a blank page.
 *
 * EVERY SCREEN IS A LAZY CHUNK, one per workspace (`lazyScreen.ts` says why
 * the flat import list this replaced was the wrong trade), so the names below
 * are components that suspend until their workspace's code is in. The routed
 * screen sits inside `ScreenBoundary` (`boundaries.tsx`): a suspense boundary
 * drawing a skeleton while the chunk loads, and an error boundary that takes
 * down only the screen when it throws or its chunk never arrives.
 *
 * # The console is an admin's, and the frame says so
 *
 * Every address under `#/admin` is the console's, and the console is what the
 * machine processed and how it is run (ADR-0031). A reader the engine has said
 * is not an admin is drawn ONE refusal in place of any console screen —
 * `AdminRequired`, never Not Found, because the address is right and it is
 * the key that does not reach it — and the console's chunk is never fetched
 * for them. The first viewer answer is waited for: until it is in, a console
 * screen asks nothing it may be refused.
 *
 * # `#/` is the reader's own
 *
 * The landing resolves to no screen of its own: once the viewer has answered,
 * it is REPLACED by the address `nav.ts`'s `landingFor` chooses for this
 * reader, so the address bar always names the page on screen and Back never
 * lands on a bare `#/` that would move again.
 *
 * # Keyed on the subject, every screen that has one
 *
 * A hash change re-renders this switch rather than remounting it, so
 * `#/admin/live/turns/A` → `#/admin/live/turns/B` reconciles: React keeps the
 * same component instance and every piece of per-SUBJECT state in it outlives
 * the subject it describes. A disclosure left open, a tab left selected and a
 * refusal left on screen are all the same bug waiting for somebody to notice.
 */

import { useEffect, type ReactNode } from "react";
import { Shell } from "./Shell.tsx";
import { LayerHost, Skeleton, ToastProvider } from "@crewlethq/ui";
import { useNavigator, useRoute } from "./router.tsx";
import { resolve, type Resolved } from "./routes.ts";
import { landingFor, sectionOf, shellOf, workspaceRow, type Shell as ShellName } from "./nav.ts";
import { AdminRequired } from "./frame/AdminRequired.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { NotFound } from "~/routes/NotFound.tsx";
import { lazyScreen, prefetchOnIdle } from "./lazyScreen.ts";
import { ScreenBoundary } from "./boundaries.tsx";

// ------------------------------------------------------------ person shell
const Today = lazyScreen("today", (m) => m.Today);
const Inbox = lazyScreen("inbox", (m) => m.Inbox);
const MyWork = lazyScreen("me", (m) => m.MyWork);
const Work = lazyScreen("work", (m) => m.Work);
const Projects = lazyScreen("work", (m) => m.Projects);
const SavedViews = lazyScreen("work", (m) => m.SavedViews);
const WorkSearch = lazyScreen("work", (m) => m.WorkSearch);
const Project = lazyScreen("work", (m) => m.Project);
const WorkItem = lazyScreen("work", (m) => m.WorkItem);
const OrgChart = lazyScreen("team", (m) => m.OrgChart);
const People = lazyScreen("team", (m) => m.People);
const Units = lazyScreen("team", (m) => m.Units);
const UnitScreen = lazyScreen("team", (m) => m.UnitScreen);
const About = lazyScreen("team", (m) => m.About);
const SeatScreen = lazyScreen("team", (m) => m.SeatScreen);
const Knowledge = lazyScreen("knowledge", (m) => m.Knowledge);
const Pages = lazyScreen("knowledge", (m) => m.Pages);
const PageView = lazyScreen("knowledge", (m) => m.PageView);
// ----------------------------------------------------------------- console
const Overview = lazyScreen("overview", (m) => m.Overview);
const LiveNow = lazyScreen("live", (m) => m.LiveNow);
const Turns = lazyScreen("live", (m) => m.Turns);
const TurnScreen = lazyScreen("live", (m) => m.TurnScreen);
const Runs = lazyScreen("live", (m) => m.Runs);
const RunScreen = lazyScreen("live", (m) => m.RunScreen);
const Conversations = lazyScreen("live", (m) => m.Conversations);
const ChannelScreen = lazyScreen("live", (m) => m.ChannelScreen);
const TraceScreen = lazyScreen("live", (m) => m.TraceScreen);
const Activity = lazyScreen("live", (m) => m.Activity);
const EventScreen = lazyScreen("live", (m) => m.EventScreen);
const Seats = lazyScreen("setup", (m) => m.Seats);
const SeatSetup = lazyScreen("setup", (m) => m.SeatSetup);
const Schedules = lazyScreen("setup", (m) => m.Schedules);
const Skills = lazyScreen("setup", (m) => m.Skills);
const Diaries = lazyScreen("setup", (m) => m.Diaries);
const Diary = lazyScreen("setup", (m) => m.Diary);
const Spend = lazyScreen("spend", (m) => m.Spend);
const Budgets = lazyScreen("spend", (m) => m.Budgets);
const ExpensiveTasks = lazyScreen("spend", (m) => m.ExpensiveTasks);
const CharterScreen = lazyScreen("company", (m) => m.CharterScreen);
const PeopleAndAccess = lazyScreen("company", (m) => m.PeopleAndAccess);
const OrgEdit = lazyScreen("org", (m) => m.OrgEdit);
const Audit = lazyScreen("company", (m) => m.Audit);
const EveryChange = lazyScreen("company", (m) => m.EveryChange);
const Integrations = lazyScreen("connect", (m) => m.Integrations);
const Tools = lazyScreen("connect", (m) => m.Tools);
const Models = lazyScreen("connect", (m) => m.Models);
const Secrets = lazyScreen("connect", (m) => m.Secrets);
const Fleet = lazyScreen("engine", (m) => m.Fleet);
const ConfigScreen = lazyScreen("engine", (m) => m.ConfigScreen);
const Backups = lazyScreen("engine", (m) => m.Backups);

/** One resolved screen, drawn. */
export function screenFor(route: Resolved): ReactNode {
  switch (route.screen) {
    case "landing":
      return <Landing />;
    case "today":
      return <Today />;
    case "inbox":
      return <Inbox />;
    case "me":
      return <MyWork section={route.section} />;
    case "work":
      return <Work />;
    case "work-projects":
      return <Projects />;
    case "work-views":
      return route.id ? <SavedViews key={route.id} id={route.id} /> : <SavedViews />;
    case "work-search":
      return <WorkSearch />;
    case "project":
      return <Project key={route.key} projectKey={route.key} />;
    case "item":
      return <WorkItem key={route.id} id={route.id} />;
    case "org-chart":
      return <OrgChart />;
    case "people":
      return <People />;
    case "units":
      return route.unit ? <UnitScreen key={route.unit} id={route.unit} /> : <Units />;
    case "about":
      return <About />;
    case "seat":
      return <SeatScreen key={route.handle} handle={route.handle} />;
    case "knowledge":
      return <Knowledge />;
    case "container":
      return <Pages key={route.key} container={route.key} />;
    case "page":
      return <PageView key={route.id} id={route.id} />;
    case "overview":
      return <Overview />;
    case "live":
      return <LiveNow />;
    case "turns":
      return <Turns />;
    case "turn":
      return <TurnScreen key={route.id} turnId={route.id} />;
    case "runs":
      return route.id ? <RunScreen key={route.id} turnId={route.id} /> : <Runs />;
    case "a2a":
      return route.id ? <ChannelScreen key={route.id} id={route.id} /> : <Conversations />;
    case "trace":
      return <TraceScreen key={route.id} traceId={route.id} />;
    case "events":
      return <Activity />;
    case "event":
      return <EventScreen key={route.id} eventId={route.id} />;
    case "seats":
      return <Seats />;
    case "seat-setup":
      return <SeatSetup key={route.handle} handle={route.handle} />;
    case "schedules":
      return <Schedules key={route.scope.join("/")} scope={route.scope} />;
    case "skills":
      return <Skills />;
    case "memory":
      return <Diaries />;
    case "diary":
      return <Diary key={route.handle} handle={route.handle} />;
    case "spend":
      return <Spend />;
    case "spend-tasks":
      return <ExpensiveTasks />;
    case "budgets":
      return <Budgets />;
    case "charter":
      return <CharterScreen />;
    case "access":
      return <PeopleAndAccess />;
    case "org-edit":
      return <OrgEdit />;
    case "audit":
      return <Audit />;
    case "changes":
      return <EveryChange />;
    case "integrations":
      return <Integrations key={route.kind ?? ""} kind={route.kind} />;
    case "tools":
      return (
        <Tools
          key={`${route.server ?? ""}/${route.tool ?? ""}`}
          server={route.server}
          tool={route.tool}
        />
      );
    case "models":
      return <Models key={route.id ?? ""} id={route.id} />;
    case "secrets":
      return <Secrets key={route.name ?? ""} name={route.name} />;
    case "nodes":
      return <Fleet key={route.node ?? ""} node={route.node} />;
    case "config":
      return (
        <ConfigScreen
          key={`${route.revisions}/${route.revision ?? ""}`}
          revision={route.revision}
          revisions={route.revisions}
        />
      );
    case "backups":
      return <Backups key={route.domain ?? ""} domain={route.domain} />;
  }
}

/**
 * `#/`, until the viewer has said who is reading — then the address
 * `landingFor` chooses, REPLACED so the bare fragment leaves no entry Back
 * would land on.
 */
function Landing() {
  const viewer = useViewer();
  const nav = useNavigator();
  const target = landingFor(viewer);
  const at = target?.join("/");
  useEffect(() => {
    if (at !== undefined) nav.replace(at.split("/"));
  }, [at, nav]);
  return <Skeleton variant="text" rows={4} label="Finding where you start" />;
}

function Screen() {
  const route = useRoute();
  const viewer = useViewer();
  const where = resolve(route.path);
  if (!where.resolved) return <NotFound what={where.what} hint={where.hint} />;
  // A CONSOLE ADDRESS FOR A VIEWER THE ENGINE HAS SAID IS NOT AN ADMIN is its
  // refusal and nothing else — see the file's doc. Only on an ANSWER, and the
  // first one is waited for: until it is in, the screen asks nothing it may be
  // refused (a cold load of a console screen used to send its reads, be
  // refused, and draw the refusal a frame before the frame knew to). A viewer
  // read that FAILED is no answer and does not hold the screen: it mounts, and
  // its own reads say what they find. Edit org answers for itself (see
  // `Section.answersRefusal`).
  if (shellOf(route.path) === "admin" && !sectionOf(route.path)?.answersRefusal) {
    const what = sectionOf(route.path)?.label ?? workspaceRow(where.workspace)?.label ?? "This";
    if (viewer.asking) {
      return <Skeleton variant="text" rows={4} label={`Checking your access to ${what}`} />;
    }
    if (!viewer.loading && !viewer.admin) return <AdminRequired what={what} />;
  }
  return screenFor(where);
}

/**
 * The shells whose chunks this reader's browser fetches while idle: the
 * person shell for everybody, and the console only for an admin — see
 * `lazyScreen.ts`'s "A member's browser never fetches the console".
 */
function prefetched(admin: boolean): readonly ShellName[] {
  return admin ? ["person", "admin"] : ["person"];
}

export function App() {
  const route = useRoute();
  return (
    // The toast host wraps the shell rather than sitting inside a screen: an
    // outcome has to survive the navigation the write causes, and a provider
    // mounted per screen is unmounted by exactly that.
    <ToastProvider>
      {/* THE ONE PORTAL TARGET, DECLARED RATHER THAN FALLEN BACK TO. Every
          overlay the kit draws — a Modal, a Select's listbox, a Popover, and
          the palette — asks `useLayerContainer()` where to go, and with no
          host mounted that answers `document.body`. The host is one
          absolutely-positioned box covering the shell, inert until something
          is drawn in it, holding the whole layer band in a stacking context of
          its own. INSIDE THE TOAST PROVIDER, so a toast reporting what a write
          inside a dialog did is readable over the dialog that caused it. */}
      <LayerHost>
        <Shell>
          <Prefetch />
          <ScreenBoundary resetKey={route.path.join("/")}>
            <Screen />
          </ScreenBoundary>
        </Shell>
      </LayerHost>
    </ToastProvider>
  );
}

/**
 * The idle prefetch, inside the frame because the viewer is the frame's
 * reading: it starts once the viewer has answered — a member's tab must not
 * fetch the console's chunks in the moment before it knows it is a member's —
 * and again only if the answer changes which shells this reader may open.
 */
function Prefetch() {
  const viewer = useViewer();
  const ready = !viewer.asking;
  const admin = viewer.admin;
  useEffect(() => (ready ? prefetchOnIdle(prefetched(admin)) : undefined), [ready, admin]);
  return null;
}
