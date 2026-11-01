/**
 * One artifact: its page, addressed by its id (`#/artifacts/{id}`), with the
 * version on show in the address (`version=`) and the panel open at its side
 * (`panel=`), so a link survives a reload and a pasted address in chat.
 *
 * # One slim bar, then the artifact
 *
 * The artifact is the screen, so its page draws a single bar in the frame's
 * place (`OwnPageHeader`, the kit's `ArtifactBar`) and gives the artifact
 * everything under it. The bar holds the way back, what it is (its glyph, its
 * title, renamed in place by an editor, and the version chip), the kind's own
 * views, and at its end the controls that open the rest on demand: Details,
 * History, Data where it keeps any, the people here, Share and More. What the
 * page header, its fact line and the View, Data, History and Settings tabs
 * used to stack over the artifact is in those panels now, one at a time at
 * its side (`panels.ts`), each reached by its key too (`app/keymap.ts`).
 *
 * # The engine decides what this reader sees
 *
 * The head (`artifact`) is shaped by the reader's role: the owner and editors
 * see who it is shared with, a viewer sees the owner and how many shares
 * there are. A reader with no relation is answered exactly as for an artifact
 * nobody holds, and the page says both together (`AccessDeniedCard`), so a link
 * never tells anybody an artifact is there. Every control is drawn for every
 * reader and says why it is held where it is; the engine decides every press.
 *
 * # In the room while the page is open
 *
 * The page joins the artifact's room (`useArtifactRoom`): who else is here is
 * drawn in the bar, what moves on the artifact re-reads the head and the
 * versions, what its datastore commits moves the sheet and the page bridge,
 * and a room the engine withdraws (the reader's share was removed while they
 * had it open) turns the page into the card that says so.
 *
 * # Who is here, and following them
 *
 * The People panel (`panel=people`, opened from the presence stack or its own
 * control) lists everybody in words with Follow, and an agent's turn. Following
 * (`follow={handle}`, so a link can start it) moves this reader to the version
 * and, on a sheet, the collection the followed person is on, under a
 * `FollowBanner`; the reader's own scroll, wheel, press or navigation key in
 * the body stops it, as do Escape and the followed person leaving.
 */

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  ArtifactBar,
  ArtifactPanel,
  ArtifactVersionChip,
  ArtifactWorkspace,
  Button,
  ButtonLink,
  Callout,
  EmptyState,
  FollowBanner,
  PeopleRail,
  PresenceStack,
  Skeleton,
  peopleRailKey,
  useToast,
  type ArtifactBarTool,
  type MenuEntry,
  type NodeHue,
  type PeopleRailSelf,
  EditingStatus,
} from "@crewlethq/ui";
import {
  CopyGlyph,
  DatabaseGlyph,
  ExternalLinkGlyph,
  ImageGlyph,
  InfoGlyph,
  PencilGlyph,
  PinGlyph,
  RotateCcwClockGlyph,
  SettingsGlyph,
  Share2Glyph,
  TrashGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { artifactFolderPath, artifactSectionPath } from "~/app/routes.ts";
import { artifactPlace, useFolderTree } from "~/lib/artifacts/folders.ts";
import { href, useNavigator, useParam, useRoute } from "~/app/router.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { useKeymap } from "~/app/keymap.ts";
import { FrameNavToggle, OwnPageHeader } from "~/app/frame/OwnHeader.tsx";
import { AssistantTrigger } from "~/app/assistant/AssistantTrigger.tsx";
import { useOwnStar, usePageLabels } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { utf8Bytes } from "~/lib/format.ts";
import { useConnection } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { keyedWrite, useKeyedGesture } from "~/lib/keyedWrite.ts";
import { ARTIFACT_TITLE_MAX_BYTES } from "~/contract/artifacts.ts";
import { patchArtifact, untrashArtifact } from "~/lib/artifacts/api.ts";
import { usePins } from "~/lib/artifacts/pins.ts";
import { usePinToggle } from "~/lib/artifacts/pinToggle.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import {
  isViewRegion,
  memberTurn,
  presenceMembers,
  selfWords,
  splitOwnSessions,
  useArtifactPeople,
  type PresencePlace,
} from "~/lib/artifacts/people.ts";
import { useArtifactRoom } from "~/lib/artifacts/room.ts";
import { useDataStore } from "~/lib/artifacts/useDataStore.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { contentIsData, keepsData } from "~/lib/artifacts/kinds.ts";
import { KIND_WORDS, purgeSentence } from "~/lib/artifacts/words.ts";
import {
  newActOpID,
  type ArtifactDetail,
  type ArtifactVersionSummary,
  type LogRefusal,
  type QueryRefusal,
} from "~/protocol/index.ts";
import { Denied, artifactAddress, copyAddress, deniedReason } from "./parts.tsx";
import { ArtifactShare } from "./Share.tsx";
import { ViewTab } from "./ViewTab.tsx";
import { HistoryTab } from "./HistoryTab.tsx";
import { DataTab } from "./DataTab.tsx";
import { DetailsPanel } from "./DetailsPanel.tsx";
import { PurgeControl, SettingsTab, TrashDialog } from "./SettingsTab.tsx";
import {
  KindControlsSink,
  RegionNamesSink,
  type KindControls,
  type RegionNames,
} from "~/lib/artifacts/kindControls.ts";
import {
  PANEL_SPECS,
  panelOf,
  panelTitle,
  spelledTheOldWay,
  withPanel,
  type PanelName,
} from "./panels.ts";
import { useArtifactPresence } from "./presence.ts";
import { followMoves, followTarget, moves } from "~/lib/artifacts/follow.ts";
import { useViewer } from "~/lib/viewer.ts";

