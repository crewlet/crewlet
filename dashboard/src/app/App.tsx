/**
 * Route dispatch.
 *
 * A flat switch rather than a route table with lazy chunks: there are twenty
 * screens, the whole application is ~180 KB gzipped, and it is served from the
 * same binary as the API — so a code-split chunk buys a round trip against a
 * server that is already answering. The switch is also what makes the screen
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
import { ToastProvider } from "~/ui/Toast.tsx";
import { useRoute } from "./router.tsx";
import { Inbox } from "~/routes/Inbox.tsx";
import { MyWork } from "~/routes/MyWork.tsx";
import { Goal, Goals } from "~/routes/Goals.tsx";
import { People } from "~/routes/People.tsx";
import { SeatScreen } from "~/routes/Seat.tsx";
import { CompanyScreen, UnitScreen } from "~/routes/Company.tsx";
import { Runs } from "~/routes/Runs.tsx";
import { Work } from "~/routes/Work.tsx";
import { WorkItem } from "~/routes/WorkItem.tsx";
import { SavedViews } from "~/routes/SavedViews.tsx";
import { Sprints } from "~/routes/Sprints.tsx";
import { Pages, PageView } from "~/routes/Pages.tsx";
import { Conversations } from "~/routes/Conversations.tsx";
import { Schedules } from "~/routes/Schedules.tsx";
import { ModelActivity } from "~/routes/Model.tsx";
import { LiveNow } from "~/routes/LiveNow.tsx";
import { Activity } from "~/routes/Activity.tsx";
import { Knowledge } from "~/routes/Knowledge.tsx";
import { Spend } from "~/routes/Spend.tsx";
import { Budgets } from "~/routes/Budgets.tsx";
import { Fleet } from "~/routes/Fleet.tsx";
import { Integrations } from "~/routes/Integrations.tsx";
import { Tools } from "~/routes/Tools.tsx";
import { ConfigScreen } from "~/routes/Config.tsx";
import { Secrets } from "~/routes/Secrets.tsx";
import { EventScreen } from "~/routes/Event.tsx";
import { TurnScreen } from "~/routes/Turn.tsx";
import { NotFound } from "~/routes/NotFound.tsx";

/** A project key is uppercase; an item key is `KEY-n`; an id is a uuid. */
const PROJECT_KEY = /^[A-Z][A-Z0-9_]*$/;

function WorkRoutes({ rest }: { rest: string[] }) {
  const [first, second, third] = rest;
  if (!first) return <Work />;
  if (first === "views") return second ? <SavedViews key={second} id={second} /> : <SavedViews />;
  // A PROJECT, ITS SPRINTS, OR AN ITEM — decided by the SHAPE of the key
  // rather than by a lookup, so the route resolves before any answer arrives.
  if (PROJECT_KEY.test(first)) {
    if (second === "sprints") {
      return <Sprints key={`${first}/${third ?? ""}`} project={first} sprint={third} />;
    }
    return <Work key={first} project={first} />;
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
      return id ? <TurnScreen key={id} turnId={id} /> : <ModelActivity />;
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
    case "fleet":
      return <Fleet key={tail[0] ?? ""} node={tail[0]} />;
    case "integrations":
      return <Integrations key={tail[0] ?? ""} kind={tail[0]} />;
    case "tools":
      return <Tools key={tail.join("/")} server={tail[0] === "servers" ? tail[1] : undefined} />;
    case "config":
      return (
        <ConfigScreen
          key={tail.join("/")}
          revision={tail[0] === "revisions" ? tail[1] : undefined}
        />
      );
    case "credentials":
      return <Secrets key={tail[0] ?? ""} name={tail[0]} />;
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
    case "goals":
      return rest[0] ? <Goal key={rest[0]} id={rest[0]} /> : <Goals />;
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
      return rest[0] === "budgets" ? <Budgets /> : <Spend />;
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
    <ToastProvider>
      <Shell>
        <Screen />
      </Shell>
    </ToastProvider>
  );
}
