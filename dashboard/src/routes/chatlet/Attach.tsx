/**
 * Attaching a task, a page or an artifact to a message: a search for it, and
 * a link to it written into the box.
 *
 * A MESSAGE CARRIES NO OBJECT, ONLY WORDS (`internal/chatlet`'s note on why
 * there are no attachments): what this writes is a Markdown link to the
 * record's page in this dashboard, `[GP-12 Rotate the tokens](#/worklet/GP-12)`,
 * which every reader of the message opens in place (`links: "own"`) and which
 * the room's preview read resolves into its title, status and owner as each
 * reader may see it (`chat_unfurls`). The address is the dashboard's own,
 * built from the record's id by the route table, never typed.
 *
 * THE MESSAGE'S LINKS ARE THE RECORDS ITS WORDS LINK TO ([recordLinks]): read
 * from the body when it is sent, so a link the person deleted before sending
 * is not sent, and one they wrote by hand is, and sent as whole addresses on
 * this dashboard, which is what the room's preview read takes for a record.
 *
 * Each kind is searched by the read its own screen searches with: a task by
 * the work search, a page by its title, an artifact by the gallery's listing.
 */

import { useEffect, useId, useRef, useState, type ReactNode } from "react";

import { Input, List, ListItem, Modal, FormField } from "@crewlethq/ui";
import { FileTextGlyph, LayersGlyph, SquareKanbanGlyph } from "@crewlethq/icons/glyphs";
import { artifactPath, resolve } from "~/app/routes.ts";
import { href } from "~/app/router.tsx";
import { MAX_MESSAGE_LINKS } from "~/contract/chat.ts";
import { PAGE_ADDRESS_PREFIX } from "~/contract/links.ts";
import { appRoute } from "~/lib/appRoute.ts";
import { linkTargets } from "~/lib/markdown.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useSearchQuery } from "~/lib/useSearchQuery.ts";
import { itemPath } from "~/lib/work.ts";

/** What can be attached. */
export type AttachKind = "task" | "page" | "artifact";

/** How many records a search lists: enough to recognise one, short enough to scan. */
const PICKER_HITS = 8;

/** Characters typed before a search is asked: one is every record. */
const MIN_QUERY = 2;

/** One record found, as the picker lists it and the link names it. */
export interface AttachHit {
  id: string;
  /** What the link says: the record's code and title. */
  label: string;
  /** The record's page in this dashboard, built by the route table. */
  address: string;
}

const KINDS: Record<AttachKind, { title: string; field: string; helper: string; icon: ReactNode }> =
  {
    task: {
      title: "Attach a Worklet task",
      field: "Find a task",
      helper: "A key like GP-12, or words from its title.",
      icon: <SquareKanbanGlyph size="md" />,
    },
    page: {
      title: "Attach a Doclet page",
      field: "Find a page",
      helper: "Words from its title.",
      icon: <FileTextGlyph size="md" />,
    },
    artifact: {
      title: "Attach an artifact",
      field: "Find an artifact",
      helper: "Words from its title.",
      icon: <LayersGlyph size="md" />,
    },
  };

/**
 * A Markdown link to a record, its label escaped so a bracket in a title
 * cannot end the label early and turn the rest of it into an address.
 */
export function recordLink(hit: AttachHit): string {
  const label = hit.label.replace(/[\\[\]]/g, (c) => `\\${c}`);
  return `[${label}](${hit.address})`;
}

/** The screens whose page is one record the room previews: a task, a page, an artifact. */
const RECORD_SCREENS: ReadonlySet<string> = new Set(["item", "page", "artifact"]);

/**
 * The records `body` links to, as the message's links: each a whole address
 * of its page on this dashboard, rebuilt from its path, once each, in the
 * order written, and no more than a message carries ([MAX_MESSAGE_LINKS]).
 */