/** The head is pushed (`artifact_changed`); this read is the floor under a push the socket missed. */
const DETAIL_POLL_MS = 60_000;

/** How many versions the chip's menu lists before "Every version" takes the rest. */
const CHIP_VERSIONS = 8;

/** Where the bar's way back goes when the artifact is in no folder the reader sees: the gallery. */
const BACK = { href: href(["artifacts"]), label: "Back to artifacts" };

/**
 * Where an artifact's page leads back to: the folder it is kept in, named,
 * where the reader can see it, and the gallery otherwise. The tree is read
 * only for an artifact filed in a folder.
 */
function useWayBack(folder: string | undefined) {
  const tree = useFolderTree(Boolean(folder));
  const place = artifactPlace(folder, tree.index);
  const back =
    place.at === "folder"
      ? { href: href(artifactFolderPath(place.id)), label: `Back to ${place.name}` }
      : BACK;
  return { place, back };
}

export function ArtifactPage({ id }: { id: string }) {
  const detail = useQuery("artifact", { id }, { pollMs: DETAIL_POLL_MS });
  const versions = useQuery(
    "artifact_versions",
    { id, limit: 100 },
    { enabled: detail.data !== null },
  );
  const head = detail.data;
  const live = head?.status === "live";
  // THE STAR IS IN THIS PAGE'S MORE MENU, beside Pin, so the frame draws none.
  useOwnStar();

  // THE SHEET AND THE DATA PANEL write as the dashboard; a page's bridge
  // writes as the page's version, through a store of its own (ViewTab).
  const store = useDataStore(id, { via: "dashboard" }, live === true && keepsData(head!));
  const bridgeStores = useRef(new Set<ArtifactDataStore>());

  const room = useArtifactRoom(id, live === true, {
    onJoined: (answer) => {
      store?.joined(answer);
      for (const s of bridgeStores.current) s.joined(answer);
    },
    onData: (push) => {
      store?.applyPush(push);
      for (const s of bridgeStores.current) s.applyPush(push);
    },
    onChanged: (push) => {
      // A HINT, NEVER CONTENT: what moved is read again through the
      // questions that decide what this reader may see.
      detail.refetch();
      if (push.what === "version" || push.what === "reset") versions.refetch();
    },
  });

  usePageLabels(head ? { [id]: head.title } : {});

  if (detail.error && !head) {
    const reason = deniedReason(detail.error);
    if (reason) return <Denied reason={reason} />;
  }
  if (room.state.kind === "refused" && room.state.code === "not_found") {
    return <Denied reason="revoked" />;
  }
  if (!head) {
    return detail.loading ? (
      <>
        {/* THE BAR STANDS WHILE THE HEAD IS READ, so the frame's own bar is
            never drawn for a moment and then swapped for this one. */}
        <OwnPageHeader>
          <ArtifactBar
            end={<AssistantTrigger />}
            title=""
            kind="html"
            kindLabel="Artifact"
            pending
            back={BACK}
            start={<FrameNavToggle />}
          />
        </OwnPageHeader>
        <div className="artifact-loading">
          <Skeleton variant="text" rows={6} label="Loading the artifact" />
        </div>
      </>
    ) : (
      <QueryState error={detail.error} refusal={detail.refusal} loading={false} />
    );
  }
  return head.status === "trashed" ? (
    <Trashed detail={head} onChanged={detail.refetch} />
  ) : (
    <Loaded
      detail={head}
      versions={versions.data?.versions ?? []}
      versionsError={versions.error}
      versionsRefusal={versions.refusal}
      room={room}
      store={store}
      bridgeStores={bridgeStores.current}
      refetch={detail.refetch}
    />
  );
}

