/**
 * One artifact: its page, addressed by its id (`#/artifacts/{id}`), with the
 * frame's object tabs (View, Data, History, Settings) and the version on show
 * in the address (`version=`), so a link survives a reload and a pasted
 * address in chat.
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
 * drawn in the header, what moves on the artifact re-reads the head and the
 * versions, what its datastore commits moves the sheet and the page bridge,
 * and a room the engine withdraws (the reader's share was removed while they
 * had it open) turns the page into the card that says so.
 *
 * # Who is here, and following them
 *
 * The People rail (`rail=people`, opened from the presence stack or its own
 * control) lists everybody in words with Follow, and an agent's turn. Following
 * (`follow={handle}`, so a link can start it) moves this reader to the version
 * and, on a sheet, the collection the followed person is on, under a
 * `FollowBanner`; the reader's own scroll, wheel, press or navigation key in
 * the body stops it, as do Escape and the followed person leaving.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  EmptyState,
  FollowBanner,
  Menu,
  PeopleRail,
  PresenceStack,
  Skeleton,
  peopleRailKey,
  useToast,
  type NodeHue,
  type PeopleRailSelf,
} from "@crewlethq/ui";
import {
  CopyGlyph,
  EllipsisGlyph,
  PinGlyph,
  TrashGlyph,
  UsersGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator, useParam } from "~/app/router.tsx";
import { useOwnStar, usePageLabels, usePageMenu } from "~/app/Shell.tsx";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { ObjectTabs } from "~/app/frame/ObjectTabs.tsx";
import { useTab } from "~/app/frame/tabs.ts";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { relTime } from "~/lib/format.ts";
import { useConnection } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { pinArtifact, untrashArtifact } from "~/lib/artifacts/api.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { memberTurn, presenceMembers, useArtifactPeople } from "~/lib/artifacts/people.ts";
import { useArtifactRoom } from "~/lib/artifacts/room.ts";
import { useDataStore } from "~/lib/artifacts/useDataStore.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { KIND_WORDS, ROLE_WORDS } from "~/lib/artifacts/words.ts";
import type {
  ArtifactDetail,
  ArtifactVersionSummary,
  LogRefusal,
  QueryRefusal,
} from "~/protocol/index.ts";
import { Denied, copyAddress, deniedReason } from "./parts.tsx";
import { ShareControl } from "./Share.tsx";
import { ViewTab } from "./ViewTab.tsx";
import { HistoryTab } from "./HistoryTab.tsx";
import { DataTab } from "./DataTab.tsx";
import { PurgeControl, SettingsTab } from "./SettingsTab.tsx";
import { useArtifactPresence } from "./presence.ts";
import { followMoves, followTarget, moves } from "~/lib/artifacts/follow.ts";
import { useViewer } from "~/lib/viewer.ts";

/** The head is pushed (`artifact_changed`); this read is the floor under a push the socket missed. */
const DETAIL_POLL_MS = 60_000;

/** The object tabs, in order: the digits 1 to 4 reach them. */
const TABS = ["view", "data", "history", "settings"] as const;
type Tab = (typeof TABS)[number];

const TAB_WORDS: Readonly<Record<Tab, string>> = {
  view: "View",
  data: "Data",
  history: "History",
  settings: "Settings",
};

/** Whether an artifact keeps a datastore a person can open: a sheet always, a page that declared it. */
function hasData(detail: ArtifactDetail): boolean {
  return detail.kind === "sheet" || detail.capabilities.includes("db");
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

  // THE SHEET AND THE DATA TAB write as the dashboard; a page's bridge
  // writes as the page's version, through a store of its own (ViewTab).
  const store = useDataStore(id, { via: "dashboard" }, live === true && hasData(head!));
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
      <Skeleton variant="text" rows={6} label="Loading the artifact" />
    ) : (
      <QueryState error={detail.error} refusal={detail.refusal} loading={false} />
    );
  }
  return (
    <>
      {head.status === "trashed" ? (
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
      )}
    </>
  );
}