export function recordLinks(body: string): string[] {
  const out = new Set<string>();
  for (const target of linkTargets(body)) {
    const page = appRoute(target);
    if (page === null) continue;
    const where = resolve(page.path);
    if (!where.resolved || !RECORD_SCREENS.has(where.screen)) continue;
    out.add(new URL(page.canonical, location.href).href);
    if (out.size === MAX_MESSAGE_LINKS) break;
  }
  return [...out];
}

export function AttachDialog({
  kind,
  onPick,
  onClose,
}: {
  kind: AttachKind;
  onPick: (hit: AttachHit) => void;
  onClose: () => void;
}) {
  const [q, setQ] = useState("");
  const words = q.trim();
  const id = useId();
  const field = useRef<HTMLInputElement | null>(null);
  useEffect(() => field.current?.focus(), []);
  const said = KINDS[kind];
  return (
    <Modal open size="sm" title={said.title} icon={said.icon} onClose={onClose}>
      <div className="col gap-3">
        <FormField label={said.field} htmlFor={id} helper={said.helper}>
          <Input id={id} ref={field} value={q} onChange={(event) => setQ(event.target.value)} />
        </FormField>
        {words.length < MIN_QUERY ? (
          <p className="t-caption muted">Type two characters to search.</p>
        ) : kind === "task" ? (
          <TaskHits q={words} onPick={onPick} />
        ) : kind === "page" ? (
          <PageHits q={words} onPick={onPick} />
        ) : (
          <ArtifactHits q={words} onPick={onPick} />
        )}
      </div>
    </Modal>
  );
}

function TaskHits({ q, onPick }: { q: string; onPick: (hit: AttachHit) => void }) {
  const search = useSearchQuery("work_search", { q, mode: "hybrid", limit: PICKER_HITS });
  // A REFUSED SEARCH LISTS NOTHING: a kept answer would be another term's hits.
  const hits = search.error ? [] : (search.data?.hits ?? []);
  return (
    <Hits
      loading={search.loading && !search.data}
      hits={hits.map((h) => ({
        id: h.id,
        label: `${h.key} ${h.title}`,
        address: href(itemPath({ id: h.id, key: h.key })),
      }))}
      none="No task matches that."
      onPick={onPick}
    />
  );
}

function PageHits({ q, onPick }: { q: string; onPick: (hit: AttachHit) => void }) {
  const found = useQuery("pages", { title: q, limit: PICKER_HITS });
  const hits = found.error ? [] : (found.data?.pages ?? []);
  return (
    <Hits
      loading={found.loading && !found.data}
      hits={hits.map((p) => ({
        id: p.id,
        label: p.title,
        address: `${PAGE_ADDRESS_PREFIX}${p.id}`,
      }))}
      none="No page has that in its title."
      onPick={onPick}
    />
  );
}

function ArtifactHits({ q, onPick }: { q: string; onPick: (hit: AttachHit) => void }) {
  const found = useQuery("artifacts", {
    scope: "all",
    sort: "updated",
    "max-keys": PICKER_HITS,
    q,
  });
  const hits = found.error ? [] : (found.data?.contents ?? []);
  return (
    <Hits
      loading={found.loading && !found.data}
      hits={hits.map((a) => ({ id: a.id, label: a.title, address: href(artifactPath(a.id)) }))}
      none="No artifact has that in its title."
      onPick={onPick}
    />
  );
}

function Hits({
  loading,
  hits,
  none,
  onPick,
}: {
  loading: boolean;
  hits: readonly AttachHit[];
  none: string;
  onPick: (hit: AttachHit) => void;
}) {
  if (loading) return <p className="t-caption muted">Searching…</p>;
  if (hits.length === 0) return <p className="t-caption muted">{none}</p>;
  return (
    <List variant="divided">
      {hits.map((hit) => (
        <ListItem key={hit.id} onClick={() => onPick(hit)}>
          <span className="truncate">{hit.label}</span>
        </ListItem>
      ))}
    </List>
  );
}
