/**
 * Settings › Ask Crewlet: how Ask Crewlet writes. The company's instructions,
 * set by an administrator in the company document (`assistant.instructions`,
 * a configuration revision like any other), and each person's own, kept under
 * their seat (`save_assistant_instructions`) and applied on top. Both shape
 * tone, length and layout only; what Ask Crewlet may read or change is the
 * engine's, whatever they say.
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
  Text,
  Textarea,
} from "@crewlethq/ui";
import { BuildingComplexGlyph, UserGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { SettingWriteStatus } from "~/components/SettingWriteStatus.tsx";
import type { SettingWrite } from "~/lib/useSettingWrite.ts";
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
        <Stack gap={5}>
          <CompanyCard layer={answer.company} onApplied={query.refetch} />
          <PersonCard seat={answer.seat} layer={answer.mine} revision={answer.revision} />
        </Stack>
      )}
    </>
  );
}

/** The company's layer: an administrator's form, anybody else's reading of it. */
function CompanyCard({
  layer,
  onApplied,
}: {
  layer: AssistantInstructions;
  onApplied: () => void;
}) {
  // WHOSE TO CHANGE is the standing answer; whether it can be pressed NOW
  // (offline, a held document) is the control's reason.
  const may = mayChangeConfig(useViewer());
  const access = useConfigWriteAccess();
  const write = useSettingWrite({
    reread: "Reload to see the company's instructions as they are now.",
    onApplied,
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
      busy={write.busy || applying}
      disabledReason={access.can ? undefined : access.reason}
      status={<CompanyStatus write={write} onReload={onApplied} />}
      onSave={(next) => write.submit(companyInstructionsPlan(layer, next))}
      onEdit={() => {
        if (write.state.kind !== "idle" && !write.busy) write.reset();
      }}
    />
  );
}

/** The reader's own layer, saved under their seat. */
function PersonCard({
  seat,
  layer,
  revision,
}: {
  seat: string;
  layer: AssistantInstructions;
  revision: number;
}) {
  const act = useAct("save_assistant_instructions");
  const [said, setSaid] = useState<string | null>(null);
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
      busy={act.busy}
      disabledReason={act.access.can ? undefined : act.access.reason}
      status={
        act.refusal ? (
          <Callout variant="danger" title="Nothing was saved">
            {act.refusal.sentence}
          </Callout>
        ) : said ? (
          <Text as="p" variant="caption" tone="secondary" role="status">
            {said}
          </Text>
        ) : null
      }
      onEdit={() => {
        setSaid(null);
        if (act.refusal) act.dismiss();
      }}
      onSave={(next) => {
        void act
          .run(
            { instructions: next, if_match: revision },
            { done: "Your instructions saved", quiet: true },
          )
          .then((result) => {
            if (result?.kind === "applied" || result?.kind === "pending") {
              setSaid("Saved. Ask Crewlet follows them from your next question.");
            }
          });
      }}
    />
  );
}

/** What the company layer's save came to, and once saved, when it applies. */
function CompanyStatus({ write, onReload }: { write: SettingWrite; onReload: () => void }) {
  if (write.state.kind !== "saved") return <SettingWriteStatus write={write} onReload={onReload} />;
  return (
    <Text as="p" variant="caption" tone="secondary" role="status">
      {write.state.standing === "applying"
        ? "Saved. Applying it on this node…"
        : "Saved. Ask Crewlet follows them from the next question."}
    </Text>
  );
}

/** One layer's card: its header, two presets and the free text, with Save and Discard. */
function InstructionsForm({
  header,
  name,
  saved,
  busy,
  status,
  disabledReason,
  onSave,
  onEdit,
}: {
  header: ReactNode;
  name: string;
  saved: AssistantInstructions;
  busy: boolean;
  status: ReactNode;
  disabledReason?: string | undefined;
  onSave: (next: AssistantInstructions) => void;
  onEdit: () => void;
}) {
  const [draft, setDraft] = useState<InstructionsDraft>(() => draftOf(saved));
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
