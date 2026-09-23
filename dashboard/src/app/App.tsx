/**
 * Route dispatch.
 *
 * A flat switch rather than a route table with lazy chunks: there are twenty
 * screens, the whole application is about 400 KB gzipped (the entry, the React
 * chunk and the stylesheet, measured as the engine serves them), and it is
 * served from the same binary as the API — so a code-split chunk buys a round
 * trip against a server that is already answering. The switch is also what makes the screen
 * list readable in one place.
 *
 * # Two levels of dispatch, matching the two levels of navigation
 *
 * The first segment names a WORKSPACE and the second names what inside it. A
 * single-level switch is what the previous shape had, and it is why nineteen
 * unrelated nouns sat at the top of the URL space: `#/runs`, `#/schedules`,
 * `#/conversations` and `#/model` are four views of one question — what the
 * company's workforce did — and nothing in the address said so.
 *
 * # Keyed on the subject, every screen that has one
 *
 * A hash change re-renders this switch rather than remounting it, so
 * `#/activity/turns/A` → `#/activity/turns/B` reconciles: React keeps the same
 * component instance and every piece of per-SUBJECT state in it outlives the
 * subject it describes. A disclosure left open, a tab left selected and a
 * refusal left on screen are all the same bug waiting for somebody to notice.
 */

import { Shell } from "./Shell.tsx";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { useRoute } from "./router.tsx";
import { Inbox } from "~/routes/inbox/Inbox.tsx";
import { MyWork } from "~/routes/me/MyWork.tsx";
import { People } from "~/routes/company/People.tsx";
import { SeatScreen } from "~/routes/company/Seat.tsx";
import { CompanyScreen, UnitScreen } from "~/routes/company/Company.tsx";
import { Runs } from "~/routes/activity/Runs.tsx";
import { Work } from "~/routes/work/Work.tsx";
import { Project } from "~/routes/work/Project.tsx";
import { Projects } from "~/routes/work/Projects.tsx";
import { History } from "~/routes/work/History.tsx";
import { WorkItem } from "~/routes/work/WorkItem.tsx";
import { SavedViews } from "~/routes/work/SavedViews.tsx";
import { WorkSearch } from "~/routes/work/WorkSearch.tsx";
import { Pages, PageView } from "~/routes/knowledge/Pages.tsx";
import { Conversations } from "~/routes/activity/Conversations.tsx";
import { Turns } from "~/routes/activity/Turns.tsx";
import { Schedules } from "~/routes/activity/Schedules.tsx";
import { LiveNow } from "~/routes/activity/LiveNow.tsx";
import { Activity } from "~/routes/activity/Activity.tsx";
import { Knowledge } from "~/routes/knowledge/Knowledge.tsx";
import { Spend } from "~/routes/cost/Spend.tsx";
import { Budgets } from "~/routes/cost/Budgets.tsx";
import { Fleet } from "~/routes/admin/Fleet.tsx";
import { DomainScreen } from "~/routes/admin/Domain.tsx";
import { Integrations } from "~/routes/admin/Integrations.tsx";
import { Tools } from "~/routes/admin/Tools.tsx";
import { ConfigScreen } from "~/routes/admin/Config.tsx";
import { Secrets } from "~/routes/admin/Secrets.tsx";
import { Audit } from "~/routes/admin/Audit.tsx";
import { EventScreen } from "~/routes/activity/Event.tsx";
import { TurnScreen } from "~/routes/activity/Turn.tsx";
import { TraceScreen } from "~/routes/activity/Trace.tsx";
import { NotFound } from "~/routes/NotFound.tsx";

/** A project key is uppercase; an item key is `KEY-n`; an id is a uuid. */
const PROJECT_KEY = /^[A-Z][A-Z0-9_]*$/;

function WorkRoutes({ rest }: { rest: string[] }) {
  const [first, second] = rest;
  if (!first) return <Work />;
  if (first === "views") return second ? <SavedViews key={second} id={second} /> : <SavedViews />;
  if (first === "search") return <WorkSearch />;
  // THE TWO WORKSPACE-LEVEL LISTS, AND NEITHER TAKES A TAIL. Both answer a
  // question about the whole company — which projects there are, and what has
  // changed — so there is nothing under them to address: a project has its own
  // route one line down, and a change is a record on the item it changed.
  // Rendering the list with the tail dropped would put a trail over it naming
  // a page nobody routed to, which is the defect the Admin arms record.
  if (first === "projects" || first === "history") {
    if (second) return <NotFound what={`“${rest.join("/")}” under Work`} />;
    return first === "projects" ? <Projects /> : <History />;
  }
  // A PROJECT OR AN ITEM — decided by the SHAPE of the key rather than by a
  // lookup, so the route resolves before any answer arrives.
  if (PROJECT_KEY.test(first)) {
    return <Project key={first} projectKey={first} />;
  }
  // A key or an id BOTH resolve, because the reader has whichever they were
  // shown: a person pastes ENG-42 out of chat, and every internal link carries
  // the id. The engine's own Get takes either.
  return <WorkItem key={first} id={first} />;
}

