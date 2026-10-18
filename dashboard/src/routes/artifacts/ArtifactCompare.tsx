/**
 * Two versions of an artifact, compared: the History tab's answer to "what
 * changed when the agent republished it".
 *
 * TWO LENSES, addressed in the URL (`against=` names the other version and
 * `lens=` the way they are compared), so a pasted link opens the same
 * comparison:
 *
 * - SOURCE: every file added, removed or changed between the two, by digest,
 *   and for a changed text file the lines that changed, as the design
 *   system's `DiffList` draws a difference (a glyph and a word on every row,
 *   never colour alone).
 * - SIDE BY SIDE: both versions drawn, a page in two frames or a document
 *   rendered twice. Data is live and not versioned with the page, so both
 *   show today's data, and the pane says so.
 *
 * The older version is always the left side and the "from" half, whichever of
 * the two the reader picked first, so a comparison reads forward in time.
 *
 * The frame, the heading, the lens switch and the two panes are uilet's
 * [VersionCompare]; what stays here is what only an artifact has: its
 * manifests compared by digest, the per-file fetch and the two renderings.
 */

import { useMemo, useState } from "react";
import { Callout, DiffList, Select, Skeleton, VersionCompare, type DiffRow } from "@crewlethq/ui";
import type { ArtifactDetail, ArtifactEntry, ArtifactVersionSummary } from "~/protocol/index.ts";
import { QueryState } from "~/components/common.tsx";
import { fmtBytes, plural } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRest } from "~/lib/useRest.ts";
import { versionFile } from "~/lib/artifacts/api.ts";
import {
  MAX_COMPARE_BYTES,
  compareFiles,
  compareLines,
  isComparableText,
  type FileChange,
  type Hunk,
  type LineComparison,
} from "~/lib/artifacts/compare.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { contentIsData, dataBesideVersions } from "~/lib/artifacts/kinds.ts";
import type { PresenceReport } from "./presence.ts";
import { KIND_VIEWS } from "./ViewTab.tsx";

/** How two versions are compared. */
export type CompareLens = "source" | "visual";

/** The lenses, as the URL spells them. */
export const COMPARE_LENSES: readonly CompareLens[] = ["source", "visual"];

/** Whether a kind of artifact can be drawn side by side: one whose versions hold what it draws, never a sheet. */
export function drawnSideBySide(kind: ArtifactDetail["kind"]): boolean {
  return !contentIsData(kind);
}

/** The most changed runs of lines one file shows before it says how many more there are. */
const SHOWN_HUNKS = 200;

export function ArtifactCompare({
  detail,
  picked,
  against,
  lens,
  onLens,
  versions,
  room,
  presence,
  bridgeStores,
}: {
  detail: ArtifactDetail;
  picked: number;
  against: number;
  lens: CompareLens;
  onLens: (lens: CompareLens) => void;
  versions: ArtifactVersionSummary[];
  room: ArtifactRoom;
  presence: PresenceReport;
  bridgeStores: Set<ArtifactDataStore>;
}) {
  const older = Math.min(picked, against);
  const newer = Math.max(picked, against);
  const visual = drawnSideBySide(detail.kind);
  const side = (n: number) =>
    KIND_VIEWS[detail.kind]({
      detail,
      version: n,
      versions,
      store: null,
      room,
      presence,
      drawn: [],
      bridgeStores,
    });
  // THE PANES ARE HANDED OVER ONLY FOR A KIND DRAWN SIDE BY SIDE, and uilet
  // mounts them only while that lens is on show, so no frame runs behind the
  // Files lens. Without `onLensChange` a sheet gets no switch and shows Files.
  return (
    <VersionCompare
      fromLabel={`Version ${older}`}
      toLabel={`Version ${newer}`}
      title={`Version ${older} and version ${newer}`}
      description="What changed between them"
      lens={visual && lens === "visual" ? "versions" : "changes"}
      onLensChange={
        visual ? (next) => onLens(next === "versions" ? "visual" : "source") : undefined
      }
      labels={{ changes: "Files", versions: "Side by side" }}
      changes={<SourceCompare detail={detail} older={older} newer={newer} />}
      note={
        dataBesideVersions(detail.kind)
          ? "Both versions show today's data: a page's data is live and is not versioned with it."
          : undefined
      }
      from={visual ? side(older) : undefined}
      to={visual ? side(newer) : undefined}
    />
  );
}