/** A trashed artifact's page: what it was, and the way back for its owner. */
function Trashed({ detail, onChanged }: { detail: ArtifactDetail; onChanged: () => void }) {
  const people = useArtifactPeople();
  const restore = useArtifactAccess({ role: "owner", held: detail.role, person: true });
  const gesture = useKeyedGesture();
  const toast = useToast();
  const trashedBy = people.seatName(detail.trashed_by.name);
  return (
    <>
      <OwnPageHeader>
        <ArtifactBar
          end={<AssistantTrigger />}
          title={detail.title}
          kind={detail.kind}
          icon={detail.icon}
          kindLabel={KIND_WORDS[detail.kind]}
          start={<FrameNavToggle />}
          back={{
            href: href(artifactSectionPath("yours"), { status: "trashed" }),
            label: "Back to the trash",
          }}
          version={<span className="artifact-trashed-chip">In the trash</span>}
        />
      </OwnPageHeader>
      <div className="artifact-trashed">
        <EmptyState
          icon={<TrashGlyph size="xl" />}
          title="This artifact is in the trash"
          // WHY THE WAY BACK IS HELD, said where it is read: a held Restore
          // otherwise only said so under the pointer.
          description={`${trashedBy} moved it to the trash. ${purgeSentence(detail.trashed_at)}${
            restore.can || restore.block === "loading" ? "" : ` ${restore.reason}`
          }`}
          action={
            <span className="artifact-actions">
              <Button
                size="small"
                variant="primary"
                loading={gesture.busy}
                disabledReason={restore.can ? undefined : restore.reason}
                onClick={async () => {
                  const answer = await gesture.run(untrashArtifact(detail.id));
                  if (answer?.kind === "done") {
                    toast.ok(`Restored “${detail.title}”`);
                    onChanged();
                  } else if (answer) {
                    toast.show({
                      variant: "danger",
                      title: "Could not restore it",
                      message: answer.text,
                    });
                  }
                }}
              >
                Restore
              </Button>
              <PurgeControl detail={detail} />
            </span>
          }
        />
      </div>
    </>
  );
}

