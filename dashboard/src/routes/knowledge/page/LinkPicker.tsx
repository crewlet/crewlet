/**
 * "Link a page", for every place a page's markdown is written: the editor and
 * the composer.
 *
 * # The link is the address the backlinks read
 *
 * It inserts `[title](#/knowledge/pages/{id})`: `PAGE_ADDRESS_PREFIX` plus the
 * id, which is exactly what `pages.Links` reads back, so a link made here lists
 * this page in the other one's "Linked from" once the index has read the save.
 * A title would break on the first rename; the id does not.
 */

import { useEffect, useId, useMemo, useRef, useState } from "react";
import {
  Button,
  FormField,
  Input,
  KeyChip,
  List,
  ListItem,
  Popover,
  type MarkdownEditorApi,
} from "@crewlethq/ui";
import { LinkGlyph } from "@crewlethq/icons/glyphs";
import { PAGE_ADDRESS_PREFIX } from "~/contract/links.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { PageGlyph } from "../parts.tsx";

/** How many titles the picker lists: enough to recognise one, short enough to scan. */
const PICKER_HITS = 8;

/** The markdown link to a page, by its id. */
export function pageLink(title: string, id: string): string {
  // A `]` in a title would close the label early; escaped, it renders as itself.
  return `[${title.replace(/([[\]\\])/g, "\\$1")}](${PAGE_ADDRESS_PREFIX}${id})`;
}

/**
 * The button in an editor's bar that opens the picker, and inserts the chosen
 * page's link at the caret. In Preview there is no caret, so it says so.
 */
export function LinkPageButton({
  editor,
  exclude,
}: {
  editor: MarkdownEditorApi;
  /** The page being edited, which a link to itself would not help. */
  exclude?: string | undefined;
}) {
  return (
    <Popover
      label="Link a page"
      align="end"
      trigger={(open, toggle) => (
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<LinkGlyph size="sm" />}
          aria-expanded={open}
          disabledReason={editor.mode === "write" ? undefined : "Switch to Write to link a page."}
          onClick={toggle}
        >
          Link a page
        </Button>
      )}
    >
      {(close) => (
        <PagePicker
          exclude={exclude}
          onPick={(page) => {
            close();
            editor.insert(pageLink(page.title, page.id));
          }}
        />
      )}
    </Popover>
  );
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
