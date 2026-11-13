/**
 * Add entry: something the company runs on that no source reports, recorded
 * in the person's own name with what they know of it (its name, its vendor,
 * who answers for it, what it is for in one line, and the Doclet page that
 * documents it). One write: the engine records each part and the same press
 * finishes one that stopped part of the way.
 *
 * A key or a value shaped like a credential is refused by the engine, never
 * stored masked, and its refusal is drawn here in its own words.
 */

import { useState, type ReactNode } from "react";
import { Button, FormField, Input, Modal, Select, type SelectOption } from "@crewlethq/ui";
import { PlusGlyph } from "@crewlethq/icons/glyphs";
import { usePeekControls } from "~/app/frame/peekAddress.ts";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import type { AtlasKind, AtlasVendor } from "~/contract/atlas.ts";
import type { ActionTool } from "~/protocol/act.ts";
import { useAct, type Act } from "~/lib/useAct.ts";
import { EntryPicker, OwnerField, PageField } from "./pickers.tsx";
import { KINDS, VENDORS } from "./words.ts";
import { ADDABLE_KINDS, KEY_HINTS, addArgs, belongs, draftKey, type AddDraft } from "./writes.ts";

/** The longest name or purpose the engine keeps: `atlas.MaxLabelRunes`. */
export const LABEL_MAX = 120;

/** What a dialog says while its press is out. */
export const WAITING = "Waiting for the engine to answer";

const KIND_OPTIONS: SelectOption[] = ADDABLE_KINDS.map((kind) => ({
  value: kind,
  label: KINDS[kind].word,
}));

const VENDOR_OPTIONS: SelectOption[] = (Object.keys(VENDORS) as AtlasVendor[])
  .filter((vendor) => vendor !== "gitlet")
  .map((vendor) => ({ value: vendor, label: VENDORS[vendor].name }))
  .sort((a, b) =>
    a.value === "other" ? 1 : b.value === "other" ? -1 : a.label.localeCompare(b.label),
  );

/** A service's key from its name: what its grammar admits of it. */
function slug(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 40)
    .replace(/-+$/, "");
}

const EMPTY: AddDraft = {
  kind: "service",
  key: "",
  name: "",
  vendor: "other",
  account: "",
  purpose: "",
  owner: null,
  page: null,
};

/**
 * A dialog that makes one write: its fields, the engine's refusal under them,
 * and Cancel beside the press. It closes once the write is applied or sent,
 * and stays open on a refusal so the field it names can be changed.
 */
export function WriteDialog<T extends ActionTool>({
  title,
  subtitle,
  icon,
  write,
  blocked,
  submitLabel,
  danger = false,
  alert = false,
  size = "md",
  onClose,
  onSubmit,
  children,
}: {
  title: string;
  subtitle?: ReactNode;
  icon?: ReactNode;
  write: Act<T>;
  /** Why the press would change nothing yet. */
  blocked?: string | undefined;
  submitLabel: string;
  /** The write cannot be undone: the press is drawn as the danger it is. */
  danger?: boolean;
  alert?: boolean;
  size?: "sm" | "md" | "lg";
  onClose: () => void;
  /** Make the write; the dialog closes when it resolves applied or pending. */
  onSubmit: () => Promise<{ kind: string } | null>;
  children: ReactNode;
}) {
  const submit = async () => {
    // ENTER REACHES HERE WITHOUT THE BUTTON'S GATE, so the same check holds it.
    if (!pressable(write, blocked)) return;
    const result = await onSubmit();
    if (result?.kind === "applied" || result?.kind === "pending") onClose();
  };
  return (
    <Modal
      open
      size={size}
      phoneSheet
      stackBody
      role={alert ? "alertdialog" : "dialog"}
      title={title}
      subtitle={subtitle}
      icon={icon}
      onClose={onClose}
      dismissable={!write.busy}
      closeDisabledReason={write.busy ? WAITING : undefined}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={write.busy}>
            Cancel
          </Button>
          <WriteButton
            write={write}
            variant={danger ? "danger" : "primary"}
            showRefusal={false}
            blocked={blocked}
            onPress={() => void submit()}
          >
            {submitLabel}
          </WriteButton>
        </>
      }
    >
      {children}
      {!write.access.can && <p className="muted">{write.access.reason}</p>}
      <RefusalNote write={write} />
    </Modal>
  );
}

