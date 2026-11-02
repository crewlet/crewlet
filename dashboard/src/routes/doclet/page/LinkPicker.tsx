/**
 * "Link to a page", for every place a page's Markdown is written: the editor
 * and the composer. An entry of the rich editor's slash menu, whose panel is
 * the picker below.
 *
 * # The link is the address the backlinks read
 *
 * It inserts a link to `#/doclet/pages/{id}`: `PAGE_ADDRESS_PREFIX` plus
 * the id, which is exactly what `doclet.Links` reads back, so a link made here
 * lists this page in the other one's "Linked from" once the index has read the
 * save. A title would break on the first rename; the id does not.
 */

import { useEffect, useId, useMemo, useRef, useState } from "react";
import { FormField, Input, KeyChip, List, ListItem, type RichEditorCommand } from "@crewlethq/ui";
import { FileTextGlyph } from "@crewlethq/icons/glyphs";
import { PAGE_ADDRESS_PREFIX } from "~/contract/links.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { PageGlyph } from "../parts.tsx";

/** How many titles the picker lists: enough to recognise one, short enough to scan. */
const PICKER_HITS = 8;

/**
 * The slash menu's "Link to a page". `exclude` is the page being edited,
 * which a link to itself would not help.
 */
export function linkPageCommand(exclude?: string): RichEditorCommand {
  return {
    key: "link-page",
    label: "Link to a page",
    description: "A page of this company, by its title",
    icon: <FileTextGlyph size="sm" />,
    keywords: ["page", "link", "wiki", "knowledge"],
    panel: (editor) => (
      <PagePicker
        exclude={exclude}
        onPick={(page) => editor.insertLink(page.title, `${PAGE_ADDRESS_PREFIX}${page.id}`)}
      />
    ),
  };
}

/**
 * Choosing a page to link to: words from its title, and the first hits. The
 * page being edited is left out: a page linking to itself is not a link
 * anybody follows, and the backlinks do not count it.
 */
function PagePicker({
  exclude,
  onPick,
}: {
  exclude?: string | undefined;
  onPick: (page: { id: string; title: string }) => void;
}) {
  const [q, setQ] = useState("");
  const inputID = useId();
  const words = q.trim();
  const found = useQuery(
    "pages",
    { title: words, limit: PICKER_HITS },
    { enabled: words.length >= 2 },
  );
  const hits = useMemo(
    () => (found.data?.pages ?? []).filter((p) => p.id !== exclude),
    [found.data, exclude],
  );
  const field = useRef<HTMLInputElement>(null);
  useEffect(() => field.current?.focus(), []);
  return (
    <div className="knowledge-picker">
      <FormField label="Link to the page" htmlFor={inputID} helper="Words from its title.">
        <Input id={inputID} ref={field} value={q} onChange={(event) => setQ(event.target.value)} />
      </FormField>
      {words.length < 2 ? (
        <p className="knowledge-note">Type two characters to search.</p>
      ) : found.loading && !found.data ? (
        <p className="knowledge-note">Searching&hellip;</p>
      ) : hits.length === 0 ? (
        <p className="knowledge-note">No page has that in its title.</p>
      ) : (
        <List variant="divided">
          {hits.map((p) => (
            <ListItem
              key={p.id}
              leading={<PageGlyph folder={p.folder} />}
              trailing={<KeyChip>{p.container}</KeyChip>}
              onClick={() => onPick(p)}
            >
              <span className="truncate">{p.title}</span>
            </ListItem>
          ))}
        </List>
      )}
    </div>
  );
}
