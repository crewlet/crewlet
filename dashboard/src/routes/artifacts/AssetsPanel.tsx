/**
 * The Files panel: an artifact's asset store, the pictures and data files its
 * own page shows by name, kept beside its versions rather than in one.
 *
 * # Adding or removing a file publishes nothing
 *
 * An asset is uploaded and removed on its own (`POST` and `DELETE
 * /artifacts/{id}/assets`), so a picture changes without a new version, and a
 * page reads the store as it is now (`crewlet.assets` in the runtime). The
 * panel says so, since everywhere else a change to an artifact is a version.
 *
 * # Only where the page asks for them
 *
 * A page reaches the store only when the version it runs declares `assets`
 * (the create page's "Shows files added later"), so a file added to one that
 * does not is a file nothing shows. Adding is held with that sentence until a
 * version declaring it is published; what the store already holds stays
 * listed, and removable, either way.
 *
 * # One file, one upload, one answer
 *
 * Each file brought is its own upload and its own add, so one the engine
 * refuses (a type it does not serve, a file over its bound, a store at its
 * limit) is named with the engine's own sentence while the rest land. A name
 * the store already holds is left out and said so: the store keeps two files
 * of one name side by side, and a page asking for it by name would get either.
 */

import { useMemo, useState } from "react";
import {
  Button,
  Callout,
  FileList,
  FilePickButton,
  useToast,
  type FileListItem,
  type PickedFile,
} from "@crewlethq/ui";
import { ConfirmModal } from "~/components/Modal.tsx";
import { TrashGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import { QueryState, Section } from "~/components/common.tsx";
import { fmtBytes } from "~/lib/format.ts";
import { keyedWrite, useKeyedGesture } from "~/lib/keyedWrite.ts";
import { refusalText } from "~/lib/refusal.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { addAsset, removeAsset, uploadFiles } from "~/lib/artifacts/api.ts";
import { newActOpID, type ArtifactAsset, type ArtifactDetail } from "~/protocol/index.ts";

/** Why adding a file is held, or null when the reader may add one now. */
export function addHold(
  detail: Pick<ArtifactDetail, "capabilities" | "status">,
  access: { can: boolean; reason?: string },
): string | null {
  if (!access.can) return access.reason ?? "You cannot change this artifact.";
  if (detail.status === "trashed") return "It is in the trash. Restore it to add files.";
  if (!detail.capabilities.includes("assets")) {
    return "Its latest version does not ask for added files, so its page could not show one. Publish a version that declares it first.";
  }
  return null;
}

/** The kinds of file a person names by their type, and what each is called. */
const TYPE_WORDS: Readonly<Record<string, string>> = {
  "image/png": "PNG image",
  "image/jpeg": "JPEG image",
  "image/gif": "GIF image",
  "image/webp": "WebP image",
  "image/avif": "AVIF image",
  "image/svg+xml": "SVG image",
  "text/csv": "CSV file",
  "application/json": "JSON file",
  "text/plain": "Text file",
  "text/markdown": "Markdown file",
  "application/pdf": "PDF",
  "font/woff2": "WOFF2 font",
  "font/woff": "WOFF font",
};

/** A served type in words: "PNG image", else the file's extension ("GEOJSON file"). */
export function typeWords(contentType: string, name: string): string {
  const essence = contentType.split(";")[0]!.trim().toLowerCase();
  const known = TYPE_WORDS[essence];
  if (known) return known;
  const ext = name.includes(".") ? name.split(".").pop()!.toUpperCase() : "";
  return ext ? `${ext} file` : "File";
}

/** What one add came to: the name, and the engine's sentence where it did not land. */
interface Added {
  name: string;
  refused: string | null;
}

/**
 * Bring one file into the store: its upload (`asset: true`), then its add,
 * each under a key of its own. Answers the engine's sentence on a refusal.
 */
export async function addOne(id: string, picked: PickedFile): Promise<Added> {
  const name = picked.path;
  try {
    const up = await uploadFiles(id, [{ path: name, body: picked.file }], newActOpID(), "assets");
    const answer = await keyedWrite({ ...addAsset(id, up.upload, name), key: newActOpID() });
    return { name, refused: answer.kind === "done" ? null : answer.text };
  } catch (err) {
    return { name, refused: refusalText(err) };
  }
}

export function AssetsPanel({ detail }: { detail: ArtifactDetail }) {
  const store = useQuery("artifact_assets", { id: detail.id });
  const access = useArtifactAccess({ role: "editor", held: detail.role });
  const toast = useToast();
  const [adding, setAdding] = useState(false);
  const [refused, setRefused] = useState<Added[]>([]);
  const [skipped, setSkipped] = useState<string[]>([]);
  const [removing, setRemoving] = useState<ArtifactAsset | null>(null);
  const removal = useKeyedGesture();
  const assets = useMemo(() => store.data?.assets ?? [], [store.data]);
  const held = addHold(detail, access);
  const removeHeld = access.can ? null : (access.reason ?? "You cannot change this artifact.");

  const bring = async (picked: PickedFile[]) => {
    if (held || adding || picked.length === 0) return;
    const taken = new Set(assets.map((a) => a.name));
    const fresh = picked.filter((p) => !taken.has(p.path));
    setSkipped(picked.filter((p) => taken.has(p.path)).map((p) => p.path));
    setRefused([]);
    if (fresh.length === 0) return;
    setAdding(true);
    const answers: Added[] = [];
    // ONE AT A TIME, so a store nearing its bound refuses the file that
    // crosses it rather than every file sent beside it.
    for (const p of fresh) answers.push(await addOne(detail.id, p));
    setAdding(false);
    const landed = answers.filter((a) => a.refused === null);
    setRefused(answers.filter((a) => a.refused !== null));
    if (landed.length > 0) {
      toast.ok(landed.length === 1 ? `Added ${landed[0]!.name}` : `Added ${landed.length} files`);
    }
    store.refetch();
  };

  const remove = async (asset: ArtifactAsset) => {
    const answer = await removal.run(removeAsset(detail.id, asset.id));
    if (answer?.kind === "done") {
      setRemoving(null);
      toast.ok(`Removed ${asset.name}`);
      store.refetch();
    }
  };

  const total = assets.reduce((sum, a) => sum + (a.object?.size ?? 0), 0);
  const files: FileListItem[] = assets.map((a) => ({
    id: a.id,
    name: a.name,
    type: typeWords(a.content_type, a.name),
    size: fmtBytes(a.object?.size),
    action: (
      <Button
        size="small"
        variant="ghost"
        leadingIcon={<TrashGlyph size="sm" />}
        disabledReason={removeHeld ?? undefined}
        aria-label={`Remove ${a.name}`}
        onClick={() => {
          removal.reset();
          setRemoving(a);
        }}
      >
        Remove
      </Button>
    ),
  }));

  return (
    <div className="artifact-settings">
      <Section title="Files its page shows" hint="Kept beside its versions">
        <p className="artifact-note">
          Pictures and data files its page shows by name. Adding or removing one publishes no
          version: the page shows the store as it is now.
        </p>
        <div className="artifact-form-actions">
          <FilePickButton
            size="small"
            variant="secondary"
            multiple
            leadingIcon={<UploadGlyph size="sm" />}
            loading={adding}
            disabledReason={held ?? undefined}
            onFiles={(picked) => void bring(picked)}
          >
            Add files
          </FilePickButton>
        </div>
        {held && detail.status !== "trashed" && access.can ? (
          <Callout variant="info" title="Its page does not show added files yet">
            {held}
          </Callout>
        ) : null}
        {skipped.length > 0 ? (
          <Callout variant="warning" title="Some files were left out">
            {`${skipped.join(", ")} ${skipped.length === 1 ? "is" : "are"} already here. Remove the file first to replace it.`}
          </Callout>
        ) : null}
        {refused.length > 0 ? (
          <Callout
            variant="danger"
            title={refused.length === 1 ? "A file was not added" : "Some files were not added"}
            live="polite"
          >
            <ul className="artifact-list" role="list">
              {refused.map((r) => (
                <li key={r.name}>{`${r.name}: ${r.refused}`}</li>
              ))}
            </ul>
          </Callout>
        ) : null}
      </Section>
      <Section
        title="In the store"
        hint={
          assets.length > 0
            ? `${assets.length} ${assets.length === 1 ? "file" : "files"}, ${fmtBytes(total)}`
            : undefined
        }
      >
        <QueryState
          error={store.error}
          refusal={store.refusal}
          detail={store.detail}
          loading={store.loading && !store.data}
          {...(assets.length === 0
            ? {
                empty: {
                  title: "No files yet",
                  hint: "Files added here are the ones its page can list and show.",
                  headingLevel: "none" as const,
                },
              }
            : {})}
        >
          <FileList label="Files its page shows" files={files} />
        </QueryState>
      </Section>
      {removing ? (
        <ConfirmModal
          open
          destructive
          title={`Remove ${removing.name}?`}
          message={
            removal.answer && removal.answer.kind !== "done"
              ? removal.answer.text
              : "Its page stops showing it at once. No version changes."
          }
          cancelLabel="Keep it"
          confirmLabel="Remove"
          onClose={() => setRemoving(null)}
          onConfirm={() => remove(removing)}
          submitting={removal.busy}
        />
      ) : null}
    </div>
  );
}
