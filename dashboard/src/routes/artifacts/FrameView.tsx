/**
 * A page artifact, embedded: the engine mints a frame for the version on show
 * (a view ticket bound to this browser's sign-in, the artifact origin's URL
 * under it, and the sandbox and allow attributes), and the design system's
 * `SandboxedFrame` embeds it exactly as minted.
 *
 * THE FRAME NEVER OPENS A WINDOW. A link the page asks to open reaches the
 * page's host, which names the destination first (`LinkInterstitial`) and
 * opens it with no opener and no referrer once the reader says so (D16).
 *
 * THE PAGE TALKS TO THIS HOST, never to the engine: once the frame has checked
 * the page's hello it transfers one port, and every call on it is answered by
 * the [PageHost] built for this frame, bound to this artifact and version.
 *
 * ONE FRAME, ONE DOCUMENT. A page's frame opens its entry; a design's frame
 * opens the one artboard it names (`document`), under a ticket minted for that
 * board alone, so every board on a canvas is a frame of its own.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  LinkInterstitial,
  SandboxedFrame,
  SandboxedFrameNotice,
  SandboxedFramePlaceholder,
  type SandboxedFrameState,
} from "@crewlethq/ui";
import type { ThemeId } from "@crewlethq/tokens";
import {
  newActOpID,
  RestError,
  type ArtifactDetail,
  type ArtifactFrame,
  type ArtifactVersionSummary,
} from "~/protocol/index.ts";
import { useRest } from "~/lib/useRest.ts";
import { useClient, useConnection } from "~/lib/store-hooks.ts";
import { useDensity, usePalette, useTheme } from "~/lib/prefs.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useOwned } from "~/lib/useOwned.ts";
import { mintFrame, reportReady } from "~/lib/artifacts/api.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import { useDataStore } from "~/lib/artifacts/useDataStore.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { rowID, type ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import type { BridgeRoster, BridgeViewer, RuntimeCapability } from "~/runtime/bridge.ts";
import { PageHost } from "./host.ts";
import type { PresenceReport } from "./presence.ts";

/** Why a frame could not be minted, in the reader's words, from the engine's code. */
function mintRefusal(err: RestError): string {
  // NOTHING ANSWERED: the client's own words for a deadline or a dropped
  // connection describe the transport, not the page.
  if (err.status === 0)
    return "The engine could not be reached. Check your connection, then try again.";
  switch (err.code) {
    case "artifact_origin_unset":
      return "This deployment is not set up to show pages yet. Ask an operator to give it an artifact origin (api.artifacts.external_url).";
    case "artifact_not_framed":
      return "This version has no page to show.";
    case "session_revoked":
      return "Your sign-in is ending. Sign in again to open this page.";
    case "artifact_trashed":
      return "This artifact is in the trash. Restore it to open its page.";
    default:
      return err.detail || "The engine could not open this page. Try again in a moment.";
  }
}

