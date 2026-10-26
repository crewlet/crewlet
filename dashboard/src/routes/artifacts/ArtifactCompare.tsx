/**
 * Two versions of an artifact, compared: the History panel's answer to "what
 * changed when the agent republished it".
 *
 * TWO LENSES, addressed in the URL (`against=` names the other version and
 * `lens=` the way they are compared), so a pasted link opens the same
 * comparison:
 *
 * - SOURCE: every file added, removed or changed between the two, by digest,
 *   and for a changed text file the lines that changed, both drawn by the
 *   dashboard's one [DiffView]: the files as a diff KEYED by path, a changed
 *   file being its old facts removed and its new ones added (the digest
 *   among them, since a file can change and keep its type and size), and the
 *   lines numbered in both versions.
 * - SIDE BY SIDE: both versions drawn, a page in two frames or a document
 *   rendered twice. Data is live and not versioned with the page, so both
 *   show today's data, and the pane says so. A kind compared ITEM BY ITEM
 *   draws its own comparison in this lens instead (`KIND_ITEM_COMPARES`): a
 *   deck By slide, each slide's two versions side by side, paired by id, with
 *   added, removed and moved slides said in words.
 *
 * The older version is always the left side and the "from" half, whichever of
 * the two the reader picked first, so a comparison reads forward in time.
 *
 * The frame, the heading, the lens switch and the two panes are uilet's
 * [VersionCompare]; what stays here is what only an artifact has: its
 * manifests compared by digest, the per-file fetch and the two renderings.
 */

import { useMemo, useState, type ReactNode } from "react";
import { Callout, Select, Skeleton, VersionCompare } from "@crewlethq/ui";
import type { ArtifactDetail, ArtifactEntry } from "~/protocol/index.ts";
import { QueryState } from "~/components/common.tsx";
import { DiffView, type DiffHunk } from "~/components/DiffView.tsx";
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
import { SlidesCompare } from "./SlidesCompare.tsx";
import { KIND_VIEWS } from "./ViewTab.tsx";

/** How two versions are compared. */
export type CompareLens = "source" | "visual";

/** The lenses, as the URL spells them. */
export const COMPARE_LENSES: readonly CompareLens[] = ["source", "visual"];

/** Whether a kind of artifact can be drawn side by side: one whose versions hold what it draws, never a sheet. */
export function drawnSideBySide(kind: ArtifactDetail["kind"]): boolean {
  return !contentIsData(kind);
}

/**
 * The kinds compared item by item rather than as two whole drawings: the
 * lens's name and the comparison drawn in it.
 */
const KIND_ITEM_COMPARES: Partial<
  Record<
    ArtifactDetail["kind"],
    { label: string; draw: (detail: ArtifactDetail, older: number, newer: number) => ReactNode }
  >
> = {
  slides: {
    label: "By slide",
    draw: (detail, older, newer) => <SlidesCompare detail={detail} older={older} newer={newer} />,
  },
};

/** The most changed runs of lines one file shows before it says how many more there are. */
const SHOWN_HUNKS = 200;

export function ArtifactCompare({
  detail,
  picked,
  against,
  lens,
  onLens,
  room,
  presence,
  bridgeStores,
}: {
  detail: ArtifactDetail;
  picked: number;
  against: number;
  lens: CompareLens;
  onLens: (lens: CompareLens) => void;
  room: ArtifactRoom;
  presence: PresenceReport;
  bridgeStores: Set<ArtifactDataStore>;
}) {
  const older = Math.min(picked, against);
  const newer = Math.max(picked, against);
  const visual = drawnSideBySide(detail.kind);
  const items = KIND_ITEM_COMPARES[detail.kind];
  const side = (n: number) =>
    KIND_VIEWS[detail.kind]({
      detail,
      version: n,
      store: null,
      room,
      presence,
      drawn: [],
      bridgeStores,
      // THE NOTE ON TODAY'S DATA IS SAID ONCE for both sides, under the comparison.
      dataNote: false,
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
      labels={{ changes: "Files", versions: items?.label ?? "Side by side" }}
      changes={<SourceCompare detail={detail} older={older} newer={newer} />}
      versions={visual && items ? items.draw(detail, older, newer) : undefined}
      note={
        dataBesideVersions(detail.kind)
          ? "Both versions show today's data: an artifact's data is live and is not versioned with it."
          : undefined
      }
      from={visual && !items ? side(older) : undefined}
      to={visual && !items ? side(newer) : undefined}
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
  // ONE ANSWER, ONE SET OF HUNKS: a diff keyed by new hunks starts its
  // window and its measured heights over, so they are built with the answer.
  const fileDiff = useMemo(() => fileHunks(files?.changes ?? []), [files]);
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
          <DiffView
            hunks={fileDiff}
            aria-label={`Files changed from version ${older} to version ${newer}`}
            empty={
              <p className="artifact-note">
                The two versions hold the same {plural(files.unchanged, "file")}.
              </p>
            }
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

/**
 * How much of a file's digest its facts show: enough that the two digests of
 * one path, read one above the other, differ where they differ — two SHA-256s
 * of different bytes agree in their first twelve hex digits once in 2^48 —
 * and short enough to read as a fact beside a type and a size.
 */
const DIGEST_SHOWN = 12;

/** What a file is, in a version: its type, its size and the digest it is compared by. */
function facts(e: ArtifactEntry): string {
  return [
    e.content_type.split(";")[0],
    fmtBytes(e.object.size),
    e.object.hash.slice(0, DIGEST_SHOWN),
  ].join(" · ");
}

/**
 * The files of two versions as a diff KEYED by path, one hunk a file: an added
 * file its new facts, a removed one its old, and a changed one both, so the
 * underline says which of its facts moved.
 */
function fileHunks(changes: readonly FileChange[]): DiffHunk[] {
  const line = (kind: "add" | "remove", path: string, e: ArtifactEntry) => ({
    kind,
    text: facts(e),
    before: null,
    after: null,
    path,
  });
  return changes.map((c) => ({
    skipped: 0,
    lines:
      c.kind === "added"
        ? [line("add", c.path, c.to)]
        : c.kind === "removed"
          ? [line("remove", c.path, c.from)]
          : [line("remove", c.path, c.from), line("add", c.path, c.to)],
  }));
}

/**
 * A text file's changed runs as a diff numbered in both versions: each run its
 * hunk, its older lines removed and its newer ones added, opening on how many
 * unchanged lines lay between it and the run before.
 */
export function lineHunks(hunks: readonly Hunk[]): DiffHunk[] {
  // The older version's lines accounted for by the runs so far.
  let seen = 0;
  return hunks.map((h) => {
    const skipped = h.fromStart - 1 - seen;
    seen = h.fromStart - 1 + h.from.length;
    return {
      skipped,
      lines: [
        ...h.from.map((text, i) => ({
          kind: "remove" as const,
          text,
          before: h.fromStart + i,
          after: null,
        })),
        ...h.to.map((text, i) => ({
          kind: "add" as const,
          text,
          before: null,
          after: h.toStart + i,
        })),
      ],
    };
  });
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
  // Built with the answer, for the reason `fileDiff` is.
  const lineDiff = useMemo(
    () => (answer.data?.kind === "hunks" ? lineHunks(answer.data.hunks.slice(0, SHOWN_HUNKS)) : []),
    [answer.data],
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
          <DiffView
            hunks={lineDiff}
            aria-label={`${to.path}: the lines that changed`}
            empty={
              <p className="artifact-note">
                The text is the same; only its bytes moved (a line ending or an encoding).
              </p>
            }
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