/** The files of two versions, compared, and the lines of the one text file picked. */
function SourceCompare({
  detail,
  older,
  newer,
}: {
  detail: ArtifactDetail;
  older: number;
  newer: number;
}) {
  const from = useQuery("artifact_version", { id: detail.id, n: older });
  const to = useQuery("artifact_version", { id: detail.id, n: newer });
  const files = useMemo(
    () => (from.data && to.data ? compareFiles(from.data.manifest, to.data.manifest) : null),
    [from.data, to.data],
  );
  const texts = useMemo(
    () =>
      (files?.changes ?? []).filter(
        (c): c is Extract<FileChange, { kind: "changed" }> =>
          c.kind === "changed" &&
          isComparableText(c.from.content_type) &&
          isComparableText(c.to.content_type),
      ),
    [files],
  );
  const entry = to.data?.manifest.entry ?? "";
  const [chosen, setChosen] = useState<string | null>(null);
  const picked =
    texts.find((c) => c.path === chosen) ?? texts.find((c) => c.path === entry) ?? texts[0];

  return (
    <QueryState
      error={from.error ?? to.error}
      refusal={from.refusal ?? to.refusal}
      loading={from.loading || to.loading}
    >
      {files && (
        <div className="artifact-compare">
          <DiffList
            rows={files.changes.map(fileRow)}
            emptyMessage={`The two versions hold the same ${plural(files.unchanged, "file")}.`}
          />
          {files.changes.length > 0 && files.unchanged > 0 && (
            <p className="artifact-note">
              {plural(files.unchanged, "other file")} {files.unchanged === 1 ? "is" : "are"} the
              same in both.
            </p>
          )}
          {picked && (
            <>
              {texts.length > 1 && (
                <Select
                  width="auto"
                  value={picked.path}
                  onChange={(value) => setChosen(String(value))}
                  options={texts.map((c) => ({ value: c.path, label: c.path }))}
                  ariaLabel="File to compare line by line"
                />
              )}
              <LineCompare
                detail={detail}
                older={older}
                newer={newer}
                from={picked.from}
                to={picked.to}
              />
            </>
          )}
        </div>
      )}
    </QueryState>
  );
}

/** One file's change as a row of the difference. */
function fileRow(c: FileChange): DiffRow {
  const facts = (e: ArtifactEntry) => `${e.content_type.split(";")[0]}, ${fmtBytes(e.object.size)}`;
  const path = <span className="mono">{c.path}</span>;
  switch (c.kind) {
    case "added":
      return { kind: "added", path, to: facts(c.to) };
    case "removed":
      return { kind: "removed", path, from: facts(c.from) };
    case "changed":
      return { kind: "changed", path, from: facts(c.from), to: facts(c.to) };
  }
}

/** The lines that changed in one text file between the two versions. */
function LineCompare({
  detail,
  older,
  newer,
  from,
  to,
}: {
  detail: ArtifactDetail;
  older: number;
  newer: number;
  from: ArtifactEntry;
  to: ArtifactEntry;
}) {
  const tooBig = from.object.size > MAX_COMPARE_BYTES || to.object.size > MAX_COMPARE_BYTES;
  const answer = useRest<LineComparison>(
    tooBig
      ? null
      : `${detail.id}:${older}:${newer}:${from.path}:${from.object.hash}:${to.object.hash}`,
    async (signal) => {
      const [a, b] = await Promise.all([
        versionFile(detail.id, older, from.path, signal),
        versionFile(detail.id, newer, to.path, signal),
      ]);
      return compareLines(await a.text(), await b.text());
    },
  );
  if (tooBig) {
    return (
      <p className="artifact-note">
        {to.path} is too large to compare line by line here (over {fmtBytes(MAX_COMPARE_BYTES)});
        download both versions from the files list.
      </p>
    );
  }
  if (!answer.data) {
    return answer.loading ? (
      <Skeleton variant="text" rows={4} label={`Comparing ${to.path}`} />
    ) : (
      <QueryState error={answer.code} refusal={answer.refusal} loading={false} />
    );
  }
  const result = answer.data;
  switch (result.kind) {
    case "too-large":
      return (
        <p className="artifact-note">
          {to.path} has {result.lines.toLocaleString()} lines, too many to compare line by line
          here.
        </p>
      );
    case "too-different":
      return (
        <Callout variant="info" title={`${to.path} was rewritten`}>
          The two versions of this file have almost nothing in common, so a line by line comparison
          would list every line. Open each version to read it.
        </Callout>
      );
    case "hunks":
      return (
        <div className="artifact-compare-lines">
          <h3 className="artifact-compare-heading">
            {to.path}: {plural(result.hunks.length, "change")}
          </h3>
          <DiffList
            rows={result.hunks.slice(0, SHOWN_HUNKS).map(hunkRow)}
            emptyMessage="The text is the same; only its bytes moved (a line ending or an encoding)."
          />
          {result.hunks.length > SHOWN_HUNKS && (
            <p className="artifact-note">
              {plural(result.hunks.length - SHOWN_HUNKS, "more change")} not shown.
            </p>
          )}
        </div>
      );
  }
}

/** One run of changed lines as a row of the difference. */
function hunkRow(h: Hunk): DiffRow {
  const block = (text: string[]) => <pre className="artifact-compare-text">{text.join("\n")}</pre>;
  if (h.from.length === 0) {
    return { kind: "added", path: lineSpan("Line", h.toStart, h.to.length), to: block(h.to) };
  }
  if (h.to.length === 0) {
    return {
      kind: "removed",
      path: lineSpan("Line", h.fromStart, h.from.length),
      from: block(h.from),
    };
  }
  return {
    kind: "changed",
    path: lineSpan("Line", h.toStart, h.to.length),
    from: block(h.from),
    to: block(h.to),
  };
}

/** A run of lines, as a reader names it: "Line 4", or "Lines 4 to 9". */
function lineSpan(word: string, start: number, count: number): string {
  return count <= 1 ? `${word} ${start}` : `${word}s ${start} to ${start + count - 1}`;
}
