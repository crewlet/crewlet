/**
 * Writing a new page, as the signed-in person (`write_page`).
 *
 * FILED WHERE IT WAS ASKED FOR: a space's own page files it at the space's
 * top, a page's "New sub-page" under that page, and the sheet says which
 * rather than offering a container picker — the reader chose the place by
 * where they pressed. A title is the page's address inside its space and is
 * unique there, so the engine's refusal of a taken one (`exists`) is drawn
 * beside the field it is about.
 *
 * On success the reader is taken to the page they wrote, by the id the engine
 * answered with — never by the title, which is exactly what a rename moves.
 */

import { useId, useState } from "react";
import { Button, FormField, Input, Modal, Textarea } from "@crewlethq/ui";
import { FileTextGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { useAct } from "~/lib/useAct.ts";

export function NewPageDialog({
  container,
  parent,
  onClose,
}: {
  container: string;
  /** The page it is filed under, or none for the space's top. */
  parent?: { id: string; title: string };
  onClose: () => void;
}) {
  const write = useAct("write_page");
  const nav = useNavigator();
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const titleID = useId();
  const bodyID = useId();
  const name = title.trim();
  const blocked = !name
    ? "Give the page a title."
    : !body.trim()
      ? "Write the page first."
      : undefined;

  const create = async () => {
    const result = await write.run(
      { title: name, body, container, ...(parent ? { parent: parent.id } : {}) },
      { done: `Wrote “${name}”` },
    );
    if (!result || (result.kind !== "applied" && result.kind !== "pending")) return;
    const id = (result.receipt as { id?: unknown } | null)?.id;
    onClose();
    if (typeof id === "string" && id) nav.to(["knowledge", "pages", id]);
  };

  return (
    <Modal
      open
      variant="sheet"
      stackBody
      size="lg"
      title={parent ? `New page under “${parent.title}”` : `New page in ${container}`}
      icon={<FileTextGlyph />}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason={write.busy ? "Waiting for the engine to answer" : undefined}
      footerStart={
        <span className="t-caption">
          {blocked ??
            `Written as you, in ${container}${parent ? `, under “${parent.title}”` : ""}.`}
        </span>
      }
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant="primary"
            showRefusal={false}
            blocked={blocked}
            onPress={() => void create()}
          >
            Write the page
          </WriteButton>
        </>
      }
    >
      <FormField
        label="Title"
        htmlFor={titleID}
        helper="How people will refer to it — unique in its space."
      >
        <Input
          id={titleID}
          value={title}
          autoFocus
          maxLength={200}
          onChange={(event) => setTitle(event.target.value)}
        />
      </FormField>
      <FormField label="The page" htmlFor={bodyID} helper="Markdown.">
        <Textarea
          id={bodyID}
          value={body}
          rows={14}
          onChange={(event) => setBody(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
              event.preventDefault();
              if (pressable(write, blocked)) void create();
            }
          }}
        />
      </FormField>
      <RefusalNote write={write} />
    </Modal>
  );
}
