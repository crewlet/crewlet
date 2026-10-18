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
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  Callout,
  EmptyState,
  Menu,
  PresenceStack,
  Skeleton,
  useToast,
  type NodeHue,
} from "@crewlethq/ui";
import { CopyGlyph, EllipsisGlyph, PinGlyph, TrashGlyph } from "@crewlethq/icons/glyphs";
import { useParam } from "~/app/router.tsx";
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
import { useArtifactPeople, presenceMembers } from "~/lib/artifacts/people.ts";
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
  return detail.kind === "sheet" || (detail.capabilities ?? []).includes("db");
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
        description={`${people.seatName(detail.trashed_by.Name)} moved it to the trash. It is deleted for good 30 days after that unless its owner restores it.`}
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
  const held = useRef(new Map<string, NodeHue>());
  const drawn = useMemo(() => {
    const out = presenceMembers(room.others, people, held.current);
    held.current = out.hues;
    return out.members;
  }, [room.others, people]);

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

  const updater = people.seatName(detail.updated_by.Name);
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
              <PresenceStack members={drawn} self size="sm" className="artifact-presence" />
            )}
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
      <div className="artifact-body">
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
  );
}
