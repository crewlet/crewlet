/**
 * New project: an owner and a key, then Edit org.
 *
 * # A project is the org's to make
 *
 * The engine creates a project when a team, or a seat above every team, names
 * a tracker project key in the company configuration, and there is no other
 * write that declares one. So this dialog asks the two things that key needs
 * (whose it is, and what it is called) and continues in Edit org with that
 * node's editor open on its Project field, holding the key and not yet
 * applied: the reader saves it there, through the review every change to the
 * org takes. Nothing is written from here.
 *
 * # Who can own one
 *
 * Teams without a project, and agent seats above every team without one: a
 * seat in a team files its work under the team's project, and a person owns
 * no project at all — the engine refuses a human seat one
 * (`org.Role.humanForbidden`), since nothing files work for a person by it.
 * Each is offered by its
 * IDENTITY — a team by its key, a seat by its handle — which is what Edit org
 * opens an editor at, since two teams may share a name.
 *
 * # Who may make one
 *
 * Whoever may change the company's configuration ([useConfigWriteAccess]:
 * Admin, on a document the deployment does not manage for
 * somebody else). Not a lead on their own team: a project key is one of the
 * KEYS another system finds a team by, and it outlives the team that declared
 * it, so the engine refuses a lead every write that sets one
 * (`config.DiffOrg`'s keyed fields, `routes/org/builder/model/scope.ts`). A
 * dialog that offered a lead their team would hand them to a Project field
 * Edit org draws locked. So for anybody the gate refuses, this says the
 * gate's own sentence and reads nothing.
 *
 * # The key is checked as the engine will
 *
 * Its shape (`KEY_SHAPE`: capital letters, digits and underscores, starting
 * with a letter), and that no project, active or archived, and no other node
 * in the document already has it.
 */

import { useEffect, useMemo, useState } from "react";
import {
  Button,
  FormField,
  Input,
  KeyChip,
  Modal,
  Select,
  Text,
  type SelectOption,
} from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { KEY_SHAPE, ORG_EDIT_PATH } from "~/app/routes.ts";
import { documentUnits } from "~/lib/seats.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import type { CompanyDocument } from "~/protocol/index.ts";

/** The longest key the field takes. */
const KEY_MAX = 12;
/** How long typing rests before the key is looked up. */
const CHECK_DELAY_MS = 150;

/** A node that can own a project, as the dialog offers it and Edit org is asked for it. */
export interface ProjectOwner {
  /** The option's value: unique across teams and seats. */
  id: string;
  /** What the owner is called, which is what the project will be called. */
  name: string;
  kind: "team" | "seat";
  /** The type a unit declares, drawn beside a team's name. */
  type?: string;
  /** The address Edit org opens the owner's editor at. */
  query: Record<string, string>;
}

/**
 * Every node that could own a new project: teams that name none, then agent
 * seats above every team that name none and are not moved into a team.
 *
 * BY ITS IDENTITY, never its name: a team by its key and a seat by its handle,
 * which every write of the document mints and which is what Edit org's
 * `unit=` and `seat=` find a node by. A node the document holds no identity
 * for has no address to continue at, so it is not offered.
 */
export function ownersOf(doc: CompanyDocument | null | undefined): ProjectOwner[] {
  const teams = documentUnits(doc)
    .filter((unit) => !unit.project && !!unit.id)
    .map((unit): ProjectOwner => ({
      id: `unit:${unit.id}`,
      name: unit.name || unit.id!,
      kind: "team",
      ...(unit.type ? { type: unit.type } : {}),
      query: { unit: unit.id! },
    }));
  const seats = (doc?.roles ?? [])
    .filter((role) => role.kind !== "human" && !role.project && !role.unit && !!role.handle)
    .map((role): ProjectOwner => ({
      id: `seat:${role.handle}`,
      name: role.name || role.handle!,
      kind: "seat",
      query: { seat: role.handle! },
    }));
  return [...teams, ...seats];
}

/** Every project key the document already names, so a draft one is not a second. */
function keysIn(doc: CompanyDocument | null | undefined): Set<string> {
  const keys = new Set<string>();
  for (const unit of documentUnits(doc)) if (unit.project) keys.add(unit.project);
  for (const role of doc?.roles ?? []) if (role.project) keys.add(role.project);
  return keys;
}

/** A key the field holds: capitals, digits and underscores, cut at the field's length. */
export function cleanKey(typed: string): string {
  return typed
    .toUpperCase()
    .replace(/[^A-Z0-9_]/g, "")
    .slice(0, KEY_MAX);
}

/** A key to suggest for an owner: the first letters of its name. */
export function suggestedKey(name: string): string {
  const letters = name.toUpperCase().replace(/[^A-Z]/g, "");
  return letters.slice(0, 3) || "PRJ";
}