export function FrameView({
  detail,
  version,
  versions,
  room,
  presence,
  bridgeStores,
  document,
  title,
  print,
  onReady,
  className,
  dataNote = true,
}: {
  detail: ArtifactDetail;
  version: number;
  versions: ArtifactVersionSummary[];
  room: ArtifactRoom;
  presence: PresenceReport;
  bridgeStores: Set<ArtifactDataStore>;
  /** The page the frame opens: a design's artboard. Left out, the version's entry. */
  document?: string | undefined;
  /** The frame's accessible name, where it is not the artifact's title. */
  title?: string | undefined;
  /** Prints this page alone when it asks (`crewlet.print()`); absent, a print is refused. */
  print?: (() => Promise<boolean>) | undefined;
  /** The page said hello and holds its port: it is drawn. */
  onReady?: (() => void) | undefined;
  className?: string | undefined;
  /** Says under an old version that its data is today's; off where the caller says it once for two. */
  dataNote?: boolean;
}) {
  const [attempt, setAttempt] = useState(0);
  const frame = useRest<ArtifactFrame>(`${detail.id}:${version}:${document ?? ""}:${attempt}`, () =>
    mintFrame(detail.id, version, document),
  );
  // The print a page asks for, read through a ref so a new callback never
  // rebuilds the host and drops the page's port.
  const printRef = useRef(print);
  printRef.current = print;
  const canPrint = print !== undefined;
  // WHEN THE FRAME WAS HANDED OVER, by this browser's clock: the ready report
  // measures from here to the page's hello.
  const handed = useRef(0);
  useEffect(() => {
    if (frame.data) handed.current = performance.now();
  }, [frame.data]);

  // WHAT THE VERSION DECLARES is what the page may use, read off the version
  // itself rather than the head, which may be newer.
  const summary = versions.find((v) => v.number === version);
  const capabilities = useMemo(
    () =>
      (summary?.capabilities ??
        (version === detail.head_number ? detail.capabilities : undefined) ??
        []) as RuntimeCapability[],
    [summary, version, detail.head_number, detail.capabilities],
  );
  const needsData = capabilities.includes("db") || capabilities.includes("state");
  const store = useDataStore(detail.id, { via: "bridge", version }, needsData);
  // THE PAGE'S STORE HEARS THE ROOM the viewer joined: the page's own store
  // writes as its version, and reads the same pushes as the sheet's.
  useEffect(() => {
    if (!store) return undefined;
    bridgeStores.add(store);
    return () => void bridgeStores.delete(store);
  }, [store, bridgeStores]);

  const [link, setLink] = useState<{ url: URL; answer: (opened: boolean) => void } | null>(null);
  const { socket } = useClient();
  const connection = useConnection();
  const viewer = useViewer();
  const people = useArtifactPeople();
  const [clientID] = useState(() => `page-${rowID()}`);
  // THE HOST IS OWNED BY AN EFFECT: it hears the page's store from the moment
  // it is made, so a memo would hand back one a cleanup had stopped.
  const host = useOwned(
    () =>
      !store && needsData
        ? null
        : new PageHost({
            artifact: detail.id,
            version,
            capabilities,
            socket,
            store,
            clientID,
            newKey: () => newActOpID(),
            openLink: (url) => new Promise<boolean>((answer) => setLink({ url, answer })),
            ...(canPrint ? { print: () => printRef.current?.() ?? Promise.resolve(false) } : {}),
            setPresence: (activity, focus) => {
              const { version: _v, ...rest } = focus;
              presence.at(Object.keys(rest).length > 0 ? rest : null, activity === "editing");
            },
          }),
    (made) => made.stop(),
    // The presence report is read through its stable `at`.
    [detail.id, version, capabilities, socket, store, needsData, clientID, canPrint],
  );

  // WHO IS VIEWING, and the people a page may name, to a page that declared
  // it may know: display facts only, from the chart this tab holds.
  const knows = capabilities.includes("identity");
  const bridgeViewer = useMemo<BridgeViewer | null>(
    () =>
      knows && viewer.handle
        ? {
            ...people.pagePerson(viewer.handle, viewer.kind === "agent" ? "agent" : "human"),
            displayName: viewer.name || people.seatName(viewer.handle),
            role: detail.role,
          }
        : null,
    [knows, people, viewer.handle, viewer.kind, viewer.name, detail.role],
  );
  useEffect(() => host?.setViewer(bridgeViewer), [host, bridgeViewer]);
  useEffect(() => host?.setPeople(knows ? people.pagePeople : []), [host, knows, people]);
  useEffect(
    () => host?.setConnection(connection.connected ? "live" : "reconnecting"),
    [host, connection.connected],
  );
  const roster = useMemo<BridgeRoster>(
    () => ({
      members: [...room.roster.members.values()].map((m) => ({
        ...people.pagePerson(m.seat, m.kind === "agent" ? "agent" : "human"),
        session: m.session,
        activity: m.activity,
        focus: m.focus,
        at: m.at,
      })),
      ...(room.roster.counts ? { counts: room.roster.counts } : {}),
      truncated: room.roster.truncated,
    }),
    [room.roster, people],
  );
  useEffect(() => host?.setRoster(roster), [host, roster]);

  const [mode] = useTheme();
  const [palette] = usePalette();
  const [density] = useDensity();
  const theme = useMemo(
    () => ({ mode, palette: palette as ThemeId, density }),
    [mode, palette, density],
  );
  const [state, setState] = useState<SandboxedFrameState>("loading");
  const reported = useRef("");

  // EVERY STATE IS DRAWN IN THE ONE BOX the page will fill, by the frame's own
  // parts: the placeholder while the ticket is minted is the one the frame
  // shows until the page says hello, and a refused ticket is said exactly as
  // a page that failed to start is.
  const box = className ? `artifact-frame-box ${className}` : "artifact-frame-box";
  const name = title ?? detail.title;
  if (frame.error) {
    return (
      <div className={box}>
        <SandboxedFrameNotice
          className="artifact-frame"
          title={name}
          heading="This page could not be opened"
          action={
            <Button variant="outline" size="small" onClick={() => setAttempt((n) => n + 1)}>
              Try again
            </Button>
          }
        >
          {mintRefusal(frame.error)}
        </SandboxedFrameNotice>
      </div>
    );
  }
  if (!frame.data || !host)
    return (
      <div className={box}>
        <SandboxedFramePlaceholder className="artifact-frame" title={name} />
      </div>
    );
  const minted = frame.data;
  const report = (outcome: "ready" | "failed") => {
    if (reported.current === minted.frame_url) return;
    reported.current = minted.frame_url;
    reportReady(minted.frame_url, outcome, performance.now() - handed.current);
  };
  return (
    <div className={box}>
      <SandboxedFrame
        className="artifact-frame"
        src={minted.frame_url}
        origin={minted.origin}
        mode={minted.mode}
        sandbox={minted.sandbox}
        allow={minted.allow}
        title={name}
        theme={theme}
        nonce={minted.nonce}
        onReady={(port) => {
          report("ready");
          host.attach(port);
          onReady?.();
        }}
        onMessage={(data, port) => void host.receive(data, port)}
        onOpenLink={(url) => setLink({ url, answer: () => {} })}
        onStateChange={(next) => {
          setState(next);
          if (next === "failed" || next === "refused" || next === "outdated") report("failed");
          if (next !== "ready") host.detach();
        }}
        onReload={() => setAttempt((n) => n + 1)}
      />
      {dataNote && state === "ready" && version !== detail.head_number && (
        <p className="artifact-note">Every version shows today's data.</p>
      )}
      <LinkInterstitial
        url={link?.url ?? null}
        source={detail.title}
        onOpened={() => {
          link?.answer(true);
        }}
        onClose={() => {
          link?.answer(false);
          setLink(null);
        }}
      />
    </div>
  );
}
