/**
 * Writing back from the pane: a reply, or the answer itself, as the person the
 * token is bound to (ADR-0024).
 *
 * TWO MODES, and the difference is whether the ask CLOSES:
 *
 *  - a REPLY (`comment_on_work_item{item, body, reply_to}`) is said in the
 *    thread and answers nothing — a person asking the asker a question back
 *    leaves the question open;
 *  - an ANSWER (`{item, body, answers}`) is what "Reply with instructions"
 *    turns this into: it closes the ask and wakes the asker with the words,
 *    and where the ask promised a channel the line beside Send says so.
 *
 * `@` MENTIONS OVER THE ORG. The engine resolves a mention from the body at
 * write, so the picker only writes `@handle` into the text; it never sends a
 * list beside it that could disagree with what the text says.
 *
 * AND THE TWO THINGS A COMMENT CANNOT CARRY: a link to another task (the item's
 * `linked` relation) and a page — an existing one, or what was written here
 * saved as a new one (`write_page`) — attached to the item (`linked_pages`).
 * Both are writes of their own, made with the same person's token, so a
 * reader sees them land in the item's links rather than in the prose.
 */

import { forwardRef, useImperativeHandle, useMemo, useRef, useState } from "react";
import { Button, FormField, IconButton, Input, Modal, Textarea } from "@crewlethq/ui";
import { FileTextGlyph, HashGlyph, PaperclipGlyph, SendGlyph } from "@crewlethq/icons/glyphs";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { matchesRow } from "~/app/keymap.ts";
import { handleLabel, type OrgIndex } from "~/lib/seats.ts";
import { firstLine } from "./NoticeList.tsx";

/** How the composer posts. */
export type ComposeMode =
  | { kind: "reply"; replyTo?: string }
  | { kind: "answer"; answers: string; inform?: { surface: string; channel: string } };

export interface ComposerHandle {
  focus: () => void;
}

/** The chat surface as its product writes its name. */
const SURFACE: Record<string, string> = { slack: "Slack", mattermost: "Mattermost" };

/** How many seats the `@` picker offers at once. */
const MENTION_CHOICES = 6;

export const Composer = forwardRef<
  ComposerHandle,
  {
    /** The task's key or id. */
    item: string;
    mode: ComposeMode;
    /** Whose words the reader is answering, for the placeholder. */
    to: string;
    index: OrgIndex;
    /** Back to a plain reply, from answer mode. */
    onReply?: () => void;
  }
