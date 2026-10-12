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
 * # And a level above both: in the frame, or outside it
 *
 * Signing in, redeeming an invitation, spending a password reset link and
 * enrolling a required second factor are drawn OUTSIDE the frame (`FRAMELESS`
 * in `nav.ts`), because a browser on one of them holds no session the frame
 * could use. They are also where the
 * transports send a browser that lost its session — `FollowSessionNeed`,
 * mounted once here beside both, is what reads that and moves. The step-up
 * ceremony (`StepUp.tsx`) is mounted beside both for the same reason: a
 * refusal that asks for a fresher proof arrives on either side. So is the
 * live region the design system's controls speak into (`announcer.tsx`),
 * which a region inside the frame would lose on every sign-in. Those three
 * are EAGER: a browser that has lost its session must reach the sign-in without
 * waiting on a chunk. The sign-in screens themselves are not — they are a
 * chunk of their own (`signin` in `lazyScreen.ts`), which a reader who is
 * signed in never downloads, and `resolve` names them like any other screen
 * so a link to `#/login` is a link to a page.
 *
 * # Keyed on the subject, every screen that has one
 *
 * A hash change re-renders this switch rather than remounting it, so
 * `#/live/turns/A` → `#/live/turns/B` reconciles: React keeps the same
 * component instance and every piece of per-SUBJECT state in it outlives the
 * subject it describes. A disclosure left open, a tab left selected and a
 * refusal left on screen are all the same bug waiting for somebody to notice.
 */

import { useEffect, useSyncExternalStore, type ReactNode } from "react";
import { Skeleton, ToastProvider } from "@crewlethq/ui";
import { Shell } from "./Shell.tsx";
import { StepUpHost } from "./StepUp.tsx";
import { AppAnnouncer } from "./announcer.tsx";
import { parseHash, useNavigator, useRoute } from "./router.tsx";
import { resolve, type Resolved, type Route } from "./routes.ts";
import { framelessOf, grantsMissing, personalPath, sectionOf, sectionOpen } from "./nav.ts";
import { AccessRefused } from "./frame/AccessRefused.tsx";
import { GrantRequired } from "./frame/GrantRequired.tsx";
import { useConnection } from "~/lib/store-hooks.ts";
import { useHeldReading, useViewer } from "~/lib/viewer.ts";
import { useFrameSession } from "~/lib/frameSession.ts";
import { safeNext, signInHash } from "~/lib/session.ts";
import { currentSessionNeed, onSessionNeed } from "~/protocol/index.ts";
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
const Composer = lazyScreen("knowledge", (m) => m.Composer);
const Skills = lazyScreen("knowledge", (m) => m.Skills);
const Diaries = lazyScreen("knowledge", (m) => m.Diaries);
const Diary = lazyScreen("knowledge", (m) => m.Diary);
const Spend = lazyScreen("spend", (m) => m.Spend);
const Budgets = lazyScreen("spend", (m) => m.Budgets);
const ExpensiveTasks = lazyScreen("spend", (m) => m.ExpensiveTasks);
const General = lazyScreen("settings", (m) => m.General);
const PeopleAndAccess = lazyScreen("settings", (m) => m.PeopleAndAccess);
const Integrations = lazyScreen("settings", (m) => m.Integrations);
const Tools = lazyScreen("settings", (m) => m.Tools);
const Models = lazyScreen("settings", (m) => m.Models);
const Secrets = lazyScreen("settings", (m) => m.Secrets);
const Fleet = lazyScreen("settings", (m) => m.Fleet);
const ConfigScreen = lazyScreen("settings", (m) => m.ConfigScreen);
const Backups = lazyScreen("settings", (m) => m.Backups);
const Audit = lazyScreen("settings", (m) => m.Audit);
const Account = lazyScreen("account", (m) => m.Account);
const SignIn = lazyScreen("signin", (m) => m.SignIn);
const Invite = lazyScreen("signin", (m) => m.Invite);
const Reset = lazyScreen("signin", (m) => m.Reset);
const Enrol = lazyScreen("signin", (m) => m.Enrol);
const Appearance = lazyScreen("settings", (m) => m.Appearance);

/** One resolved screen, drawn. */
export function screenFor(route: Resolved): ReactNode {
  switch (route.screen) {
    case "login":
      return <SignIn />;
    case "enrol":
      return <Enrol />;
    case "invite":
      return <Invite key={route.link} link={route.link} />;
    case "reset":
      return <Reset key={route.link} link={route.link} />;
    case "home":
      return <Home />;
    case "inbox":
      return <Inbox />;
    case "me":
      return <MyWork section={route.section} />;
    case "work-projects":
      return <Projects />;
    case "work-tasks":
      return <Work />;
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
    case "new-page":
      return <Composer />;
    case "skills":
      return <Skills />;
    case "diaries":
      return <Diaries />;
    case "diary":
      return <Diary key={route.handle} handle={route.handle} />;
    case "spend":
      return <Spend />;
    case "spend-tasks":
      return <ExpensiveTasks />;
    case "budgets":
      return <Budgets />;
    case "general":
      return <General />;
    case "people":
      return <PeopleAndAccess />;
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
    case "audit":
      return <Audit />;
    case "account":
      return <Account />;
    case "appearance":
      return <Appearance />;
  }
}

/**
 * Follows what the transports say the session needs.
 *
 * A `401` anywhere — the socket's refusal probe, any REST call — means this
 * browser holds nothing the engine accepts, and the answer is the sign-in
 * screen, carrying the address the reader was on as `next`. A session that
 * may only enrol a second factor goes to the enrolment instead. Both REPLACE
 * the entry: the screen the reader was on cannot be drawn for them, and Back
 * from the sign-in must not land on it again.
 *
 * THE SIGN-IN SCREENS THEMSELVES ARE LEFT ALONE: they answer their own
 * refusals, and a sign-in form routed to itself would lose what was typed.
 * An enrolment that loses its session goes to sign in, carrying its own
 * `next` rather than itself (`signInHash`).
 */