/** What the key field says about the key it holds, and whether Continue may go on. */
export function keyVerdict(
  key: string,
  owner: ProjectOwner | undefined,
  taken: { inDocument: boolean; inTracker: boolean | null },
): { ok: boolean; message: string } {
  if (key === "") {
    return { ok: false, message: `Choose a key, such as ${suggestedKey(owner?.name ?? "")}.` };
  }
  if (!KEY_SHAPE.test(key)) {
    return {
      ok: false,
      message: "Use capital letters, digits and underscores, starting with a letter.",
    };
  }
  if (taken.inDocument || taken.inTracker === true) {
    return { ok: false, message: `${key} is already a project.` };
  }
  if (taken.inTracker === null) return { ok: false, message: `Checking ${key}…` };
  return { ok: true, message: `Tasks will be numbered ${key}-1, ${key}-2 and on.` };
}

export function NewProjectDialog({ onClose }: { onClose: () => void }) {
  const nav = useNavigator();
  // THE GUARDED DOCUMENT, read only by a reader the gate admits: anybody else
  // is told the gate's own sentence below rather than handed a refusal from a
  // read they could not win, or a list of owners they could not give a key.
  const access = useConfigWriteAccess();
  const config = useQuery("config", undefined, { enabled: access.can });
  const doc = config.error ? null : (config.data ?? null);
  const owners = useMemo(() => ownersOf(doc), [doc]);
  const taken = useMemo(() => keysIn(doc), [doc]);

  const [ownerId, setOwnerId] = useState("");
  const owner = owners.find((o) => o.id === ownerId) ?? owners[0];
  const [key, setKey] = useState("");

  // THE TRACKER IS ASKED FOR THE KEY ITSELF once typing rests: an exact
  // answer for an archived project too, and no listing's cap to fall under.
  // Each answer is kept WITH THE KEY IT ANSWERS, so a reply to a key the
  // reader has since changed is never read as the verdict on the new one.
  const { socket } = useClient();
  const [checked, setChecked] = useState<{ key: string; taken: boolean } | null>(null);
  useEffect(() => {
    if (!KEY_SHAPE.test(key)) return;
    let live = true;
    const timer = window.setTimeout(() => {
      socket.query("work_project", { key }).then(
        () => {
          if (live) setChecked({ key, taken: true });
        },
        // NOT FOUND IS A FREE KEY. Any other refusal is not a taken one
        // either: the save in Edit org checks the key again, so the reader
        // goes on rather than waiting on a read this dialog cannot win.
        () => {
          if (live) setChecked({ key, taken: false });
        },
      );
    }, CHECK_DELAY_MS);
    return () => {
      live = false;
      window.clearTimeout(timer);
    };
  }, [key, socket]);
  const inTracker = checked?.key === key ? checked.taken : null;

  const verdict = keyVerdict(key, owner, { inDocument: taken.has(key), inTracker });

  const options: SelectOption[] = owners.map((o) => ({
    value: o.id,
    label: `${o.name} (${o.kind === "team" ? (o.type ?? "team") : "seat"})`,
    group: o.kind === "team" ? "Teams" : "Seats",
  }));

  const blocked = !access.can
    ? access.reason
    : !owner
      ? "Every team and seat in the org already has a project."
      : !verdict.ok
        ? verdict.message
        : undefined;

  const go = () => {
    if (!access.can || !owner || !verdict.ok) return;
    onClose();
    nav.to(ORG_EDIT_PATH, { ...owner.query, project: key });
  };

  return (
    <Modal
      open
      onClose={onClose}
      title="New project"
      size="sm"
      stackBody
      onSubmit={go}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" type="submit" disabledReason={blocked}>
            Continue in Edit org
          </Button>
        </>
      }
    >
      {!access.can ? (
        <Text as="p" role="alert" className="row gap-2">
          {access.block === "no_grant" && <KeyGlyph size="xs" aria-hidden="true" />}
          {access.reason}
        </Text>
      ) : config.error ? (
        <Text as="p" role="alert">
          Couldn’t load the org. Close this and try again.
        </Text>
      ) : (
        <>
          <FormField
            label="Owner"
            htmlFor="new-project-owner"
            helper="Only teams and seats without a project are listed."
          >
            <Select
              id="new-project-owner"
              ariaLabel="Owner"
              placeholder={config.loading ? "Reading the org…" : "Nobody without a project"}
              value={owner?.id}
              options={options}
              disabled={owners.length === 0}
              onChange={(next) => setOwnerId(String(next))}
            />
          </FormField>
          <FormField
            label="Project key"
            htmlFor="new-project-key"
            {...(verdict.ok || key === ""
              ? { helper: verdict.message }
              : { error: verdict.message })}
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                appearance="reference"
                error={!verdict.ok && key !== ""}
                value={key}
                maxLength={KEY_MAX}
                autoComplete="off"
                spellCheck={false}
                onChange={(event) => setKey(cleanKey(event.target.value))}
              />
            )}
          </FormField>
          {/* THE PROJECT AS IT WILL BE DRAWN: its key and its owner's name. */}
          <div className="row gap-2" aria-live="polite">
            <KeyChip>{key || "?"}</KeyChip>
            <Text>{owner?.name ?? ""}</Text>
          </div>
        </>
      )}
      {access.can && (
        <Text as="p" variant="caption" className="row gap-2">
          <KeyGlyph size="xs" aria-hidden="true" />
          Saving the key takes Admin access, since a project outlives its team.
        </Text>
      )}
    </Modal>
  );
}
