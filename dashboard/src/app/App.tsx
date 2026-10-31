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
 * EVERY SCREEN IS A LAZY CHUNK, one per screen directory under `routes/`
 * (`lazyScreen.ts` says why the flat import list this replaced was the wrong
 * trade), so the names below are components that suspend until their code is
 * in. The routed
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

import { useEffect, useSyncExternalStore, type ComponentType, type ReactNode } from "react";
import { Skeleton, ToastProvider } from "@crewlethq/ui";
import { Shell } from "./Shell.tsx";
import { StepUpHost } from "./StepUp.tsx";
import { AppAnnouncer } from "./announcer.tsx";
import { parseHash, useNavigator, useRoute } from "./router.tsx";
import {
  artifactSectionPath,
  newArtifactPath,
  resolve,
  type Resolved,
  type Route,
} from "./routes.ts";
import { closedBy, framelessOf, guardedPath, personalPath, serviceOfPath } from "./nav.ts";
import { AccessRefused } from "./frame/AccessRefused.tsx";
import { GrantRequired } from "./frame/GrantRequired.tsx";
import { useConnection } from "~/lib/store-hooks.ts";
import { useHeldReading, useViewer } from "~/lib/viewer.ts";
import { useOrgArrived, useServicesKnown, useServicesRun } from "~/lib/services.ts";
import { ServiceNotice } from "./ServiceNotice.tsx";
import { safeNext, signInHash } from "~/lib/session.ts";
import { isMakeKind, type MakeKind } from "~/lib/artifacts/words.ts";
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
const Changes = lazyScreen("code", (m) => m.Changes);
const ChangePage = lazyScreen("code", (m) => m.ChangePage);
const Repositories = lazyScreen("code", (m) => m.Repositories);
const RepositoryPage = lazyScreen("code", (m) => m.RepositoryPage);
const CommitPage = lazyScreen("code", (m) => m.CommitPage);
const ComparePage = lazyScreen("code", (m) => m.ComparePage);
const Pushes = lazyScreen("code", (m) => m.Pushes);
const OrgChart = lazyScreen("agents", (m) => m.OrgChart);
const Roster = lazyScreen("agents", (m) => m.People);
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
const ArtifactGallery = lazyScreen("artifacts", (m) => m.ArtifactGallery);
const ArtifactPage = lazyScreen("artifacts", (m) => m.ArtifactPage);
const ArtifactFolders = lazyScreen("artifacts", (m) => m.ArtifactFolders);
const ArtifactFolderPage = lazyScreen("artifacts", (m) => m.ArtifactFolderPage);
/** Each kind's create page, a chunk of its own (`lazyScreen.ts`). */
const NEW_ARTIFACT: Readonly<Record<MakeKind, ComponentType>> = {
  doc: lazyScreen("new-doc", (m) => m.DocCreate),
  slides: lazyScreen("new-slides", (m) => m.SlidesCreate),
  sheet: lazyScreen("new-sheet", (m) => m.SheetCreate),
  design: lazyScreen("new-design", (m) => m.DesignCreate),
  dashboard: lazyScreen("new-dashboard", (m) => m.DashboardCreate),
  html: lazyScreen("new-html", (m) => m.HtmlCreate),
};
const Spend = lazyScreen("spend", (m) => m.Spend);
const Budgets = lazyScreen("spend", (m) => m.Budgets);
const ExpensiveTasks = lazyScreen("spend", (m) => m.ExpensiveTasks);
const Principles = lazyScreen("settings", (m) => m.Principles);
const Brand = lazyScreen("brand", (m) => m.BrandPage);
const BrandSetup = lazyScreen("brand", (m) => m.BrandSetup);
const Integrations = lazyScreen("settings", (m) => m.Integrations);
const Models = lazyScreen("settings", (m) => m.Models);
const Tools = lazyScreen("settings", (m) => m.Tools);
const Secrets = lazyScreen("settings", (m) => m.Secrets);
const Limits = lazyScreen("settings", (m) => m.Limits);
const People = lazyScreen("settings", (m) => m.People);
const ServiceAccounts = lazyScreen("settings", (m) => m.ServiceAccounts);
const ApiTokens = lazyScreen("settings", (m) => m.ApiTokens);
const SignInPolicy = lazyScreen("settings", (m) => m.SignInPolicy);
const Audit = lazyScreen("settings", (m) => m.Audit);
const Fleet = lazyScreen("settings", (m) => m.Fleet);
const ConfigScreen = lazyScreen("settings", (m) => m.ConfigScreen);
const Backups = lazyScreen("settings", (m) => m.Backups);
const FileStoragePage = lazyScreen("settings", (m) => m.FileStoragePage);
const Account = lazyScreen("account", (m) => m.Account);
const SignIn = lazyScreen("signin", (m) => m.SignIn);
const Invite = lazyScreen("signin", (m) => m.Invite);
const Reset = lazyScreen("signin", (m) => m.Reset);
const Enrol = lazyScreen("signin", (m) => m.Enrol);

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
    case "code-changes":
      return <Changes />;
    case "code-change":
      return (
        <ChangePage key={`${route.repo}!${route.number}`} repo={route.repo} number={route.number} />
      );
    case "code-repos":
      return <Repositories />;
    case "code-repo":
      return <RepositoryPage key={route.repo} repo={route.repo} />;
    case "code-commit":
      return <CommitPage key={`${route.repo}/${route.sha}`} repo={route.repo} sha={route.sha} />;
    case "code-compare":
      return <ComparePage key={route.repo} repo={route.repo} />;
    case "code-pushes":
      return <Pushes />;
    case "org-chart":
      return <OrgChart />;
    case "roster":
      return <Roster />;
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
    case "artifacts":
      return <ArtifactGallery key={route.section} section={route.section} />;
    case "artifact-folders":
      return <ArtifactFolders />;
    case "artifact-folder":
      return <ArtifactFolderPage key={route.id} id={route.id} />;
    case "artifact":
      return <ArtifactPage key={route.id} id={route.id} />;
    case "new-artifact": {
      if (route.kind === null) return <NewArtifactRedirect />;
      const Create = NEW_ARTIFACT[route.kind];
      return <Create key={route.kind} />;
    }
    case "principles":
      return <Principles />;
    case "brand":
      return <Brand />;
    case "brand-setup":
      return <BrandSetup />;
    case "integrations":
      // ONE PAGE FOR EVERY SELECTION, never keyed on it: the directory holds
      // its place and its readings while the detail beside it changes.
      return <Integrations kind={route.kind} />;
    case "budgets":
      return <Budgets />;
    case "models":
      return <Models key={route.id ?? ""} id={route.id} />;
    case "mcp-tools":
      return (
        <Tools
          key={`${route.server ?? ""}/${route.tool ?? ""}`}
          server={route.server}
          tool={route.tool}
        />
      );
    case "secrets":
      return <Secrets key={route.name ?? ""} name={route.name} />;
    case "limits":
      return <Limits />;
    case "spend":
      return <Spend />;
    case "spend-tasks":
      return <ExpensiveTasks />;
    case "people":
      return <People />;
    case "service-accounts":
      return <ServiceAccounts />;
    case "tokens":
      return <ApiTokens />;
    case "sign-in":
      return <SignInPolicy />;
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
    case "storage":
      return <FileStoragePage />;
    case "audit":
      return <Audit />;
    case "account":
      return <Account />;
  }
}

