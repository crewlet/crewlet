/**
 * Route dispatch: what `app/routes.ts` resolved, as a component.
 *
 * THE TABLE IS NOT HERE. Which paths are pages, which segment is a key and
 * which an id, what a bare `#/live/traces` is — all of that is
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
 * # Keyed on the subject, every screen that has one
 *
 * A hash change re-renders this switch rather than remounting it, so
 * `#/live/turns/A` → `#/live/turns/B` reconciles: React keeps the same
 * component instance and every piece of per-SUBJECT state in it outlives the
 * subject it describes. A disclosure left open, a tab left selected and a
 * refusal left on screen are all the same bug waiting for somebody to notice.
 */

import { useEffect, type ReactNode } from "react";
import { Shell } from "./Shell.tsx";
import { LayerHost, Skeleton, ToastProvider } from "@crewlethq/ui";
import { useRoute } from "./router.tsx";
import { resolve, type Resolved } from "./routes.ts";
import { sectionOf } from "./nav.ts";
import { OperatorRequired } from "./frame/OperatorRequired.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { NotFound } from "~/routes/NotFound.tsx";
import { lazyScreen, prefetchOnIdle } from "./lazyScreen.ts";
import { ScreenBoundary } from "./boundaries.tsx";

const Home = lazyScreen("home", (m) => m.Home);
const Inbox = lazyScreen("inbox", (m) => m.Inbox);
const MyWork = lazyScreen("me", (m) => m.MyWork);
const Work = lazyScreen("work", (m) => m.Work);
const Projects = lazyScreen("work", (m) => m.Projects);
const SavedViews = lazyScreen("work", (m) => m.SavedViews);
const History = lazyScreen("work", (m) => m.History);
const WorkSearch = lazyScreen("work", (m) => m.WorkSearch);
const Project = lazyScreen("work", (m) => m.Project);
const WorkItem = lazyScreen("work", (m) => m.WorkItem);
const OrgChart = lazyScreen("agents", (m) => m.OrgChart);
const People = lazyScreen("agents", (m) => m.People);
const Teams = lazyScreen("agents", (m) => m.Teams);
const UnitScreen = lazyScreen("agents", (m) => m.UnitScreen);
const Schedules = lazyScreen("agents", (m) => m.Schedules);
const SeatScreen = lazyScreen("agents", (m) => m.SeatScreen);
const OrgEdit = lazyScreen("org", (m) => m.OrgEdit);
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
const Knowledge = lazyScreen("knowledge", (m) => m.Knowledge);
const Pages = lazyScreen("knowledge", (m) => m.Pages);
const PageView = lazyScreen("knowledge", (m) => m.PageView);
const Spend = lazyScreen("spend", (m) => m.Spend);
const Budgets = lazyScreen("spend", (m) => m.Budgets);
const General = lazyScreen("settings", (m) => m.General);
const Integrations = lazyScreen("settings", (m) => m.Integrations);
const Tools = lazyScreen("settings", (m) => m.Tools);
const Secrets = lazyScreen("settings", (m) => m.Secrets);
const Fleet = lazyScreen("settings", (m) => m.Fleet);
const ConfigScreen = lazyScreen("settings", (m) => m.ConfigScreen);
const Backups = lazyScreen("settings", (m) => m.Backups);
const Audit = lazyScreen("settings", (m) => m.Audit);

/** One resolved screen, drawn. */
export function screenFor(route: Resolved): ReactNode {
  switch (route.screen) {
    case "home":
      return <Home />;
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
    case "work-history":
      return <History />;
    case "work-search":
      return <WorkSearch />;
    case "project":
      return <Project key={route.key} projectKey={route.key} />;
    case "item":
      return <WorkItem key={route.id} id={route.id} />;
    case "org-chart":
      return <OrgChart />;
    case "roster":
      return <People />;
    case "teams":
      return route.unit ? <UnitScreen key={route.unit} id={route.unit} /> : <Teams />;
    case "schedules":
      return <Schedules key={route.scope.join("/")} scope={route.scope} />;
    case "org-edit":
      return <OrgEdit />;
    case "seat":
      return <SeatScreen key={route.handle} handle={route.handle} />;
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
    case "knowledge":
      return <Knowledge />;
    case "container":
      return <Pages key={route.key} container={route.key} />;
    case "page":
      return <PageView key={route.id} id={route.id} />;
    case "spend":
      return <Spend />;
    case "budgets":
      return <Budgets />;
    case "general":
      return <General />;
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
    case "secrets":
      return <Secrets key={route.name ?? ""} name={route.name} />;
    case "nodes":
      return <Fleet key={route.node ?? ""} node={route.node} />;
    case "config":
      return (
        <ConfigScreen
          key={`${route.revisions}/${route.revision ?? ""}`}
          revision={route.revision}
        />
      );
    case "backups":
      return <Backups key={route.domain ?? ""} domain={route.domain} />;
    case "audit":
      return <Audit />;
  }
}

function Screen() {
  const route = useRoute();
  const viewer = useViewer();
  const where = resolve(route.path);
  if (!where.resolved) return <NotFound what={where.what} hint={where.hint} />;
  // A GUARDED SECTION FOR A VIEWER THE ENGINE HAS SAID CANNOT READ IT is its
  // refusal and nothing else — see `OperatorRequired`. Only on an ANSWER, and
  // the first one is waited for: until it is in, the section asks nothing it
  // may be refused (a cold load of #/settings/secrets sent its guarded reads,
  // was refused, and drew the refusal, a frame before the frame knew to). A
  // viewer read that FAILED is no answer and does not hold the section: the
  // screen mounts, and its own reads say what they find.
  const section = sectionOf(route.path);
  if (section?.guarded && !section.answersRefusal) {
    if (viewer.asking) {
      return (
        <Skeleton variant="text" rows={4} label={`Checking your access to ${section.label}`} />
      );
    }
    if (!viewer.loading && !viewer.operator) return <OperatorRequired what={section.label} />;
  }
  return screenFor(where);
}

export function App() {
  const route = useRoute();
  // EVERY OTHER WORKSPACE, FETCHED WHILE NOBODY IS WAITING — see
  // `prefetchOnIdle`. Once per tab: the effect has no dependency to re-run on.
  useEffect(() => prefetchOnIdle(), []);
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
          <ScreenBoundary resetKey={route.path.join("/")}>
            <Screen />
          </ScreenBoundary>
        </Shell>
      </LayerHost>
    </ToastProvider>
  );
}
