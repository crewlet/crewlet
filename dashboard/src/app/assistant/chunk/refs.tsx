/**
 * The one renderer of the records a change card names (`AssistantRef`): a seat's
 * face, an artifact's kind glyph, any other kind's glyph, linked and opened in place.
 */

import { Fragment, type ReactNode } from "react";
import { ArtifactKindGlyph, ArtifactRef, InlineRef } from "@crewlethq/ui";
import {
  ActivityGlyph,
  BotGlyph,
  CalendarClockGlyph,
  FileTextGlyph,
  FolderGlyph,
  FolderKanbanGlyph,
  GlobeGlyph,
  LayersGlyph,
  ListFilterGlyph,
  MapGlyph,
  MessageSquareGlyph,
  SquareKanbanGlyph,
  UsersGlyph,
  type GlyphSize,
} from "@crewlethq/icons/glyphs";
import type { AssistantRef } from "~/contract/assistant.ts";
import type { AssistantPhrase } from "./phrase.ts";
import { isArtifactKind, KIND_WORDS as ARTIFACT_WORDS } from "~/lib/artifacts/words.ts";
import { seatLookup, useOrgIndex } from "~/lib/seats.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { href as hrefOf } from "../../router.tsx";
import { openInApp, type InAppTarget } from "./openInApp.ts";

type Kind = AssistantRef["kind"];

/** Each kind of record as its glyph: decoration, the words are read. */
const KIND_GLYPH: Record<Kind, (size?: GlyphSize) => ReactNode> = {
  task: (size) => <SquareKanbanGlyph size={size} />,
  project: (size) => <FolderKanbanGlyph size={size} />,
  seat: (size) => <BotGlyph size={size} />,
  unit: (size) => <UsersGlyph size={size} />,
  page: (size) => <FileTextGlyph size={size} />,
  container: (size) => <FolderGlyph size={size} />,
  artifact: (size) => <LayersGlyph size={size} />,
  turn: (size) => <ActivityGlyph size={size} />,
  run: (size) => <ActivityGlyph size={size} />,
  schedule: (size) => <CalendarClockGlyph size={size} />,
  view: (size) => <ListFilterGlyph size={size} />,
  room: (size) => <MessageSquareGlyph size={size} />,
  atlas_entry: (size) => <MapGlyph size={size} />,
  web_page: (size) => <GlobeGlyph size={size} />,
};

/** What each kind is said as before its name, to a screen reader. */
const KIND_SAID: Record<Kind, string> = {
  task: "Task",
  project: "Project",
  seat: "Agent",
  unit: "Team",
  page: "Page",
  container: "Space",
  artifact: "Artifact",
  turn: "Turn",
  run: "Executor",
  schedule: "Schedule",
  view: "View",
  room: "Room",
  atlas_entry: "Entry",
  web_page: "Web page",
};

/** The kinds named by an identifier (a key, an id), set in mono as a task's key is. */
const CODE: ReadonlySet<Kind> = new Set<Kind>(["task", "project", "turn", "run"]);

/** A seat's face, off the chart: an agent its character, a person their initials. */
function SeatFace({ handle }: { handle: string }) {
  const seat = seatLookup(useOrgIndex())(handle);
  return (
    <SeatAvatar
      name={seat.name}
      kind={seat.kind === "human" ? "human" : "agent"}
      avatar={seat.avatar}
      size="text"
      decorative
    />
  );
}

/**
 * A record's mark: a seat's face, an artifact's kind's glyph, any other kind's
 * glyph. `size` is a chip's; a reference sizes its mark to its words.
 */
export function RecordMark({
  kind,
  id,
  artifactKind,
  size,
}: {
  kind: Kind | undefined;
  id: string;
  artifactKind?: string | undefined;
  size?: GlyphSize;
}): ReactNode {
  if (kind === "seat") return <SeatFace handle={id} />;
  if (kind === "artifact" && artifactKind && isArtifactKind(artifactKind)) {
    return <ArtifactKindGlyph kind={artifactKind} size={size} />;
  }
  return kind ? KIND_GLYPH[kind](size) : <FileTextGlyph size={size} />;
}

/** Whether a seat the chart holds is a person, said before their name. */
function useSeatSaid(handle: string, isSeat: boolean): string {
  const seat = seatLookup(useOrgIndex())(handle);
  return isSeat && seat.kind === "human" ? "Person" : KIND_SAID.seat;
}

/** One record a card names, linked where its address is this dashboard's own. */
export function RecordRef({
  record,
  open,
}: {
  record: AssistantRef;
  open: (target: InAppTarget) => void;
}) {
  const seat = record.kind === "seat";
  const said = useSeatSaid(record.id, seat);
  const target = record.open ? openInApp(record.open.href) : null;
  const link = target
    ? { href: hrefOf(target.path, target.query), onFollow: () => open(target) }
    : {};
  const artifact = record.artifact_kind;
  if (record.kind === "artifact") {
    const known = artifact && isArtifactKind(artifact) ? artifact : undefined;
    return (
      <ArtifactRef
        kind={known}
        kindLabel={known ? ARTIFACT_WORDS[known] : undefined}
        title={record.name}
        {...link}
      />
    );
  }
  return (
    <InlineRef
      mark={<RecordMark kind={record.kind} id={record.id} />}
      markShape={seat ? "face" : "glyph"}
      kind={seat ? said : KIND_SAID[record.kind]}
      code={CODE.has(record.kind)}
      wrap={!CODE.has(record.kind)}
      {...link}
    >
      {record.name}
    </InlineRef>
  );
}

/** A phrase's words, each record as itself: ONE inline run, so a flex parent (a receipt) never stacks its parts. */
export function Phrase({
  phrase,
  open,
}: {
  phrase: AssistantPhrase;
  open: (target: InAppTarget) => void;
}) {
  return (
    <span>
      {phrase.map((part, i) =>
        part.ref ? (
          <RecordRef key={i} record={part.ref} open={open} />
        ) : (
          <Fragment key={i}>{part.text ?? ""}</Fragment>
        ),
      )}
    </span>
  );
}

/** Phrases one after another, `sep` between each: "QA, Design". */
export function Phrases({
  phrases,
  sep,
  open,
}: {
  phrases: readonly AssistantPhrase[];
  sep: string;
  open: (target: InAppTarget) => void;
}) {
  return (
    <>
      {phrases.map((phrase, i) => (
        <Fragment key={i}>
          {i > 0 ? sep : null}
          <Phrase phrase={phrase} open={open} />
        </Fragment>
      ))}
    </>
  );
}
