/**
 * Artifacts › Folders (`#/artifacts/folders`): the company's top-level folders
 * the reader can see, then "Not in a folder", everything they can open that is
 * filed in no folder they can see.
 *
 * A FOURTH QUESTION BESIDE THE THREE FLAT ONES. All, Yours and Shared with
 * you ask who made something and who it is shared with; Folders asks where it
 * is kept. Every reader browses them, since a folder is a place and never a
 * permission; making a top-level folder is a person's, who says there who
 * sees it (`FolderDialogs.tsx`).
 *
 * The drawing is the folder view every folder's page shares
 * (`FolderView.tsx`); what is particular to the top level is that it has no
 * header and nothing above it.
 */

import { Skeleton } from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { useFolderTree } from "~/lib/artifacts/folders.ts";
import { FolderView } from "./FolderView.tsx";

export function ArtifactFolders() {
  const tree = useFolderTree();
  // THE TREE FIRST: the tiles, the counts and the empty state are all its, so
  // "No folders yet" is never drawn for the moment before it arrives.
  if (!tree.data) {
    return tree.loading ? (
      <Skeleton variant="list" rows={4} label="Loading the folders" />
    ) : (
      <QueryState error={tree.error} refusal={tree.refusal} loading={false} />
    );
  }
  return (
    <>
      {tree.error && <QueryState error={tree.error} refusal={tree.refusal} loading={false} />}
      <FolderView folder={null} index={tree.index} topLevel={tree.data.top_level} />
    </>
  );
}