>(function Composer({ item, mode, to, index, onReply }, ref) {
  const write = useAct("comment_on_work_item");
  const [text, setText] = useState("");
  const [picker, setPicker] = useState<"link" | "page" | null>(null);
  const field = useRef<HTMLTextAreaElement | null>(null);
  useImperativeHandle(ref, () => ({ focus: () => field.current?.focus() }), []);

  // THE `@` BEING TYPED, from the caret back to the sign.
  const [caret, setCaret] = useState(0);
  const [active, setActive] = useState(0);
  // ESCAPE PUTS THE PICKER AWAY until the text changes again.
  const [dismissed, setDismissed] = useState(false);
  const typing = /(?:^|\s)@([\w.-]*)$/.exec(text.slice(0, caret));
  const query = typing && !dismissed ? typing[1]!.toLowerCase() : null;
  const choices = useMemo(() => {
    if (query === null) return [];
    return index.seats
      .filter((s) => s.handle.toLowerCase().includes(query) || s.name.toLowerCase().includes(query))
      .slice(0, MENTION_CHOICES);
  }, [index.seats, query]);
  const listId = `mention-${item}`;

  const pick = (handle: string) => {
    const before = text.slice(0, caret).replace(/@[\w.-]*$/, `@${handle} `);
    const next = before + text.slice(caret);
    setText(next);
    setCaret(before.length);
    requestAnimationFrame(() => {
      field.current?.focus();
      field.current?.setSelectionRange(before.length, before.length);
    });
  };

  const send = async () => {
    const body = text.trim();
    if (!body) return;
    const args =
      mode.kind === "answer"
        ? { item, body, answers: mode.answers }
        : { item, body, ...(mode.replyTo ? { reply_to: mode.replyTo } : {}) };
    const result = await write.run(args, {
      done: mode.kind === "answer" ? `Answered ${to} on ${item}` : `Replied on ${item}`,
    });
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      setText("");
      if (mode.kind === "answer") onReply?.();
    }
  };

  const placeholder =
    mode.kind === "answer"
      ? `Tell ${to} what to do instead… @mention to bring someone in`
      : `Reply to ${to}… @mention to bring someone in`;
  const inform =
    mode.kind === "answer" && mode.inform
      ? `Also posts to ${SURFACE[mode.inform.surface] ?? mode.inform.surface} #${mode.inform.channel}`
      : "";

  return (
    <div className="inbox-composer" data-mode={mode.kind}>
      {mode.kind === "answer" && (
        <div className="inbox-composer-mode">
          <span>{`Answering ${to} — this closes the question.`}</span>
          <Button size="small" variant="ghost" onClick={onReply}>
            Reply without answering
          </Button>
        </div>
      )}
      <div className="inbox-composer-field">
        <Textarea
          ref={field}
          rows={2}
          value={text}
          aria-label={mode.kind === "answer" ? `Answer ${to}` : `Reply to ${to}`}
          placeholder={placeholder}
          role="combobox"
          aria-autocomplete="list"
          aria-expanded={choices.length > 0}
          aria-controls={choices.length > 0 ? listId : undefined}
          aria-activedescendant={choices.length > 0 ? `${listId}-${active}` : undefined}
          onChange={(event) => {
            setText(event.target.value);
            setCaret(event.target.selectionStart ?? event.target.value.length);
            setActive(0);
            setDismissed(false);
          }}
          onSelect={(event) => setCaret(event.currentTarget.selectionStart ?? 0)}
          onKeyDown={(event) => {
            if (choices.length > 0) {
              if (event.key === "ArrowDown" || event.key === "ArrowUp") {
                event.preventDefault();
                const step = event.key === "ArrowDown" ? 1 : -1;
                setActive((i) => (i + step + choices.length) % choices.length);
                return;
              }
              if (event.key === "Enter" || event.key === "Tab") {
                event.preventDefault();
                pick(choices[active]!.handle);
                return;
              }
              if (event.key === "Escape") {
                event.preventDefault();
                event.stopPropagation();
                setDismissed(true);
                return;
              }
            }
            if (matchesRow("compose.send", event.nativeEvent)) {
              event.preventDefault();
              void send();
            }
          }}
        />
        {choices.length > 0 && (
          <ul className="inbox-mentions" id={listId} role="listbox" aria-label="Mention somebody">
            {choices.map((seat, i) => (
              <li
                key={seat.handle}
                id={`${listId}-${i}`}
                role="option"
                aria-selected={i === active}
                onMouseDown={(event) => {
                  event.preventDefault();
                  pick(seat.handle);
                }}
              >
                <strong>{seat.name}</strong>
                <span className="t-caption">{handleLabel(seat.handle)}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
      <div className="inbox-composer-foot">
        <IconButton
          size="sm"
          variant="ghost"
          label="Attach a page"
          icon={<PaperclipGlyph size="sm" />}
          onClick={() => setPicker("page")}
        />
        <IconButton
          size="sm"
          variant="ghost"
          label="Link a task"
          icon={<HashGlyph size="sm" />}
          onClick={() => setPicker("link")}
        />
        <span className="spacer" />
        {inform && <span className="t-caption">{inform}</span>}
        <WriteButton
          write={write}
          size="small"
          variant="primary"
          leadingIcon={<SendGlyph size="sm" />}
          showRefusal={false}
          blocked={text.trim() ? undefined : "Write something first."}
          onPress={() => void send()}
        >
          {mode.kind === "answer" ? "Send answer" : "Send"}
        </WriteButton>
      </div>
      <RefusalNote write={write} />
      {picker === "link" && <LinkTaskDialog item={item} onClose={() => setPicker(null)} />}
      {picker === "page" && (
        <AttachPageDialog item={item} draft={text} onClose={() => setPicker(null)} />
      )}
    </div>
  );
});

/** Link another task to this one — a `linked` relation on the item. */
function LinkTaskDialog({ item, onClose }: { item: string; onClose: () => void }) {
  const write = useAct("update_work_item");
  const [q, setQ] = useState("");
  const [chosen, setChosen] = useState<{ key: string; title: string } | null>(null);
  const search = useQuery(
    "work_search",
    { q: q.trim(), mode: "hybrid", limit: 8 },
    { enabled: q.trim().length >= 2 },
  );
  const hits = (search.data?.hits ?? []).filter((h) => h.key !== item);
  const submit = async () => {
    if (!chosen) return;
    const result = await write.run(
      { item, linked: { add: [chosen.key] } },
      { done: `Linked ${chosen.key} to ${item}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  return (
    <Modal
      open
      size="sm"
      title={`Link a task to ${item}`}
      icon={<HashGlyph />}
      onClose={onClose}
      onSubmit={() => void submit()}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            blocked={chosen ? undefined : "Choose a task first."}
            onPress={() => void submit()}
          >
            Link
          </WriteButton>
        </>
      }
    >
      <div className="col gap-3">
        <FormField
          label="Find a task"
          htmlFor="inbox-link-q"
          helper="A key like ENG-42, or words from its title."
        >
          <Input id="inbox-link-q" value={q} onChange={(event) => setQ(event.target.value)} />
        </FormField>
        <PickList
          label="Tasks"
          items={hits.map((h) => ({ id: h.key, title: h.title, meta: h.key }))}
          chosen={chosen?.key ?? ""}
          onChoose={(id) => {
            const hit = hits.find((h) => h.key === id);
            if (hit) setChosen({ key: hit.key, title: hit.title });
          }}
          empty={
            q.trim().length < 2
              ? "Type two characters to search."
              : search.loading
                ? "Searching…"
                : "No task matches."
          }
        />
        <RefusalNote write={write} />
      </div>
    </Modal>
  );
}

/**
 * Attach a page to the item: one that exists, or what the reader wrote saved
 * as a new page first. The long form a comment cannot hold belongs on a page,
 * and the engine's own refusal of an oversized comment says exactly that.
 */
function AttachPageDialog({
  item,
  draft,
  onClose,
}: {
  item: string;
  draft: string;
  onClose: () => void;
}) {
  const link = useAct("update_work_item");
  const save = useAct("write_page");
  const [q, setQ] = useState("");
  const [chosen, setChosen] = useState<{ id: string; title: string } | null>(null);
  const [title, setTitle] = useState(firstLine(draft).slice(0, 120));
  const found = useQuery("pages", { title: q.trim(), limit: 8 }, { enabled: q.trim().length >= 2 });
  const busy = link.busy || save.busy;

  const attach = async (page: { id: string; title: string }) => {
    const result = await link.run(
      { item, linked_pages: { add: [page.id] } },
      { done: `Attached “${page.title}” to ${item}` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) onClose();
  };
  const saveThenAttach = async () => {
    const body = draft.trim();
    const name = title.trim();
    if (!body || !name) return;
    const saved = await save.run({ title: name, body }, { done: `Saved “${name}” as a page` });
    if (!saved || (saved.kind !== "applied" && saved.kind !== "pending")) return;
    const id = (saved.receipt as { id?: unknown } | null)?.id;
    if (typeof id === "string" && id) await attach({ id, title: name });
  };

  return (
    <Modal
      open
      size="md"
      title={`Attach a page to ${item}`}
      icon={<PaperclipGlyph />}
      onClose={onClose}
      closeDisabledReason={busy ? "Waiting for the engine to answer" : undefined}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <WriteButton
            write={link}
            variant="primary"
            showRefusal={false}
            blocked={chosen ? undefined : "Choose a page first."}
            onPress={() => chosen && void attach(chosen)}
          >
            Attach
          </WriteButton>
        </>
      }
    >
      <div className="col gap-4">
        <FormField label="An existing page" htmlFor="inbox-page-q" helper="Words from its title.">
          <Input id="inbox-page-q" value={q} onChange={(event) => setQ(event.target.value)} />
        </FormField>
        <PickList
          label="Pages"
          items={(found.data?.pages ?? []).map((p) => ({
            id: p.id,
            title: p.title,
            meta: p.container,
          }))}
          chosen={chosen?.id ?? ""}
          onChoose={(id) => {
            const page = found.data?.pages.find((p) => p.id === id);
            if (page) setChosen({ id: page.id, title: page.title });
          }}
          empty={
            q.trim().length < 2
              ? "Type two characters to search."
              : found.loading
                ? "Searching…"
                : "No page has that in its title."
          }
        />
        <RefusalNote write={link} />
        <div className="inbox-attach-save col gap-2">
          <FormField
            label="Or save what you wrote as a new page"
            htmlFor="inbox-page-title"
            helper="Filed in your team's container, then attached to the task."
          >
            <Input
              id="inbox-page-title"
              value={title}
              onChange={(event) => setTitle(event.target.value)}
            />
          </FormField>
          <WriteButton
            write={save}
            variant="secondary"
            leadingIcon={<FileTextGlyph size="sm" />}
            blocked={
              !draft.trim()
                ? "Write something in the reply first — that is the page's body."
                : !title.trim()
                  ? "Give the page a title."
                  : undefined
            }
            onPress={() => void saveThenAttach()}
          >
            Save as a page and attach
          </WriteButton>
        </div>
      </div>
    </Modal>
  );
}

/** A short list of things to choose one of, as radio buttons. */
function PickList({
  label,
  items,
  chosen,
  onChoose,
  empty,
}: {
  label: string;
  items: { id: string; title: string; meta: string }[];
  chosen: string;
  onChoose: (id: string) => void;
  empty: string;
}) {
  if (items.length === 0) return <p className="t-caption">{empty}</p>;
  return (
    <div className="inbox-picklist" role="radiogroup" aria-label={label}>
      {items.map((it) => (
        <button
          key={it.id}
          type="button"
          role="radio"
          aria-checked={it.id === chosen}
          className="inbox-pick"
          onClick={() => onChoose(it.id)}
        >
          <span className="mono t-caption">{it.meta}</span>
          <span className="truncate">{it.title}</span>
        </button>
      ))}
    </div>
  );
}
