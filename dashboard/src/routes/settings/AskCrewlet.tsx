/**
 * Settings › Ask Crewlet: how Ask Crewlet writes. The company's instructions,
 * set by an administrator in the company document (`assistant.instructions`,
 * a configuration revision like any other), and each person's own, kept under
 * their seat (`save_assistant_instructions`) and applied on top. Both shape
 * tone, length and layout only; what Ask Crewlet may read or change is the
 * engine's, whatever they say.
 *
 * BOTH FORMS ARE ONE DRAFT, held here and declared to Ask Crewlet, so it can
 * read and set what is open; saving stays each card's own press.
 */

import { useState, type ReactNode } from "react";
import {
  Button,
  Callout,
  Card,
  FormField,
  Link,
  PageNote,
  ReadOnlyField,
  SegmentedControl,
  Skeleton,
  Stack,
  Textarea,
} from "@crewlethq/ui";
import { BuildingComplexGlyph, UserGlyph } from "@crewlethq/icons/glyphs";
import {
  refuse,
  useAssistantDraft,
  useDraftState,
  writeParts,
  type DraftFieldSpec,
  type DraftSetter,
} from "~/app/assistant/draft.ts";
import { QueryState } from "~/components/common.tsx";
import { SettingWriteStatus } from "~/components/SettingWriteStatus.tsx";
import { href } from "~/app/router.tsx";
import {
  ASSISTANT_INSTRUCTIONS_MAX_RUNES,
  type AssistantInstructions,
} from "~/contract/instructions.ts";
import {
  FORMAT_CHOICES,
  LENGTH_CHOICES,
  companyInstructionsPlan,
  draftOf,
  isEmptyLayer,
  layerOf,
  overCap,
  sameLayer,
  textLength,
  type InstructionsDraft,
} from "~/lib/instructions.ts";
import type { AssistantInstructionsAnswer } from "~/protocol/types.ts";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useSettingWrite } from "~/lib/useSettingWrite.ts";
import { mayChangeConfig, useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { useViewer } from "~/lib/viewer.ts";

export function AskCrewletSettings() {
  const query = useQuery("assistant_instructions");
  const answer = query.data;
  return (
    <>
      <PageNote>
        How Ask Crewlet writes its answers. Instructions shape tone, length and layout, never what
        it may read or change. Its model is chosen in{" "}
        <Link size="inherit" href={href(["settings", "models"])}>
          Models &amp; Fleets
        </Link>
        .
      </PageNote>
      {!answer ? (
        query.loading ? (
          <Skeleton variant="text" rows={6} label="Loading Ask Crewlet's instructions" />
        ) : (
          <QueryState error={query.error} refusal={query.refusal} loading={query.loading} />
        )
      ) : (
        <Layers answer={answer} onApplied={query.refetch} />
      )}
    </>
  );
}

/** The two layers as the page's one draft. */
interface Both {
  readonly company: InstructionsDraft;
  readonly mine: InstructionsDraft;
}

/** What the page tells Ask Crewlet about the draft it declares. */
const DRAFT_HELP =
  "Length is brief, balanced or detailed and layout lead, lists or tldr; empty is the " +
  `default. The text holds at most ${ASSISTANT_INSTRUCTIONS_MAX_RUNES} characters and never ` +
  "a credential. Saving stays the person's, with each card's Save.";

/** Both cards, over the draft this page holds and declares. */
function Layers({
  answer,
  onApplied,
}: {
  answer: AssistantInstructionsAnswer;
  onApplied: () => void;
}) {
  const may = mayChangeConfig(useViewer());
  const [company, setCompany] = useDraftState<InstructionsDraft>(() => draftOf(answer.company));
  const [mine, setMine] = useDraftState<InstructionsDraft>(() => draftOf(answer.mine));
  useAssistantDraft<Both>({
    kind: "client",
    help: DRAFT_HELP,
    scope: "The instructions open on this page (not saved)",
    value: { company, mine },
    fields: [
      ...layerFields("company", "Company", may),
      ...(answer.seat === "" ? [] : layerFields("mine", "Yours", true)),
    ],
    write: writeParts<Both>({ company, mine }, { company: setCompany, mine: setMine }),
  });
  return (
    <Stack gap={5}>
      <CompanyCard
        layer={answer.company}
        may={may}
        draft={company}
        setDraft={setCompany}
        onApplied={onApplied}
      />
      <PersonCard
        seat={answer.seat}
        layer={answer.mine}
        revision={answer.revision}
        draft={mine}
        setDraft={setMine}
      />
    </Stack>
  );
}

/** One layer's three fields as Ask Crewlet reads them, each set only where it may be. */
function layerFields(layer: keyof Both, whose: string, writable: boolean): DraftFieldSpec<Both>[] {
  const set = (d: Both, part: Partial<InstructionsDraft>): Both => ({
    ...d,
    [layer]: { ...d[layer], ...part },
  });
  const choice = (
    key: "length" | "format",
    label: string,
    choices: readonly { value: string }[],
  ): DraftFieldSpec<Both> => {
    const allowed = choices.map((c) => c.value);
    return {
      path: `${layer}.${key}`,
      label: `${whose}: ${label}`,
      hint: `${allowed.filter(Boolean).join(", ")}, or empty for the default`,
      read: (d) => d[layer][key],
      ...(writable
        ? {
            write: (d: Both, v: unknown) =>
              typeof v === "string" && allowed.includes(v)
                ? set(d, { [key]: v } as Partial<InstructionsDraft>)
                : refuse(`${label} is ${allowed.filter(Boolean).join(", ")}, or empty`),
          }
        : {}),
    };
  };
  return [
    choice("length", "Length", LENGTH_CHOICES),
    choice("format", "Layout", FORMAT_CHOICES),
    {
      path: `${layer}.text`,
      label: `${whose}: Anything else`,
      hint: `at most ${ASSISTANT_INSTRUCTIONS_MAX_RUNES} characters`,
      read: (d) => d[layer].text,
      ...(writable
        ? {
            write: (d: Both, v: unknown) =>
              typeof v === "string" && textLength(v) <= ASSISTANT_INSTRUCTIONS_MAX_RUNES
                ? set(d, { text: v })
                : refuse(`The text holds at most ${ASSISTANT_INSTRUCTIONS_MAX_RUNES} characters`),
          }
        : {}),
    },
  ];
}

/** The company's layer: an administrator's form, anybody else's reading of it. */
function CompanyCard({
  layer,
  may,
  draft,
  setDraft,
  onApplied,
}: {
  layer: AssistantInstructions;
  may: boolean;
  draft: InstructionsDraft;
  setDraft: DraftSetter<InstructionsDraft>;
  onApplied: () => void;
}) {
  // WHOSE TO CHANGE is the standing answer; whether it can be pressed NOW
  // (offline, a held document) is the control's reason.
  const access = useConfigWriteAccess();
  const write = useSettingWrite({
    reread: "Reload to see the company's instructions as they are now.",
    onApplied,
    said: {
      done: "Saved the company's instructions",
      applied: "Ask Crewlet follows them from the next question.",
    },
  });
  const header = (
    <Card.Header
      icon={<BuildingComplexGlyph size="sm" />}
      subtitle="For everybody in the company. Each person can add their own on top."
      subtitlePlacement="below"
    >
      <Card.Title>Company instructions</Card.Title>
    </Card.Header>
  );
  if (!may) {
    return (
      <Card as="section">
        {header}
        <Card.Body>
          <LayerReading layer={layer} reason="Set by an administrator for everybody." />
        </Card.Body>
      </Card>
    );
  }
  // HELD WHILE A SAVE APPLIES, until the reread brings the stored layer back.
  const applying = write.state.kind === "saved" && write.state.standing === "applying";
  return (
    <InstructionsForm
      header={header}
      name="company"
      saved={layer}
      draft={draft}
      setDraft={setDraft}
      busy={write.busy || applying}
      disabledReason={access.can ? undefined : access.reason}
      status={<SettingWriteStatus write={write} onReload={onApplied} />}
      onSave={(next) => write.submit(companyInstructionsPlan(layer, next))}
      onEdit={() => {
        if (write.state.kind !== "idle" && !write.busy) write.reset();
      }}
    />
  );
}

/** What a saved layer of the reader's own does, said in its save's toast. */
const FOLLOWS_YOURS = "Ask Crewlet follows them from your next question.";

/** The reader's own layer, saved under their seat. */
function PersonCard({
  seat,
  layer,
  revision,
  draft,
  setDraft,
}: {
  seat: string;
  layer: AssistantInstructions;
  revision: number;
  draft: InstructionsDraft;
  setDraft: DraftSetter<InstructionsDraft>;
}) {
  const act = useAct("save_assistant_instructions");
  const header = (
    <Card.Header
      icon={<UserGlyph size="sm" />}
      subtitle="Only for you, and they win where they differ from the company's."
      subtitlePlacement="below"
    >
      <Card.Title>Your instructions</Card.Title>
    </Card.Header>
  );
  if (seat === "") {
    return (
      <Card as="section">
        {header}
        <Card.Body>
          <Callout variant="neutral">
            Your own instructions are kept under the seat you hold, and this sign-in holds none.
          </Callout>
        </Card.Body>
      </Card>
    );
  }
  return (
    <InstructionsForm
      header={header}
      name="yours"
      saved={layer}
      draft={draft}
      setDraft={setDraft}
      busy={act.busy}
      disabledReason={act.access.can ? undefined : act.access.reason}
      status={
        act.refusal ? (
          <Callout variant="danger" title="Nothing was saved">
            {act.refusal.sentence}
          </Callout>
        ) : null
      }
      onEdit={() => {
        if (act.refusal) act.dismiss();
      }}
      onSave={(next) => {
        void act.run(
          { instructions: next, if_match: revision },
          { done: "Saved your instructions", landed: () => FOLLOWS_YOURS },
        );
      }}
    />
  );
}

/** One layer's card: its header, two presets and the free text, with Save and Discard. */
function InstructionsForm({
  header,
  name,
  saved,
  draft,
  setDraft,
  busy,
  status,
  disabledReason,
  onSave,
  onEdit,
}: {
  header: ReactNode;
  name: string;
  saved: AssistantInstructions;
  draft: InstructionsDraft;
  setDraft: DraftSetter<InstructionsDraft>;
  busy: boolean;
  status: ReactNode;
  disabledReason?: string | undefined;
  onSave: (next: AssistantInstructions) => void;
  onEdit: () => void;
}) {
  const [from, setFrom] = useState(saved);
  // A SAVE LANDED OR ANOTHER BROWSER SAVED: the form follows the stored layer
  // unless the reader is mid-edit.
  if (!sameLayer(from, saved)) {
    setFrom(saved);
    if (sameLayer(layerOf(draft), from)) setDraft(draftOf(saved));
  }
  const dirty = !sameLayer(layerOf(draft), saved);
  const over = overCap(draft);
  const edit = (next: Partial<InstructionsDraft>) => {
    setDraft({ ...draft, ...next });
    onEdit();
  };
  const held = disabledReason !== undefined;
  // THE SLOTS ARE THE CARD'S OWN CHILDREN, so it lays them out as its slots.
  return (
    <Card as="section">
      {header}
      <Card.Body>
        <Stack gap={4}>
          <FormField label="Length" as="fieldset">
            <SegmentedControl
              label={`${name} length`}
              semantics="radio"
              layout="chips"
              value={draft.length}
              options={LENGTH_CHOICES.map((c) => ({ ...c, disabled: held }))}
              onValueChange={(length) => edit({ length })}
            />
          </FormField>
          <FormField label="Layout" as="fieldset">
            <SegmentedControl
              label={`${name} layout`}
              semantics="radio"
              layout="chips"
              value={draft.format}
              options={FORMAT_CHOICES.map((c) => ({ ...c, disabled: held }))}
              onValueChange={(format) => edit({ format })}
            />
          </FormField>
          <FormField
            label="Anything else"
            helper="Tone, words to use or avoid, how to structure an answer. Never a password or key."
            error={
              over ? `Shorten it to ${ASSISTANT_INSTRUCTIONS_MAX_RUNES} characters.` : undefined
            }
            limit={{ length: textLength(draft.text), max: ASSISTANT_INSTRUCTIONS_MAX_RUNES }}
          >
            {(field) => (
              <Textarea
                id={field.id}
                aria-describedby={field.describedBy}
                aria-invalid={field.invalid || undefined}
                error={field.invalid}
                autoResize
                rows={3}
                value={draft.text}
                readOnly={held}
                placeholder="For example: Use British spelling. Lead with the numbers."
                onChange={(e) => edit({ text: e.target.value })}
              />
            )}
          </FormField>
          {status}
        </Stack>
      </Card.Body>
      <Card.Footer variant="actions">
        <Button
          variant="ghost"
          disabled={!dirty || busy}
          onClick={() => {
            setDraft(draftOf(saved));
            onEdit();
          }}
        >
          Discard
        </Button>
        <Button
          variant="primary"
          loading={busy}
          disabledReason={
            disabledReason ??
            (!dirty
              ? "Nothing to save"
              : over
                ? `Shorten the text to ${ASSISTANT_INSTRUCTIONS_MAX_RUNES} characters`
                : undefined)
          }
          onClick={() => onSave(layerOf(draft))}
        >
          Save
        </Button>
      </Card.Footer>
    </Card>
  );
}

/** A layer as somebody who may not change it reads it. */
function LayerReading({ layer, reason }: { layer: AssistantInstructions; reason: string }) {
  if (isEmptyLayer(layer)) {
    return (
      <ReadOnlyField label="Instructions" value="None: Ask Crewlet's defaults" reason={reason} />
    );
  }
  const word = <T extends string>(choices: readonly { value: T | ""; label: string }[], v?: T) =>
    choices.find((c) => c.value === (v ?? ""))?.label ?? "Default";
  return (
    <Stack gap={3}>
      <ReadOnlyField label="Length" value={word(LENGTH_CHOICES, layer.length)} />
      <ReadOnlyField label="Layout" value={word(FORMAT_CHOICES, layer.format)} />
      <ReadOnlyField label="Anything else" value={layer.text ?? "Nothing"} reason={reason} />
    </Stack>
  );
}