function CompanyRoutes({ rest }: { rest: string[] }) {
  const [first, second] = rest;
  if (!first) return <CompanyScreen />;
  if (first === "people") return second ? <SeatScreen key={second} handle={second} /> : <People />;
  if (first === "units") {
    return second ? <UnitScreen key={second} id={second} /> : <CompanyScreen />;
  }
  return <NotFound what={`“${first}” under Company`} />;
}

function ActivityRoutes({ rest }: { rest: string[] }) {
  const [first, ...tail] = rest;
  const id = tail[0];
  if (!first) return <LiveNow />;
  switch (first) {
    case "turns":
      return id ? <TurnScreen key={id} turnId={id} /> : <Turns />;
    // A TRACE HAS NO LIST, only a page: nothing enumerates traces, and every
    // way in is a link from an event, a turn or a coding run that already
    // holds the id. A bare `#/activity/traces` is therefore the turns list,
    // which is the nearest thing to "the traces" this product has.
    case "traces":
      return id ? <TraceScreen key={id} traceId={id} /> : <Turns />;
    case "runs":
      return <Runs key={id ?? ""} runId={id} />;
    case "schedules":
      return <Schedules key={tail.join("/")} scope={tail} />;
    case "a2a":
      return <Conversations key={id ?? ""} channelId={id} />;
    case "events":
      return id ? <EventScreen key={id} eventId={id} /> : <Activity />;
    default:
      return <NotFound what={`“${first}” under Activity`} />;
  }
}

function AdminRoutes({ rest }: { rest: string[] }) {
  const [first, ...tail] = rest;
  // THE LANDING PAGE IS THE FLEET, because a reader who opened Admin is
  // looking at the machine and the nodes are the machine.
  if (!first) return <Fleet />;
  switch (first) {
    case "fleet": {
      // TWO SHAPES UNDER ONE SEGMENT, discriminated on the tail's LENGTH for
      // the reason the `tools` arm below gives: a node id is an OPERATOR's
      // string and `domains` is a legal one, so reading `tail[0] === "domains"`
      // would take the page away from a node actually called that.
      //
      // It used to take any tail at all and keep only the first segment, so
      // `#/admin/fleet/domains/tracker` drew the node screen for a node named
      // `domains` with `tracker` silently dropped — a plausible answer to a
      // question nobody asked, under a breadcrumb that read
      // "Infrastructure / domains/tracker".
      const domain = tail.length === 2 && tail[0] === "domains" ? tail[1] : undefined;
      if (domain !== undefined) return <DomainScreen key={domain} name={domain} />;
      if (tail.length > 1) {
        return <NotFound what={`“${tail.join("/")}” under Infrastructure`} />;
      }
      return <Fleet key={tail[0] ?? ""} node={tail[0]} />;
    }
    case "integrations":
      return <Integrations key={tail[0] ?? ""} kind={tail[0]} />;
    case "tools": {
      // TWO SHAPES UNDER ONE SEGMENT, and they do not overlap: `servers/{name}`
      // is a FILTER on the catalogue's origin, and a bare `{name}` is one TOOL
      // — which is what `objects.ts` calls a tool's page and where a tool
      // peek's `Open ↗` goes. The bare form used to fall through with the
      // segment dropped, so that link landed on the unfiltered catalogue and a
      // reader lost the tool they had open.
      //
      // DISCRIMINATED ON LENGTH, not on the word. A tool name comes from a
      // third-party MCP server — `tool_prefix` is optional, so the catalogue
      // holds whatever the server called it — and `nav.ts`'s reserved-segment
      // rule ("everything the engine mints is a uuid or an uppercase key") does
      // not cover one. Reading `tail[0] === "servers"` as the filter therefore
      // took the page away from a tool literally named `servers`, which is the
      // same regression one sentence up. `router.tsx` encodes each segment
      // whole, so a tool page is always exactly ONE tail segment and the
      // origin filter always two — a test nothing else can fake.
      const server = tail.length === 2 && tail[0] === "servers" ? tail[1] : undefined;
      const tool = tail.length === 1 ? tail[0] : undefined;
      if (tail.length > 0 && server === undefined && tool === undefined) {
        return <NotFound what={`“${tail.join("/")}” under Tools`} />;
      }
      return <Tools key={tail.join("/")} server={server} tool={tool} />;
    }
    case "config": {
      // `revisions/{id}` IS THE ONLY TAIL, so anything else is an address the
      // product does not have — and rendering the config screen with the
      // segment dropped leaves the trail naming a page nobody routed to.
      if (!tail[0]) return <ConfigScreen />;
      if (tail[0] === "revisions" && tail.length <= 2) {
        return <ConfigScreen key={tail.join("/")} revision={tail[1]} />;
      }
      return <NotFound what={`“${tail.join("/")}” under Config`} />;
    }
    case "credentials":
      return <Secrets key={tail[0] ?? ""} name={tail[0]} />;
    case "audit":
      // NO TAIL. Every row here has a page of its own somewhere else — a task,
      // a wiki page, a config revision — so a detail under this address would
      // be a second page for an object that already has one, reachable by two
      // routes that would then have to agree about it.
      if (tail.length > 0) return <NotFound what={`“${tail.join("/")}” under Audit`} />;
      return <Audit />;
    default:
      return <NotFound what={`“${first}” under Admin`} />;
  }
}

