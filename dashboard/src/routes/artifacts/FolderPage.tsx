/**
 * One folder (`#/artifacts/folders/<id>`): its header, the folders in it and
 * the artifacts directly in it, under the trail Artifacts › Folders › every
 * folder above it.
 *
 * # Addressed by id
 *
 * The address names the folder by the id the engine minted, so a rename or a
 * move breaks no link, and the trail is drawn from the reader's own tree:
 * every folder above one they can see is one they can see too, so the trail
 * never stops short.
 *
 * # Not there and not theirs are one answer
 *
 * A folder the reader cannot see is answered exactly as one that does not
 * exist, "Folder not found", so a pasted link never tells anybody a folder is
 * there.
 */

import { ArtifactAccessLabel, ButtonLink, EmptyState, Skeleton } from "@crewlethq/ui";
import { FolderOpenGlyph } from "@crewlethq/icons/glyphs";
import { usePageLabels } from "~/app/Shell.tsx";
import { folderUpKey } from "~/app/crumbs.ts";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { href, useNavigator } from "~/app/router.tsx";
import { artifactFoldersPath } from "~/app/routes.ts";
import { QueryState } from "~/components/common.tsx";
import { useFolderTree } from "~/lib/artifacts/folders.ts";
import { FolderView } from "./FolderView.tsx";
import { FolderMenu, aboveRemoved, useVisibilityWords } from "./FolderDialogs.tsx";

export function ArtifactFolderPage({ id }: { id: string }) {
  const tree = useFolderTree();
  const nav = useNavigator();
  const words = useVisibilityWords();
  const folder = tree.index.get(id);
  const above = tree.index.trail(id).slice(0, -1);

  // THE TRAIL: every folder above this one, outermost first, each a way back.
  usePageLabels(
    folder
      ? {
          [id]: folder.name,
          ...Object.fromEntries(
            above.flatMap((f, i) => [
              [folderUpKey(id, i), f.id],
              [f.id, f.name],
            ]),
          ),
        }
      : {},
  );

  if (!tree.data) {
    return tree.loading ? (
      <Skeleton variant="list" rows={4} label="Loading the folder" />
    ) : (
      <QueryState error={tree.error} refusal={tree.refusal} loading={false} />
    );
  }
  if (!folder) {
    return (
      <EmptyState
        icon={<FolderOpenGlyph size="xl" />}
        title="Folder not found"
        description="It was removed, or it is not shared with you. A link to a folder opens only for somebody who can see it."
        action={
          <ButtonLink size="small" variant="secondary" href={href(artifactFoldersPath())}>
            Back to folders
          </ButtonLink>
        }
      />
    );
  }
  const said = words(folder);
  return (
    <FolderView
      key={id}
      folder={folder}
      index={tree.index}
      topLevel={tree.data.top_level}
      header={
        <ObjectHeader
          kind="Folder"
          icon="folder-open"
          title={folder.name}
          status={<ArtifactAccessLabel access={said.access} label={said.long} />}
          actions={
            <FolderMenu
              folder={folder}
              index={tree.index}
              onRemoved={() => nav.replace(aboveRemoved(folder))}
            />
          }
        />
      }
    />
  );
}
