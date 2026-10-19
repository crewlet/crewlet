/**
 * New artifact: a native sheet, blank or from the Review sheet template, or a
 * doc or a slide deck from the engine's starter, made by the person signed in
 * and owned by them. `kind=doc` and `kind=slides` open on those.
 *
 * A PAGE IS PUBLISHED, NOT TYPED HERE: an agent's tool or the CLI publishes
 * one. A sheet's content is its datastore, so it is two engine writes: the
 * artifact, then its schema. Each is keyed, so a retry is the same operation;
 * a schema the engine refused after the sheet was made leaves the sheet in
 * place, and its page says so and offers the schema editor. A doc or a deck is
 * one write: the engine publishes its own starter, held to the kind's format,
 * and the person edits it in place from there.
 *
 * `item=` names the task the sheet is made for, which lists it on that task's
 * Artifacts card.
 */

import { useState } from "react";
import { Button, ChoiceList, FormField, Input, SegmentedControl, useToast } from "@crewlethq/ui";
import { useNavigator, useParam } from "~/app/router.tsx";
import { Section } from "~/components/common.tsx";
import { utf8Bytes } from "~/lib/format.ts";
import { keyedWrite, useKeyedGesture } from "~/lib/keyedWrite.ts";
import { newActOpID } from "~/protocol/index.ts";
import { ARTIFACT_TITLE_MAX_BYTES } from "~/contract/artifacts.ts";
import {
  artifactPath,
  configureSchema,
  createFromStarter,
  createSheet,
} from "~/lib/artifacts/api.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { SHEET_TEMPLATES, type SheetTemplate } from "~/lib/artifacts/schema.ts";

/** What New can start, in the order its kind control offers them, and what each says. */
const STARTS = [
  {
    kind: "sheet",
    label: "Sheet",
    title: "New sheet",
    hint: "A sheet everyone you share it with fills in together, live",
    placeholder: "Integration requirements review",
    starts: "",
    action: "Start the sheet",
  },
  {
    kind: "doc",
    label: "Doc",
    title: "New doc",
    hint: "A document you write in Markdown, edit in place and print",
    placeholder: "On-call handbook",
    starts: "It starts as one page under its title. Add, rename and reorder its tabs from Edit.",
    action: "Start the doc",
  },
  {
    kind: "slides",
    label: "Deck",
    title: "New deck",
    hint: "A slide deck you edit in place, present full screen and print",
    placeholder: "Quarterly plan",
    starts:
      "It starts from four example slides (a cover, an agenda, two columns and a closing slide), each with notes on what to put there.",
    action: "Start the deck",
  },
] as const;

/** A work item's key, as the engine names one: a project key, a dash, a number. */
const ITEM_KEY = /^[A-Z][A-Z0-9_]*-\d+$/;

export function NewArtifact() {
  const nav = useNavigator();
  const toast = useToast();
  const [item] = useParam("item", "");
  const [templateParam] = useParam("template", "review");
  const [kindParam, setKind] = useParam("kind", "sheet");
  const kind = STARTS.find((s) => s.kind === kindParam) ?? STARTS[0];
  const [template, setTemplate] = useState<SheetTemplate>(
    templateParam === "blank" ? "blank" : "review",
  );
  const [title, setTitle] = useState("");
  const create = useKeyedGesture();
  const access = useArtifactAccess({ app: true });
  const workItem = ITEM_KEY.test(item) ? item : "";
  const bytes = utf8Bytes(title.trim());
  const titleWrong =
    title.trim() === ""
      ? undefined
      : bytes > ARTIFACT_TITLE_MAX_BYTES
        ? `The title is ${bytes} bytes and the most is ${ARTIFACT_TITLE_MAX_BYTES}.`
        : undefined;

  const startFromStarter = async (starter: "doc" | "slides") => {
    const answer = await create.run(
      createFromStarter({ kind: starter, title: title.trim(), ...(workItem ? { workItem } : {}) }),
    );
    if (answer?.kind !== "done") return;
    const id = typeof answer.body.artifact === "string" ? answer.body.artifact : "";
    if (!id) return;
    toast.ok(`Started “${title.trim()}”`);
    nav.to(artifactPath(id));
  };

  const start = async () => {
    const answer = await create.run(
      createSheet({ title: title.trim(), ...(workItem ? { workItem } : {}) }),
    );
    if (answer?.kind !== "done") return;
    const id = typeof answer.body.artifact === "string" ? answer.body.artifact : "";
    if (!id) return;
    const chosen = SHEET_TEMPLATES.find((t) => t.key === template) ?? SHEET_TEMPLATES[0];
    const schema = await keyedWrite({ ...configureSchema(id, chosen.schema()), key: newActOpID() });
    if (schema.kind === "done") {
      toast.ok(`Started “${title.trim()}”`);
      nav.to(artifactPath(id));
    } else {
      toast.show({
        variant: "warning",
        title: "The sheet was made, and its columns were not set",
        message: schema.text,
      });
      nav.to(artifactPath(id), { tab: "data", data: "schema" });
    }
  };

  return (
    <Section title={kind.title} hint={kind.hint}>
      <div className="artifact-form">
        <SegmentedControl
          label="Kind"
          semantics="radio"
          size="sm"
          options={STARTS.map((s) => ({ value: s.kind, label: s.label }))}
          value={kind.kind}
          onValueChange={(v: string) => setKind(v)}
        />
        <FormField label="Title" error={titleWrong}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              width="md"
              value={title}
              placeholder={kind.placeholder}
              onChange={(e) => setTitle(e.target.value)}
            />
          )}
        </FormField>
        {kind.kind === "sheet" ? (
          <ChoiceList
            label="Start from"
            value={template}
            onChange={(value) => setTemplate(value === "blank" ? "blank" : "review")}
            options={SHEET_TEMPLATES.map((t) => ({
              value: t.key,
              label: t.label,
              detail: t.description,
            }))}
          />
        ) : (
          <p className="artifact-note">{kind.starts}</p>
        )}
        {workItem && <p className="artifact-note">It will be listed on {workItem}.</p>}
        {create.answer && create.answer.kind !== "done" && (
          <p className="artifact-refusal">{create.answer.text}</p>
        )}
        <div className="artifact-form-actions">
          <Button size="small" variant="ghost" onClick={() => nav.back()}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            loading={create.busy}
            disabledReason={
              !access.can
                ? access.reason
                : title.trim() === ""
                  ? "Give it a title."
                  : (titleWrong ?? undefined)
            }
            onClick={() => void (kind.kind === "sheet" ? start() : startFromStarter(kind.kind))}
          >
            {kind.action}
          </Button>
        </div>
      </div>
    </Section>
  );
}