export function AddEntry({ onClose }: { onClose: () => void }) {
  const write = useAct("atlas_add_entry");
  const { open } = usePeekControls();
  const [draft, setDraft] = useState<AddDraft>(EMPTY);
  // THE KEY FOLLOWS A SERVICE'S NAME until somebody types a key of their own.
  const [keyed, setKeyed] = useState(false);
  const set = (patch: Partial<AddDraft>) => setDraft((held) => ({ ...held, ...patch }));
  const account = draft.kind === "vendor_account";
  const hint = KEY_HINTS[draft.kind];
  const key = draftKey(draft);
  const blocked =
    draft.key.trim() === "" && !(account && draft.vendor !== "other")
      ? account
        ? "Give the account a short name"
        : "Give its key"
      : undefined;
  return (
    <WriteDialog
      title="Add entry"
      subtitle="Something the company runs on that no source reports yet. It is recorded in your name and confirmed by you."
      icon={<PlusGlyph size="md" />}
      write={write}
      blocked={blocked}
      submitLabel="Add entry"
      onClose={onClose}
      onSubmit={async () => {
        const result = await write.run(addArgs(draft), {
          done: `Added ${draft.name.trim() || key} to the atlas`,
        });
        if (result?.kind === "applied") {
          const entry = (result.receipt as { entry?: unknown } | null)?.entry;
          if (typeof entry === "string") open({ kind: "atlas", id: entry });
        }
        return result;
      }}
    >
      <FormField label="Kind" required>
        {(field) => (
          <Select
            id={field.id}
            value={draft.kind}
            options={KIND_OPTIONS}
            onChange={(kind) => set({ kind: kind as AtlasKind, account: "" })}
          />
        )}
      </FormField>
      {account && (
        <FormField
          label="Vendor"
          required
          helper="A vendor the atlas does not know is Other, with a short name as its key."
        >
          {(field) => (
            <Select
              id={field.id}
              value={draft.vendor}
              options={VENDOR_OPTIONS}
              searchable
              onChange={(vendor) => set({ vendor: vendor as AtlasVendor })}
            />
          )}
        </FormField>
      )}
      <FormField
        label="Name"
        optional
        limit={{ length: [...draft.name].length, max: LABEL_MAX }}
        helper="Plain text, shown in place of the key."
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            value={draft.name}
            onChange={(event) => {
              const name = event.target.value;
              set(draft.kind === "service" && !keyed ? { name, key: slug(name) } : { name });
            }}
          />
        )}
      </FormField>
      <FormField
        label={account ? "Account id or short name" : "Key"}
        required={!(account && draft.vendor !== "other")}
        helper={`${hint.helper} Such as ${hint.example}.${account ? ` Recorded as ${key}.` : ""}`}
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            className="mono"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            value={draft.key}
            onChange={(event) => {
              setKeyed(true);
              set({ key: event.target.value });
            }}
          />
        )}
      </FormField>
      {belongs(draft.kind) && (
        <EntryPicker
          label="Vendor account"
          helper="The account it belongs to, whose vendor it is then shown under. Add the account first where the atlas holds none."
          kinds={["vendor_account"]}
          value={draft.account}
          onChange={(id) => set({ account: id })}
          optional
        />
      )}
      <OwnerField value={draft.owner} onChange={(owner) => set({ owner })} nobody="No owner yet" />
      <FormField
        label="Purpose"
        optional
        limit={{ length: [...draft.purpose].length, max: LABEL_MAX }}
        helper="What it is for, in one line."
      >
        {(field) => (
          <Input
            id={field.id}
            aria-describedby={field.describedBy}
            value={draft.purpose}
            onChange={(event) => set({ purpose: event.target.value })}
          />
        )}
      </FormField>
      <PageField value={draft.page} onChange={(page) => set({ page })} optional />
    </WriteDialog>
  );
}
