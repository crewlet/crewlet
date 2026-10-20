/**
 * The draft's page, live: the version the last preview published, framed on
 * the artifact origin exactly as its own page frames it, so what the person
 * tries here (a vote, a form, who is here) is what everyone they share it with
 * will get.
 *
 * THE SAME FRAME AS THE ARTIFACT'S PAGE, composed from the same pieces: the
 * draft's head (`artifact`), its room, the presence a page reports through,
 * and `FrameView`, which mints the frame and hosts the page's bridge. The
 * frame is keyed by the version, so a new preview loads the new version.
 */

import { useEffect, useRef } from "react";
import { Skeleton } from "@crewlethq/ui";
import { useQuery } from "~/lib/useQuery.ts";
import { useArtifactRoom } from "~/lib/artifacts/room.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { FrameView } from "../../FrameView.tsx";
import { useArtifactPresence } from "../../presence.ts";

export function DraftPreview({
  id,
  version,
  title,
}: {
  id: string;
  version: number;
  title: string;
}) {
  const detail = useQuery("artifact", { id });
  const versions = useQuery(
    "artifact_versions",
    { id, limit: 100 },
    { enabled: detail.data !== null },
  );
  const bridgeStores = useRef(new Set<ArtifactDataStore>());
  const live = detail.data?.status === "live";
  const room = useArtifactRoom(id, live, {
    onJoined: (answer) => {
      for (const s of bridgeStores.current) s.joined(answer);
    },
    onData: (push) => {
      for (const s of bridgeStores.current) s.applyPush(push);
    },
  });
  const people = useArtifactPeople();
  const presence = useArtifactPresence(room, people, version);
  const head = detail.data;
  // THE HEAD IS READ AGAIN until it names the version the preview published:
  // the socket's copy can trail the publish by a moment.
  const behind = head !== null && head.head_number < version;
  const { refetch } = detail;
  useEffect(() => {
    if (!behind) return undefined;
    const timer = setTimeout(refetch, 400);
    return () => clearTimeout(timer);
  }, [behind, refetch, head]);
  if (!head || behind) {
    return (
      <div className="html-create-preview-wait">
        <Skeleton variant="box" label="Opening the preview" />
      </div>
    );
  }
  return (
    <FrameView
      key={version}
      detail={head}
      version={version}
      versions={versions.data?.versions ?? []}
      room={room}
      presence={presence}
      bridgeStores={bridgeStores.current}
      title={`Preview of ${title}`}
      dataNote={false}
    />
  );
}