/**
 * The bare `#/artifacts/new`, which names no kind: an earlier link's, whose
 * `kind=` said which. It is rewritten in place to that kind's own page,
 * keeping the rest of what the link asked (the task it is for), or to the
 * gallery, where the reader picks one; REPLACED, so Back never lands on it.
 */
function NewArtifactRedirect() {
  const route = useRoute();
  const nav = useNavigator();
  useEffect(() => {
    const kind = route.query.get("kind") ?? "";
    if (!isMakeKind(kind)) {
      nav.replace(artifactSectionPath("all"));
      return;
    }
    const rest = new URLSearchParams(route.query);
    rest.delete("kind");
    nav.replace(newArtifactPath(kind), rest);
  }, [route.query, nav]);
  return null;
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
  const known = useServicesKnown();
  const arrived = useOrgArrived();
  const runs = useServicesRun();
  const held = useHeldReading();
  const { accessRefused } = useConnection();
  if (!where.resolved) return <NotFound what={where.what} hint={where.hint} />;
  // A PLACE THE COMPANY DOES NOT RUN THE SERVICE FOR says so in place, once
  // the engine has said what it runs (`lib/services.ts`): never a redirect,
  // since the service's card is on an administrator's page. See
  // `ServiceNotice`.
  const service = serviceOfPath(path);
  if (service !== undefined && known && !runs[service]) {
    return <ServiceNotice service={service} path={path} />;
  }
  // A BROWSER THE ENGINE KNOWS AND WILL NOT SERVE (a refused handshake, never
  // a want of access) is told so in place of every screen but the reader's
  // own: their Account, which reads no socket (`personalPath`, the rule the
  // sidebar's locks are drawn by). See `AccessRefused`.
  if (accessRefused !== null && !personalPath(path)) {
    return <AccessRefused reason={accessRefused} />;
  }
  // AN ITEM SOMEBODY WAS ASKED ABOUT IS OPEN TO THEM, Tracker or not: the
  // engine answers the person an open ask names with the item and takes their
  // answer on it, so the link in their inbox notice or chat message has to
  // open — and its own read decides, refusing anybody it names no ask to in
  // the same words it refuses an item that does not exist.
  if (where.screen === "item") return screenFor(where);
  // A WORKSPACE OR SECTION CLOSED TO THE READER is its refusal and nothing
  // else — see `GrantRequired` — decided by the rule every lock the
  // navigation draws is decided by (`nav.ts`'s `closedBy`, over
  // `sectionOpen`): the workspace's access first (Work is the tracker's), then
  // the section's (Live › Turns is admin's). Over the frame's one reading of
  // who the reader is and what they hold (`useHeldReading`): the viewer's
  // answer, or the session's where the viewer has not answered — and the
  // refusal names the reader from that same reading. Only on an ANSWER, and
  // the first one is waited for: until it is in, the screen asks nothing it
  // may be refused (a cold load of #/settings/secrets sent its guarded reads,
  // was refused, and drew the refusal, a frame before the frame knew to). A
  // read that FAILED is no answer and does not hold the screen: it mounts, and
  // its own reads say what they find.
  if (guardedPath(path)) {
    if (held === null) {
      if (viewer.asking) {
        // NAMED BY WHAT WOULD CLOSE IT to somebody holding nothing — the
        // workspace's label where the workspace is gated, else the section's.
        const what = closedBy(path, [])?.what ?? "this page";
        return <Skeleton variant="text" rows={4} label={`Checking your access to ${what}`} />;
      }
    } else {
      const closed = closedBy(path, held.grants);
      if (closed !== null) {
        return <GrantRequired grants={closed.needs} reader={held.login} />;
      }
    }
  }
  // A COLD LOAD OF A SERVICE'S PAGE WAITS FOR THE PROJECTION, which says
  // whether the company runs it, once nothing above has answered the address
  // (a refusal or a lock is decided without it): mounted on the guess, the
  // page asked its reads and was refused, then gave way to the notice.
  if (service !== undefined && !arrived) {
    return <Skeleton variant="text" rows={4} label="Checking what the company runs" />;
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