/** A trashed artifact's page: what it was, and the way back for its owner. */
function Trashed({ detail, onChanged }: { detail: ArtifactDetail; onChanged: () => void }) {
  const people = useArtifactPeople();
  const restore = useArtifactAccess({ role: "owner", held: detail.role, person: true });
  const gesture = useKeyedGesture();
  const toast = useToast();
  return (
    <>
      <ObjectHeader kind={KIND_WORDS[detail.kind]} icon="layers" title={detail.title} />
      <EmptyState
        icon={<TrashGlyph size="xl" />}
        title="This artifact is in the trash"
        description={`${people.seatName(detail.trashed_by.name)} moved it to the trash. It is deleted for good 30 days after that unless its owner restores it.`}
        action={
          <span className="artifact-actions">
            <Button
              size="small"
              variant="secondary"
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
  const people = useArtifactPeople();
  const now = useNow();
  const toast = useToast();
  const connection = useConnection();
  const tabs = useMemo(() => TABS.filter((t) => t !== "data" || hasData(detail)), [detail]);
  const [tab, setTab] = useTab("tab", tabs);
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

  const presence = useArtifactPresence(room, people, showing);
  const viewer = useViewer();
  const nav = useNavigator();
  const [rail, setRail] = useParam("rail", "", "section");
  const peopleOpen = rail === "people";
  const [follow, setFollow] = useParam("follow", "");
  const [sheetParam] = useParam("sheet", "");
  // WHO IS FOLLOWED is somebody else in the room: following oneself moves
  // nothing, and a seat that left is said once and then let go.
  const followed = follow && follow !== viewer.handle ? follow : "";
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
      tab,
      sheet: sheetParam,
    });
    if (!moves(move)) return;
    // ONE REPLACE FOR THE WHOLE MOVE: following is not a place the reader
    // went, so Back does not walk through every version they were taken to.
    nav.filter({
      ...(move.version !== undefined ? { version: move.version || null } : {}),
      ...(move.sheet !== undefined ? { sheet: move.sheet } : {}),
      ...(move.tab !== undefined ? { tab: null } : {}),
    });
  }, [
    followed,
    target,
    room.state.kind,
    detail.kind,
    detail.head_number,
    showing,
    tab,
    sheetParam,
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
  const drawn = useMemo(() => {
    const out = presenceMembers(room.others, people, held.current);
    held.current = out.hues;
    return out.members;
  }, [room.others, people]);
  // The reader, first on the People rail as "You", drawn as their own seat.
  const selfFace = useMemo<PeopleRailSelf | undefined>(() => {
    if (!viewer.handle) return undefined;
    const p = people.seatPrincipal(viewer.handle);
    return {
      name: p.name,
      kind: p.kind === "person" ? "human" : "agent",
      character: p.character,
      hue: p.hue,
      src: p.src,
    };
  }, [viewer.handle, people]);

  // WHETHER THE READER PINNED IT is their own record, which the head does not
  // carry: their pins are a listing of their own, read once and kept current
  // by their own presses here.
  const pins = useQuery("artifacts", { scope: "all", pinned: true, limit: 200 });
  const pinGesture = useKeyedGesture();
  const [pinnedHere, setPinned] = useState<boolean | null>(null);
  const pinned = pinnedHere ?? (pins.data?.items ?? []).some((item) => item.id === detail.id);
  const pin = useCallback(async () => {
    const next = !pinned;
    const answer = await pinGesture.run(pinArtifact(detail.id, next));
    if (answer?.kind === "done") {
      setPinned(next);
      toast.ok(next ? "Pinned to your sidebar" : "Removed from your sidebar");
    } else if (answer) {
      toast.show({
        variant: "danger",
        title: next ? "Could not pin it" : "Could not unpin it",
        message: answer.text,
      });
    }
  }, [pinned, pinGesture, detail.id, toast]);

  usePageMenu([
    {
      key: "copy",
      label: "Copy link",
      icon: <CopyGlyph size="sm" />,
      onSelect: () => void copyAddress(detail.id, toast),
    },
    {
      key: "pin",
      label: pinned ? "Unpin" : "Pin to your sidebar",
      icon: <PinGlyph size="sm" />,
      onSelect: () => void pin(),
    },
  ]);

  const updater = people.seatName(detail.updated_by.name);
  return (
    <div className="artifact-page">
      <ObjectHeader
        kind={KIND_WORDS[detail.kind]}
        icon="layers"
        title={detail.title}
        facts={[
          {
            label: "Owner",
            value: people.seatName(detail.owner),
            path: ["agents", "seats", detail.owner],
          },
          {
            label: "Updated",
            value: `${relTime(detail.updated_at, now)} by ${updater}`,
          },
          ...(detail.head_number > 0 ? [{ label: "Version", value: `${detail.head_number}` }] : []),
          {
            label: "Your role",
            value: detail.elevated ? "Administrator" : ROLE_WORDS[detail.role],
          },
        ]}
        actions={
          <span className="artifact-actions">
            {drawn.length > 0 && (
              <PresenceStack
                members={drawn}
                self
                size="sm"
                className="artifact-presence"
                onSelect={() => setRail("people")}
              />
            )}
            <Button
              size="small"
              variant={peopleOpen ? "secondary" : "ghost"}
              leadingIcon={<UsersGlyph size="sm" />}
              pressed={peopleOpen}
              onClick={() => setRail(peopleOpen ? "" : "people")}
            >
              People
            </Button>
            <ShareControl detail={detail} onChanged={refetch} />
            <span className="page-action-folds">
              <Menu
                label={`More for “${detail.title}”`}
                icon={<EllipsisGlyph size="sm" />}
                triggerVariant="ghost"
                align="end"
                items={[
                  {
                    key: "copy",
                    label: "Copy link",
                    icon: <CopyGlyph size="sm" />,
                    onSelect: () => void copyAddress(detail.id, toast),
                  },
                  {
                    key: "pin",
                    label: pinned ? "Unpin" : "Pin to your sidebar",
                    icon: <PinGlyph size="sm" />,
                    onSelect: () => void pin(),
                  },
                ]}
              />
            </span>
          </span>
        }
      />
      {detail.elevated && (
        <Callout variant="info" title="You can open this because you are an administrator">
          The engine recorded that you opened it: its owner and editors see that in its history, and
          every administrator sees it on the company's audit trail.
        </Callout>
      )}
      {room.state.kind === "refused" && room.state.code !== "not_found" && (
        <Callout variant="warning" title="Live changes are paused">
          This page could not join the artifact's room, so others' changes and who is here are not
          shown live.
        </Callout>
      )}
      {!connection.connected && (
        <Callout variant="warning" title="Offline">
          Changes made by others will appear when the connection comes back.
        </Callout>
      )}
      <ObjectTabs
        ariaLabel={`${detail.title} tabs`}
        items={tabs.map((t) => ({ value: t, label: TAB_WORDS[t] }))}
        value={tab}
        onValueChange={(value) => setTab(value as Tab)}
      />
      {version > 0 && (
        <div className="artifact-bar" role="status">
          <span>
            Viewing version {version} of {detail.head_number}.
          </span>
          <Button size="small" variant="ghost" onClick={() => setVersion("")}>
            Back to latest
          </Button>
        </div>
      )}
      {newer && (
        <div className="artifact-bar" role="status">
          <span>Version {detail.head_number} was published.</span>
          <Button size="small" variant="secondary" onClick={() => setShownHead(detail.head_number)}>
            Load it
          </Button>
        </div>
      )}
      {followed && target && (
        <FollowBanner
          name={people.seatName(followed)}
          onStop={stopFollowing}
          labels={{
            following: (name) => {
              const where = target.focus.region ? ` They are in ${target.focus.region}.` : "";
              return `Following ${name}.${where} Press Esc or scroll to stop.`;
            },
          }}
        />
      )}
      {left && !followed && (
        <FollowBanner
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
      <div className={peopleOpen ? "artifact-with-rail" : undefined}>
        {peopleOpen && (
          <PeopleRail
            className="artifact-people"
            members={drawn}
            self={selfFace}
            following={followed || undefined}
            onFollow={(member) => {
              setLeft(null);
              setFollow(member ? peopleRailKey(member) : "");
            }}
            onClose={() => setRail("")}
            actions={(member) => {
              const turn = memberTurn(room.roster.members, member);
              return turn ? (
                <ButtonLink size="small" variant="ghost" href={href(["live", "turns", turn])}>
                  Open turn
                </ButtonLink>
              ) : null;
            }}
          />
        )}
        <div className="artifact-body" ref={body}>
          {tab === "view" && (
            <ViewTab
              detail={detail}
              version={showing}
              versions={versions}
              store={store}
              room={room}
              presence={presence}
              drawn={drawn}
              bridgeStores={bridgeStores}
            />
          )}
          {tab === "data" && store && <DataTab detail={detail} store={store} />}
          {tab === "history" && (
            <HistoryTab
              detail={detail}
              versions={versions}
              versionsError={versionsError}
              versionsRefusal={versionsRefusal}
              showing={showing}
              onShow={(n: number) => {
                setVersion(n === detail.head_number ? "" : String(n));
                setTab("view");
              }}
              room={room}
              presence={presence}
              bridgeStores={bridgeStores}
            />
          )}
          {tab === "settings" && <SettingsTab detail={detail} onChanged={refetch} />}
        </div>
      </div>
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