function Screen() {
  const route = useRoute();
  const [head, ...rest] = route.path;
  switch (head) {
    // THE LANDING SCREEN IS THE INBOX. A dashboard's home used to be a
    // summary of the company; what a person opening this actually wants to
    // know is whether anything is waiting on them.
    case undefined:
    case "inbox":
      return <Inbox />;
    case "me":
      return <MyWork />;
    case "work":
      return <WorkRoutes rest={rest} />;
    case "company":
      return <CompanyRoutes rest={rest} />;
    case "knowledge":
      // A CONTAINER, OR A PAGE INSIDE ONE. Both are addressed by their own
      // names rather than by an id, because a container key is what somebody
      // types and a page title is what they were given.
      if (rest.length >= 2) {
        return (
          <PageView
            key={rest.join("/")}
            container={rest[0] ?? ""}
            title={rest.slice(1).join("/")}
          />
        );
      }
      return rest[0] ? <Pages key={rest[0]} container={rest[0]} /> : <Knowledge />;
    case "activity":
      return <ActivityRoutes rest={rest} />;
    case "cost":
      // A CLOSED SET OF TWO, so an unknown tail is Not Found rather than the
      // spend screen. A two-valued test read every other tail as `#/cost`, so
      // `#/cost/budget` — the obvious typo, and the shape of a stale bookmark
      // — drew the spend tables under a trail reading "Cost / budget": the
      // address, the trail and the screen each naming something different,
      // with nothing telling the reader the route does not exist.
      if (!rest[0]) return <Spend />;
      return rest[0] === "budgets" && rest.length === 1 ? (
        <Budgets />
      ) : (
        <NotFound what={`“${rest.join("/")}” under Cost`} />
      );
    case "admin":
      return <AdminRoutes rest={rest} />;
    default:
      return <NotFound what={`the screen “${head}”`} />;
  }
}

export function App() {
  return (
    // The toast host wraps the shell rather than sitting inside a screen: an
    // outcome has to survive the navigation the write causes, and a provider
    // mounted per screen is unmounted by exactly that.
    //
    // uilet's provider, whose `ok` / `failed` are ours verbatim — a success
    // dismisses itself and a failure stays until it is taken back. What it
    // adds is the part ours only asserted: two live regions rather than one,
    // so a refusal is announced assertively while a confirmation stays
    // polite, and a `max` that drops the oldest instead of letting a burst of
    // writes bury the screen.
    <ToastProvider>
      {/* THE ONE PORTAL TARGET, DECLARED RATHER THAN FALLEN BACK TO. Every
          overlay uilet draws — a Modal, a Select's listbox, a Popover, and
          the palette, which takes the layer without the frame — asks
          `useLayerContainer()` where to go, and with no host mounted that
          answers `document.body`: a fallback, and one that puts each surface
          outside `#root` as a sibling of the application, in whatever order
          the session happened to open them.

          The host is one absolutely-positioned box covering the shell, inert
          until something is drawn in it, holding the whole layer band in a
          stacking context of its own. `body` has `overflow: hidden` here and
          the shell is the window, so the box IS the viewport and nothing
          moves on screen — what changes is that the application says where
          its overlays live.

          INSIDE THE TOAST PROVIDER, so the toaster is rendered after the host
          and paints above it. A toast reports what a write inside a dialog
          did; it has to be readable over the dialog that caused it. */}
      <LayerHost>
        <Shell>
          <Screen />
        </Shell>
      </LayerHost>
    </ToastProvider>
  );
}