function Loaded({
  detail,
  versions,
  versionsError,
  versionsRefusal,
  room,
  store,
  bridgeStores,
  refetch,
}: {
  detail: ArtifactDetail;
  versions: ArtifactVersionSummary[];
  versionsError: string | null;
  versionsRefusal: QueryRefusal | LogRefusal | null;
  room: ReturnType<typeof useArtifactRoom>;
  store: ReturnType<typeof useDataStore>;
  bridgeStores: Set<ArtifactDataStore>;
  refetch: () => void;
}) {
  // THE PAGE HAS THE WINDOW'S HEIGHT: the body scrolls inside itself and a
  // panel beside it stays in view however far the artifact is read.
  useFillScreen(true);
  const { place: kept, back } = useWayBack(detail.folder);
  const people = useArtifactPeople();
  const toast = useToast();
  const connection = useConnection();
  const route = useRoute();
  const nav = useNavigator();
  const [versionParam, setVersion] = useParam("version", "");
  const asked = Number(versionParam);
  const version = Number.isInteger(asked) && asked > 0 && asked <= detail.head_number ? asked : 0;

  // THE HEAD THIS READER IS LOOKING AT: a version published while the page is
  // open is offered, never loaded under the reader's hands.
  const [shownHead, setShownHead] = useState(detail.head_number);
  useEffect(() => {
    if (shownHead === 0 && detail.head_number > 0) setShownHead(detail.head_number);
  }, [detail.head_number, shownHead]);
  const showing = version > 0 ? version : shownHead;
  const newer = version === 0 && detail.head_number > shownHead;

  // ---- the panel at the side ------------------------------------------------
  const hasData = keepsData(detail) && store !== null;
  const panel = panelOf(route.query, keepsData(detail));
  // AN OLDER LINK'S `tab=` OR `rail=` is read as the panel it became, and the
  // address rewritten in place.
  useEffect(() => {
    if (spelledTheOldWay(route.query)) nav.replace(route.path, withPanel(route.query, panel));
  }, [route.path, route.query, panel, nav]);
  // FOCUS GOES TO A PANEL THE READER OPENED, never to one a link opened.
  const readerOpened = useRef(false);
  const [settingsFocus, setSettingsFocus] = useState<"icon" | undefined>(undefined);
  const openPanel = useCallback(
    (next: PanelName | null) => {
      readerOpened.current = true;
      if (next !== "settings") setSettingsFocus(undefined);
      nav.to(route.path, withPanel(route.query, next));
    },
    [nav, route.path, route.query],
  );
  const toggle = useCallback(
    (p: PanelName) => openPanel(panel === p ? null : p),
    [openPanel, panel],
  );
  const [expandParam] = useParam("expand", "");
  const [againstParam] = useParam("against", "");
  // A COMPARISON TAKES THE WORKSPACE: two versions side by side do not fit
  // the side's width.
  const comparing = panel === "history" && againstParam !== "";
  const expanded = panel !== null && (expandParam === "1" || comparing);
  const setExpanded = useCallback(
    (on: boolean) => nav.filter(on ? { expand: "1" } : { expand: null, against: null }),
    [nav],
  );
  const panelBox = useRef<HTMLDivElement>(null);
  /** Whether the open panel is drawn over the artifact rather than beside it. */
  const covering = () => {
    // THE PANEL'S ROOT BY ITS ROLE, never its element: the kit draws it as a
    // complementary region, and an element name is its business to change.
    const panel = panelBox.current?.closest('[role="complementary"]');
    return expanded || (panel ? getComputedStyle(panel).position === "absolute" : false);
  };

  // ---- who is here, and following them --------------------------------------
  const presence = useArtifactPresence(room, people, showing);
  const viewer = useViewer();
  // THE NAME THE ROOM KNOWS THIS READER BY: their seat, or the login of a
  // reader acting under none, as the engine names every member (its
  // `iam.RecordOwner`, which the frame holds as `owner`).
  const selfName = viewer.owner;
  const [follow, setFollow] = useParam("follow", "");
  const [sheetParam] = useParam("sheet", "");
  const [slideParam] = useParam("slide", "");
  const [doctabParam] = useParam("doctab", "");
  // WHO IS FOLLOWED is somebody else in the room: following oneself moves
  // nothing, and a seat that left is said once and then let go.
  const followed = follow && follow !== selfName ? follow : "";
  const target = useMemo(
    () => (followed ? followTarget(room.others, followed) : null),
    [room.others, followed],
  );
  // WHO STOPPED BEING FOLLOWABLE, and whether they had been here: a seat that
  // left is said to have left, one a link named who was never here is said
  // to be away.
  const [left, setLeft] = useState<{ seat: string; wasHere: boolean } | null>(null);
  const seenTarget = useRef(false);
  const stopFollowing = useCallback(() => setFollow(""), [setFollow]);
  useEffect(() => {
    if (!followed || room.state.kind !== "joined") return;
    if (target === null) {
      setLeft({ seat: followed, wasHere: seenTarget.current });
      seenTarget.current = false;
      setFollow("");
      return;
    }
    seenTarget.current = true;
    const move = followMoves(target, {
      kind: detail.kind,
      head: detail.head_number,
      showing,
      // THE ARTIFACT IS IN VIEW unless a panel has taken the workspace.
      tab: expanded ? (panel ?? "view") : "view",
      sheet: sheetParam,
      slide: slideParam,
      doctab: doctabParam,
    });
    if (!moves(move)) return;
    // ONE REPLACE FOR THE WHOLE MOVE: following is not a place the reader
    // went, so Back does not walk through every version they were taken to.
    nav.filter({
      ...(move.version !== undefined ? { version: move.version || null } : {}),
      ...(move.sheet !== undefined ? { sheet: move.sheet } : {}),
      ...(move.slide !== undefined ? { slide: move.slide } : {}),
      ...(move.doctab !== undefined ? { doctab: move.doctab } : {}),
      ...(move.tab !== undefined ? { expand: null, against: null } : {}),
    });
  }, [
    followed,
    target,
    room.state.kind,
    detail.kind,
    detail.head_number,
    showing,
    expanded,
    panel,
    sheetParam,
    slideParam,
    doctabParam,
    nav,
    setFollow,
  ]);
  // THE READER'S OWN MOVE OF THE VIEW STOPS FOLLOWING: a wheel, a touch
  // drag, a press or a navigation key inside the artifact's body. Escape is
  // the banner's own.
  const body = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = body.current;
    if (!followed || !el) return undefined;
    const stop = () => setFollow("");
    const onKey = (event: KeyboardEvent) => {
      if (NAVIGATION_KEYS.has(event.key)) stop();
    };
    el.addEventListener("wheel", stop, { passive: true });
    el.addEventListener("touchmove", stop, { passive: true });
    el.addEventListener("pointerdown", stop);
    el.addEventListener("keydown", onKey);
    return () => {
      el.removeEventListener("wheel", stop);
      el.removeEventListener("touchmove", stop);
      el.removeEventListener("pointerdown", stop);
      el.removeEventListener("keydown", onKey);
    };
  }, [followed, setFollow]);
  const held = useRef(new Map<string, NodeHue>());
  // THE READER'S OWN OTHER TABS are counted beside "You", never drawn as a
  // collaborator with a Follow that would move nothing.
  const { others, ownTabs } = useMemo(
    () => splitOwnSessions(room.others, selfName),
    [room.others, selfName],
  );
  // WHERE SOMEBODY IS, as the reader sees it: a row by its number in the
  // sheet's own order, a column by its label, a doc's tab or a deck's slide
  // by the name its view publishes (`useRegionNames`).
  const [regions, setRegions] = useState<RegionNames | null>(null);
  const schema = store?.schema ?? null;
  const place = useMemo<PresencePlace>(
    () => ({
      row: (collection, doc) => {
        const at = store?.docsOf(collection).findIndex((d) => d.doc.id === doc) ?? -1;
        return at >= 0 ? `row ${at + 1}` : null;
      },
      field: (collection, field) =>
        schema?.collections
          .find((c) => c.name === collection)
          ?.columns?.find((c) => c.key === field)?.label ?? null,
      region: (region) => regions?.[region] ?? null,
    }),
    [store, schema, regions],
  );
  const drawn = useMemo(() => {
    const out = presenceMembers(others, people, held.current, place);
    held.current = out.hues;
    return out.members;
  }, [others, people, place]);
  // The reader, first in the People panel as "You", drawn as their own seat,
  // or as the person their login is where they hold none: left out, a reader
  // signed in with no seat saw a panel of everybody but themselves.
  const selfFace = useMemo<PeopleRailSelf | undefined>(() => {
    if (!viewer.handle) return viewer.login ? { name: viewer.login, kind: "human" } : undefined;
    const p = people.seatPrincipal(viewer.handle);
    return {
      name: p.name,
      kind: p.kind === "person" ? "human" : "agent",
      character: p.character,
      hue: p.hue,
      src: p.src,
    };
  }, [viewer.handle, viewer.login, people]);

  // ---- the bar's actions ----------------------------------------------------
  // WHETHER THE READER PINNED IT is their own record, which the head does not
  // carry: their pins are a listing of their own, the same one the sidebar
  // draws, and every press anywhere in this tab asks it again.
  const pins = usePins();
  const pin = usePinToggle(
    detail.id,
    (pins.data?.contents ?? []).some((item) => item.id === detail.id),
  );
  const renameAccess = useArtifactAccess({ role: "editor", held: detail.role });
  const trashAccess = useArtifactAccess({ role: "owner", held: detail.role, person: true });
  const [renaming, setRenaming] = useState(false);
  const [sharing, setSharing] = useState(false);
  const [trashing, setTrashing] = useState(false);
  const [kind, setKind] = useState<KindControls | null>(null);

  // RENAMED IN PLACE, as the Settings panel's Title field renames it: the same
  // bound, the same route, and the engine's refusal said where it was typed.
  const rename = useCallback(
    async (title: string): Promise<string | undefined> => {
      if (utf8Bytes(title) > ARTIFACT_TITLE_MAX_BYTES)
        return `That title is too long. Keep it to ${ARTIFACT_TITLE_MAX_BYTES} characters.`;
      const answer = await keyedWrite({
        ...patchArtifact(detail.id, { title }),
        key: newActOpID(),
      });
      if (answer.kind === "done") {
        refetch();
        return undefined;
      }
      return answer.text;
    },
    [detail.id, refetch],
  );

  useKeymap({
    "artifact.details": () => toggle("details"),
    "artifact.history": () => toggle("history"),
    "artifact.data": { run: () => toggle("data"), when: hasData },
    "artifact.people": () => toggle("people"),
    "artifact.share": () => setSharing(true),
  });

  const peopleLabel =
    drawn.length === 0
      ? "People here"
      : `People here, ${drawn.length} ${drawn.length === 1 ? "other" : "others"}`;
  const tools: ArtifactBarTool[] = [
    {
      key: "details",
      label: "Details",
      icon: <InfoGlyph size="sm" />,
      keys: ["i"],
      pressed: panel === "details",
      onSelect: () => toggle("details"),
    },
    {
      key: "history",
      label: panelTitle("history", !contentIsData(detail.kind)),
      icon: <RotateCcwClockGlyph size="sm" />,
      keys: ["h"],
      pressed: panel === "history",
      onSelect: () => toggle("history"),
    },
    ...(hasData
      ? [
          {
            key: "data",
            label: "Data",
            icon: <DatabaseGlyph size="sm" />,
            keys: ["d"],
            pressed: panel === "data",
            onSelect: () => toggle("data"),
          },
        ]
      : []),
    {
      key: "people",
      label: peopleLabel,
      icon: <UsersGlyph size="sm" />,
      keys: ["p"],
      pressed: panel === "people",
      ...(drawn.length > 0 ? { badge: drawn.length } : {}),
      onSelect: () => toggle("people"),
    },
    {
      key: "share",
      label: "Share",
      icon: <Share2Glyph size="sm" />,
      keys: ["s"],
      labelled: true,
      onSelect: () => setSharing(true),
    },
  ];
  const menu: MenuEntry[] = [
    {
      key: "rename",
      label: "Rename",
      icon: <PencilGlyph size="sm" />,
      ...(renameAccess.can
        ? { onSelect: () => setRenaming(true) }
        : { onSelect: () => {}, disabled: true, description: renameAccess.reason }),
    },
    {
      key: "icon",
      label: "Change icon",
      icon: <ImageGlyph size="sm" />,
      ...(renameAccess.can
        ? {
            onSelect: () => {
              setSettingsFocus("icon");
              readerOpened.current = true;
              nav.to(route.path, withPanel(route.query, "settings"));
            },
          }
        : { onSelect: () => {}, disabled: true, description: renameAccess.reason }),
    },
    {
      key: "settings",
      label: "Settings",
      icon: <SettingsGlyph size="sm" />,
      onSelect: () => toggle("settings"),
    },
    ...(kind?.menu && kind.menu.length > 0
      ? [{ kind: "separator" as const, key: "sep:kind" }, ...kind.menu]
      : []),
    { kind: "separator", key: "sep:links" },
    {
      key: "tab",
      label: "Open in a new tab",
      icon: <ExternalLinkGlyph size="sm" />,
      // THE DASHBOARD'S PAGE, never the frame's own address: a page runs
      // only inside its sandboxed frame, under the host that answers its
      // bridge, and a frame URL opened as a window would be neither. It opens
      // at the version on show, as the reader is looking at it.
      onSelect: () =>
        void window.open(
          artifactAddress(detail.id, version || undefined),
          "_blank",
          "noopener,noreferrer",
        ),
    },
    {
      key: "copy",
      label: "Copy link",
      icon: <CopyGlyph size="sm" />,
      onSelect: () => void copyAddress(detail.id, toast),
    },
    {
      key: "pin",
      label: pin.label,
      icon: <PinGlyph size="sm" />,
      onSelect: () => void pin.toggle(),
    },
    { kind: "separator", key: "sep:trash" },
    {
      key: "trash",
      label: "Move to the trash",
      icon: <TrashGlyph size="sm" />,
      danger: true,
      ...(trashAccess.can
        ? { onSelect: () => setTrashing(true) }
        : { onSelect: () => {}, disabled: true, description: trashAccess.reason }),
    },
  ];

  const versioned = !contentIsData(detail.kind) && detail.head_number > 0;
  const chipItems: MenuEntry[] = [
    ...versions.slice(0, CHIP_VERSIONS).map((v): MenuEntry => ({
      key: `v${v.number}`,
      label:
        v.number === detail.head_number ? `Version ${v.number}, the latest` : `Version ${v.number}`,
      ...(v.label ? { description: v.label } : {}),
      checked: v.number === showing,
      onSelect: () => {
        setVersion(v.number === detail.head_number ? "" : String(v.number));
        if (v.number === detail.head_number) setShownHead(detail.head_number);
      },
    })),
    { kind: "separator", key: "sep" },
    {
      key: "history",
      label:
        versions.length > CHIP_VERSIONS ? "Every version and the history" : "Versions and history",
      icon: <RotateCcwClockGlyph size="sm" />,
      onSelect: () => openPanel("history"),
    },
  ];

  const panelBody: Record<PanelName, () => ReactNode> = {
    details: () => (
      <DetailsPanel detail={detail} showing={showing} place={kept} onChanged={refetch} />
    ),
    history: () => (
      <HistoryTab
        detail={detail}
        versions={versions}
        versionsError={versionsError}
        versionsRefusal={versionsRefusal}
        showing={showing}
        onShow={(n: number) => {
          setVersion(n === detail.head_number ? "" : String(n));
          // THE VERSION IS LOOKED AT, so a panel drawn over the artifact
          // makes way for it; one beside it stays, to pick the next.
          if (covering()) openPanel(null);
        }}
        room={room}
        presence={presence}
        bridgeStores={bridgeStores}
      />
    ),
    data: () => (store ? <DataTab detail={detail} store={store} /> : null),
    people: () => (
      <PeopleRail
        variant="plain"
        members={drawn}
        self={selfFace}
        following={followed || undefined}
        onFollow={(member) => {
          setLeft(null);
          setFollow(member ? peopleRailKey(member) : "");
          // FOLLOWING IS WATCHING THE ARTIFACT: a panel over it steps aside.
          if (member && covering()) openPanel(null);
        }}
        labels={{ you: selfWords(ownTabs) }}
        actions={(member) => {
          const turn = memberTurn(room.roster.members, member);
          return turn ? (
            <ButtonLink size="small" variant="ghost" href={href(["live", "turns", turn])}>
              Open turn
            </ButtonLink>
          ) : null;
        }}
      />
    ),
    settings: () => <SettingsTab detail={detail} onChanged={refetch} focus={settingsFocus} />,
  };

  const notices: ReactNode[] = [];
  if (detail.elevated)
    notices.push(
      <Callout key="elevated" layout="banner" variant="info" title="Opened as an administrator">
        The engine recorded it: its owner and editors see that in its history, and every
        administrator sees it on the company's audit trail.
      </Callout>,
    );
  if (room.state.kind === "refused" && room.state.code !== "not_found")
    notices.push(
      <Callout key="room" layout="banner" variant="warning" title="Live changes are paused">
        This page could not join the artifact's room, so others' changes and who is here are not
        shown live.
      </Callout>,
    );
  if (!connection.connected)
    notices.push(
      <Callout key="offline" layout="banner" variant="warning" title="Offline">
        Changes made by others will appear when the connection comes back.
      </Callout>,
    );
  // AN OLDER VERSION IS SAID, not only tinted on the chip: it is read, never
  // edited, and the way back to the latest is one press.
  if (versioned && version > 0 && version < detail.head_number)
    notices.push(
      <Callout
        key="older"
        layout="banner"
        variant="info"
        action={
          <Button
            size="small"
            variant="secondary"
            onClick={() => {
              setVersion("");
              setShownHead(detail.head_number);
            }}
          >
            Show the latest
          </Button>
        }
      >
        You are looking at version {version}, an older version. The latest is version{" "}
        {detail.head_number}.
      </Callout>,
    );
  if (newer)
    notices.push(
      <Callout
        key="newer"
        layout="banner"
        variant="info"
        live="polite"
        action={
          <Button size="small" variant="secondary" onClick={() => setShownHead(detail.head_number)}>
            Show it
          </Button>
        }
      >
        Version {detail.head_number} was published while you were looking.
      </Callout>,
    );

  return (
    <div className="artifact-page">
      <OwnPageHeader>
        <ArtifactBar
          end={<AssistantTrigger />}
          title={detail.title}
          kind={detail.kind}
          icon={detail.icon}
          kindLabel={KIND_WORDS[detail.kind]}
          start={<FrameNavToggle />}
          back={back}
          {...(renameAccess.can ? { onRename: rename } : {})}
          renaming={renaming}
          onRenamingChange={setRenaming}
          version={
            versioned ? (
              <ArtifactVersionChip
                version={showing}
                latest={detail.head_number}
                items={chipItems}
              />
            ) : undefined
          }
          {...(kind?.modes ? { modes: kind.modes } : {})}
          presence={
            drawn.length > 0 ? (
              <PresenceStack
                members={drawn}
                self
                size="xs"
                onSelect={() => openPanel(panel === "people" ? null : "people")}
              />
            ) : undefined
          }
          {...(kind?.editing
            ? { status: <EditingStatus dirty={kind.editing.dirty} saving={kind.editing.saving} /> }
            : {})}
          kindTools={kind?.tools ?? []}
          tools={tools}
          menu={menu}
        />
      </OwnPageHeader>
      {notices.length > 0 && <div className="artifact-notices">{notices}</div>}
      {followed && target && (
        <FollowBanner
          className="artifact-follow"
          name={people.seatName(followed)}
          onStop={stopFollowing}
          labels={{
            following: (name) => {
              // A SLIDE OR A DOC'S TAB IS MOVED TO, not said: the view is
              // already on it. A page's own region is said in its words.
              const region = target.focus.region;
              const where = region && !isViewRegion(region) ? ` They are in ${region}.` : "";
              return `Following ${name}.${where} Press Esc or scroll to stop.`;
            },
          }}
        />
      )}
      {left && !followed && (
        <FollowBanner
          className="artifact-follow"
          name={people.seatName(left.seat)}
          state="left"
          onStop={() => setLeft(null)}
          onDismiss={() => setLeft(null)}
          {...(left.wasHere
            ? {}
            : {
                labels: {
                  left: (name: string) => `${name} is not here, so there is nobody to follow.`,
                },
              })}
        />
      )}
      <ArtifactWorkspace
        className="artifact-workspace"
        bodyRef={body}
        bodyClassName={`artifact-body artifact-body--${detail.kind}`}
        bodyLabel={detail.title}
        expanded={expanded}
        panel={
          panel ? (
            <ArtifactPanel
              key={panel}
              title={panelTitle(panel, !contentIsData(detail.kind))}
              size={PANEL_SPECS[panel].size}
              focusOnOpen={readerOpened.current}
              onClose={() => openPanel(null)}
              expanded={expanded}
              {...(panel === "data" || panel === "history"
                ? { onExpandedChange: setExpanded }
                : {})}
            >
              <div className="artifact-panel-body" ref={panelBox}>
                {panelBody[panel]()}
              </div>
            </ArtifactPanel>
          ) : null
        }
      >
        <KindControlsSink.Provider value={setKind}>
          <RegionNamesSink.Provider value={setRegions}>
            <ViewTab
              detail={detail}
              version={showing}
              store={store}
              room={room}
              presence={presence}
              drawn={drawn}
              bridgeStores={bridgeStores}
              onPublished={(n: number) => {
                refetch();
                setVersion("");
                setShownHead(n);
              }}
            />
          </RegionNamesSink.Provider>
        </KindControlsSink.Provider>
      </ArtifactWorkspace>
      <ArtifactShare
        detail={detail}
        open={sharing}
        onClose={() => setSharing(false)}
        onChanged={refetch}
      />
      <TrashDialog detail={detail} open={trashing} onClose={() => setTrashing(false)} />
    </div>
  );
}

/** The keys that move a reader's view of the body, and so stop a follow. */
const NAVIGATION_KEYS = new Set([
  "ArrowUp",
  "ArrowDown",
  "ArrowLeft",
  "ArrowRight",
  "PageUp",
  "PageDown",
  "Home",
  "End",
  " ",
]);