function FollowSessionNeed() {
  const need = useSyncExternalStore(onSessionNeed, currentSessionNeed);
  const route = useRoute();
  const nav = useNavigator();
  useEffect(() => {
    if (need === null) return;
    const at = framelessOf(route.path);
    if (need === "sign_in") {
      if (at === "login" || at === "invite" || at === "reset") return;
      const target = parseHash(signInHash(route.hash));
      nav.replace(target.path, target.query);
      return;
    }
    if (at !== null) return;
    nav.replace(["enrol"], { next: safeNext(route.hash) });
  }, [need, route, nav]);
  return null;
}

/**
 * The frame and its screen, or a screen drawn outside it.
 *
 * DECIDED ON THE HEAD, before anything resolves: every address under a
 * frameless head is drawn outside the frame, a malformed one included. A
 * `#/login/x` drawn inside it would mount the sidebar, the state bar and the
 * screen's reads for a browser that holds no session, and every one of them
 * would be refused.
 */
function Frame() {
  const route = useRoute();
  const where = resolve(route.path);
  const resetKey = route.path.join("/");
  if (framelessOf(route.path) !== null) {
    if (!where.resolved) return <NotFound what={where.what} hint={where.hint} />;
    return <ScreenBoundary resetKey={resetKey}>{screenFor(where)}</ScreenBoundary>;
  }
  return (
    <Shell>
      <ScreenBoundary resetKey={resetKey}>
        <Screen where={where} path={route.path} />
      </ScreenBoundary>
    </Shell>
  );
}

function Screen({ where, path }: { where: Route; path: string[] }) {
  const viewer = useViewer();
  const held = useHeldReading();
  const { accessRefused } = useConnection();
  const { noAccess } = useFrameSession();
  if (!where.resolved) return <NotFound what={where.what} hint={where.hint} />;
  // A BROWSER THE ENGINE KNOWS AND WILL NOT SERVE, or whose session holds no
  // grant to read the company, is told so in place of every screen but the
  // reader's own — their Account and Settings › Appearance, which read no
  // socket (`personalPath`, the rule the sidebar's locks are drawn by) — see
  // `AccessRefused`.
  if ((accessRefused !== null || noAccess) && !personalPath(path)) {
    return <AccessRefused reason={accessRefused} />;
  }
  // A SECTION CLOSED TO THE READER is its refusal and nothing else — see
  // `GrantRequired` — decided by the rule every lock the navigation draws is
  // decided by (`nav.ts`'s `sectionOpen`), over the frame's one reading of
  // who the reader is and what they hold (`useHeldReading`): the viewer's
  // answer, or the session's where the viewer has not answered — and the
  // refusal names the reader from that same reading. Read off the viewer alone, a
  // session whose socket the engine refuses (so whose viewer never answers)
  // was held at "Checking your access" for good on a section the sidebar
  // beside it had already locked. Only on an ANSWER, and the first one is
  // waited for: until it is in, the section asks nothing it may be refused (a
  // cold load of #/settings/secrets sent its guarded reads, was refused, and
  // drew the refusal, a frame before the frame knew to). A read that FAILED
  // is no answer and does not hold the section: the screen mounts, and its
  // own reads say what they find.
  const section = sectionOf(path);
  if (section?.grants) {
    if (held === null) {
      if (viewer.asking) {
        return (
          <Skeleton variant="text" rows={4} label={`Checking your access to ${section.label}`} />
        );
      }
    } else if (!sectionOpen(section, held.grants)) {
      return (
        <GrantRequired
          what={section.label}
          grants={grantsMissing(section, held.grants)}
          reader={held.login}
        />
      );
    }
  }
  return screenFor(where);
}

export function App() {
  // EVERY OTHER WORKSPACE, FETCHED WHILE NOBODY IS WAITING — see
  // `prefetchOnIdle`. Once per tab: the effect has no dependency to re-run on.
  useEffect(() => prefetchOnIdle(), []);
  return (
    // The toast host wraps the shell rather than sitting inside a screen: an
    // outcome has to survive the navigation the write causes, and a provider
    // mounted per screen is unmounted by exactly that.
    //
    // AND NO `LayerHost` AROUND THE FRAME. Every overlay the kit draws — a
    // Modal, a Select's listbox, a Popover, the palette, a toast — asks
    // `useLayerContainer()` where to go, and with no host that answers
    // `document.body`, where each one's z-index is its DEPTH on the one layer
    // stack in the ROOT stacking context. The narrow layout's drawer is on that
    // stack too, but it is the sidebar itself and cannot be portalled: it is
    // painted in place at its own depth plus one. A host here made a stacking
    // context of its own at the bottom of the band, so every overlay was capped
    // beneath the open drawer whatever its depth — on a phone the Account
    // popover and the palette opened from the drawer drew UNDER it, and their
    // Account link, sign-outs and results could not be tapped. The kit names a
    // host for a FULLSCREEN container only (the org builder keeps its own),
    // since a fullscreen element paints nothing outside itself.
    <ToastProvider>
      {/* EAGER, AND OUTSIDE THE FRAME: the session's routing, the step-up
          ceremony and the one live region are what a sign-in screen and a
          framed one both need, so none may tear them down and none of them
          waits on a chunk. */}
      <FollowSessionNeed />
      <StepUpHost />
      {/* THE ONE LIVE REGION the design system's controls speak into,
          beside the frame so a sign-in does not tear it down. See
          `announcer.tsx`. */}
      <AppAnnouncer />
      <Frame />
    </ToastProvider>
  );
}
