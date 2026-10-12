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
 * Teams without a project, and seats above every team without one: a seat in
 * a team files its work under the team's project. Both are read from the
 * company document, which an operator credential opens, and New project is
 * held, with that reason, for a viewer without one.
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
import { useNavigator } from "~/app/router.tsx";
import { KEY_SHAPE } from "~/app/routes.ts";
import { documentUnits } from "~/lib/seats.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { apiToken } from "~/protocol/authToken.ts";
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
 * Every node that could own a new project: teams that name none, then seats
 * above every team that name none and are not moved into a team.
 */
export function ownersOf(doc: CompanyDocument | null | undefined): ProjectOwner[] {
  const teams = documentUnits(doc)
    .filter((unit) => !unit.project)
    .map((unit): ProjectOwner => ({
      id: `unit:${unit.name}`,
      name: unit.name,
      kind: "team",
      ...(unit.type ? { type: unit.type } : {}),
      query: { unit: unit.name },
    }));
  const seats = (doc?.roles ?? [])
    .filter((role) => !role.project && !role.unit)
    .map((role): ProjectOwner => ({
      id: `seat:${role.handle || role.name}`,
      name: role.name,
      kind: "seat",
      query: { seat: role.handle || role.name },
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
  // THE GUARDED DOCUMENT, read only with a credential: New project is held
  // without one, so a reader here has one, and a browser that lost it since is
  // told by the refusal below rather than by a request it could not win.
  const config = useQuery("config", undefined, { enabled: apiToken() !== "" });
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

  const blocked = !owner
    ? "Every team and seat in the org already has a project."
    : !verdict.ok
      ? verdict.message
      : undefined;

  const go = () => {
    if (!owner || !verdict.ok) return;
    onClose();
    nav.to(["agents", "edit"], { ...owner.query, project: key });
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
      {config.error ? (
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
    </Modal>
  );
}
