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
 * A flat import list rather than lazy chunks: the whole application is served
 * from the same binary as the API, and a code-split chunk buys a round trip
 * against a server that is already answering.
 *
 * # Keyed on the subject, every screen that has one
 *
 * A hash change re-renders this switch rather than remounting it, so
 * `#/live/turns/A` → `#/live/turns/B` reconciles: React keeps the same
 * component instance and every piece of per-SUBJECT state in it outlives the
 * subject it describes. A disclosure left open, a tab left selected and a
 * refusal left on screen are all the same bug waiting for somebody to notice.
 */

import type { ReactNode } from "react";
import { Shell } from "./Shell.tsx";
import { LayerHost, Skeleton, ToastProvider } from "@crewlethq/ui";
import { useRoute } from "./router.tsx";
import { resolve, type Resolved } from "./routes.ts";
import { sectionOf } from "./nav.ts";
import { OperatorRequired } from "./frame/OperatorRequired.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { Home } from "~/routes/home/Home.tsx";
import { Inbox } from "~/routes/inbox/Inbox.tsx";
import { MyWork } from "~/routes/me/MyWork.tsx";
import { People } from "~/routes/agents/People.tsx";
import { SeatScreen } from "~/routes/agents/Seat.tsx";
import { OrgChart, OrgEdit, Teams, UnitScreen } from "~/routes/agents/Company.tsx";
import { Schedules } from "~/routes/agents/Schedules.tsx";
import { Work } from "~/routes/work/Work.tsx";
import { Project } from "~/routes/work/Project.tsx";
import { Projects } from "~/routes/work/Projects.tsx";
import { History } from "~/routes/work/History.tsx";
import { WorkItem } from "~/routes/work/WorkItem.tsx";
import { SavedViews } from "~/routes/work/SavedViews.tsx";
import { WorkSearch } from "~/routes/work/WorkSearch.tsx";
import { Pages, PageView } from "~/routes/knowledge/Pages.tsx";
import { Knowledge } from "~/routes/knowledge/Knowledge.tsx";
import { LiveNow } from "~/routes/live/LiveNow.tsx";
import { Turns } from "~/routes/live/Turns.tsx";
import { TurnScreen } from "~/routes/live/Turn.tsx";
import { Runs } from "~/routes/live/Runs.tsx";
import { Conversations } from "~/routes/live/Conversations.tsx";
import { TraceScreen } from "~/routes/live/Trace.tsx";
import { Activity } from "~/routes/live/Activity.tsx";
import { EventScreen } from "~/routes/live/Event.tsx";
import { Spend } from "~/routes/spend/Spend.tsx";
import { Budgets } from "~/routes/spend/Budgets.tsx";
import { General } from "~/routes/settings/General.tsx";
import { Fleet } from "~/routes/settings/Fleet.tsx";
import { Backups } from "~/routes/settings/Backups.tsx";
import { Integrations } from "~/routes/settings/Integrations.tsx";
import { Tools } from "~/routes/settings/Tools.tsx";
import { ConfigScreen } from "~/routes/settings/Config.tsx";
import { Secrets } from "~/routes/settings/Secrets.tsx";
import { Audit } from "~/routes/settings/Audit.tsx";
import { NotFound } from "~/routes/NotFound.tsx";

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
      return <Runs key={route.id ?? ""} runId={route.id} />;
    case "a2a":
      return <Conversations key={route.id ?? ""} channelId={route.id} />;
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
          <Screen />
        </Shell>
      </LayerHost>
    </ToastProvider>
  );
}
