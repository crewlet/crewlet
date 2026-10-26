/**
 * Company › Brand (`#/company/brand`): how the company looks and sounds in
 * every doc, deck and page its agents make.
 *
 * # What the page shows
 *
 * The brand an administrator PUBLISHED (`brand{}`, an adoption in the engine's
 * word, which the routes and the permission keep): one version of the
 * company's brand kit, snapshotted by the engine, which every artifact and
 * agent reads.
 * Four cards (Logos, Colours, Typography, Voice) in the main column and a
 * sticky preview beside them. Every reader reads it, downloads a logo and
 * copies a colour: the brand is the company's public face, and this page is
 * its guidelines for people too.
 *
 * # Who changes it, and how
 *
 * An administrator, a person, with a recent proof. Each card edits in place
 * (Edit, then Save or Cancel, one card at a time, the preview drawing every
 * change), and Save publishes a version of the company's kit and adopts it in
 * one gesture, after a dialog that lists what changes, which contrast pairs
 * fall short and what publishing makes public. A version somebody else
 * drafted (an agent, an editor of the kit) is offered for review and
 * publishing from the notice under the header. A reader who may not publish
 * is handed no control at all.
 *
 * # Setting it up
 *
 * The brand is set up on a page of its own (`#/company/brand/setup`, six
 * steps), entered from the empty state, from the notice over a draft a setup
 * left ("Finish draft", or "Edit draft" once it is ready) and from the bar
 * ("Run setup again"). A draft that lacks what publishing requires says so
 * wherever it is offered.
 *
 * # A draft in progress
 *
 * With none published, a standing draft is drawn in the brand's own cards,
 * read-only, its preview tagged as a draft, under a notice saying it is not
 * live, who last changed it and when, and the ONE next step that fits it:
 * "Publish brand" for an administrator once it is ready, "Finish draft"
 * while it is not. Its owner or an administrator may discard it, which moves
 * the kit to the trash (`DELETE /artifacts/{id}`, `artifacts.trash`), where
 * its owner can restore it until it is purged.
 *
 * # Nothing is claimed before the brand arrives
 *
 * "No brand yet" is a statement about the company, so the page draws a
 * skeleton until the read answers, and only then the cards, the draft or the
 * empty state.
 */

import { useMemo, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  Card,
  ConfirmModal,
  EmptyState,
  Menu,
  Skeleton,
  Stack,
  useToast,
} from "@crewlethq/ui";
import {
  EllipsisGlyph,
  ExternalLinkGlyph,
  PencilGlyph,
  PlusGlyph,
  RotateCcwClockGlyph,
  SwatchBookGlyph,
  TrashGlyph,
  WandSparklesGlyph,
  XGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useLeaveGuard, useNavigator } from "~/app/router.tsx";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import {
  BRAND_ASSISTANT_HELP,
  applyBrandDraft,
  brandDraftFields,
  brandDraftLabel,
  brandStandingDraftHelp,
  type BrandDraft,
} from "~/lib/brand/assistantDraft.ts";
import { usePageMenu } from "~/app/Shell.tsx";
import { artifactPath } from "~/app/routes.ts";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { fmtDate } from "~/lib/format.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { trashArtifact } from "~/lib/artifacts/api.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { actorName, purgeSentence, roleIncludes } from "~/lib/artifacts/words.ts";
import { clearBrand } from "~/lib/brand/api.ts";
import { mayAdoptBrand, serializeBrandDocument, type Adoption } from "~/lib/brand/brand.ts";
import { draftSource, saveFiles, type BrandEdit } from "~/lib/brand/draft.ts";
import {
  adoptionSource,
  logoDigests,
  useBrand,
  useBrandAssets,
  type BrandAssets,
} from "~/lib/brand/useBrand.ts";
import { useBrandSave } from "~/lib/brand/useBrandSave.ts";
import { draftReadiness } from "~/lib/brand/validate.ts";
import { BRAND_SETUP } from "~/components/brand.tsx";
import { BRAND_CARDS, type BrandCardEdit, type BrandCardKey } from "./BrandCards.tsx";
import {
  ColoursEditor,
  LogosEditor,
  TypographyEditor,
  VoiceEditor,
  cardProblem,
  type BrandEditorProps,
} from "./BrandEditors.tsx";
import { BrandBoard } from "./BrandBoard.tsx";
import { AdoptDialog } from "./AdoptDialog.tsx";
import { ReviewAdopt } from "./ReviewAdopt.tsx";
import { useCompanyKit, type CompanyKit } from "./useCompanyKit.ts";

const DESCRIPTION = "How the company looks and sounds in every doc, deck and page its agents make.";

/** Each card's editor. */
const EDITORS: Readonly<Record<BrandCardKey, (props: BrandEditorProps) => React.ReactNode>> = {
  logos: LogosEditor,
  colours: ColoursEditor,
  type: TypographyEditor,
  voice: VoiceEditor,
};

/** The card being changed, and the edit as it stands. */
interface Editing {
  card: BrandCardKey;
  edit: BrandEdit;
}

/**
 * Whether the reader may continue a standing draft's setup: an administrator,
 * or with none adopted the editor whose kit it is (the company kit is found
 * among the kits they may write).
 */
function mayContinueDraft(canAdopt: boolean, adoption: Adoption | null): boolean {
  return canAdopt || adoption === null;
}

/**
 * Whether the reader is offered discarding a standing draft: only with none
 * published, since a newer version of the published kit is not a kit of its
 * own to discard, and only to whoever `artifacts.trash` admits (the kit's
 * owner or an administrator, a person). The engine decides the write; this
 * only decides whether the control is drawn.
 */
function mayDiscardDraft(
  viewer: { admin: boolean; kind: string },
  adoption: Adoption | null,
  draft: CompanyKit,
): boolean {
  if (adoption !== null || viewer.kind !== "human") return false;
  return viewer.admin || roleIncludes(draft.role, "owner");
}

/** Whether an edit changes anything from the adopted brand. */
function changed(editing: Editing | null, adoption: Adoption | null | undefined): boolean {
  if (!editing || !adoption) return false;
  return (
    editing.edit.files.size > 0 ||
    serializeBrandDocument(editing.edit.document) !== serializeBrandDocument(adoption.document)
  );
}

export function BrandPage() {
  const viewer = useViewer();
  const brand = useBrand();
  const adoption = brand.adoption;
  const people = useArtifactPeople();
  // ADOPTING IS AN ADMINISTRATOR'S, AND A PERSON'S (`artifacts.brand.adopt`):
  // an agent never holds admin, and the page offers it no control.
  const canAdopt = mayAdoptBrand(viewer);
  // THE DRAFT IS READ for whoever may continue it: an administrator, or with
  // none adopted the editor whose kit it is. Anybody else is told only that a
  // newer version exists, by the adoption itself.
  const company = useCompanyKit(adoption, canAdopt, { read: canAdopt || adoption === null });
  // THE DRAFT A SETUP LEFT (or an agent or an editor published), where one
  // stands unadopted: what the notice under the header offers.
  const draft = company?.draft ? company : null;

  const [editing, setEditing] = useState<Editing | null>(null);
  const [confirming, setConfirming] = useState<"save" | "clear" | "discard" | null>(null);
  const [reviewing, setReviewing] = useState<number | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [leaving, setLeaving] = useState<{ leave: () => void } | null>(null);

  const base = useMemo(() => (adoption ? adoptionSource(adoption) : null), [adoption]);
  // WHAT THE CARDS DRAW: the published brand, an edit of it while a card is
  // changed, or with none published the draft that stands.
  const standingSource = adoption === null ? (draft?.read.source ?? null) : null;
  const shown = useMemo(
    () => (editing ? draftSource(base, editing.edit) : (base ?? standingSource)),
    [editing, base, standingSource],
  );
  const assets = useBrandAssets(shown);

  const dirty = changed(editing, adoption);
  // WHAT IS OPEN, for the assistant: the card in edit, its fields set through
  // the same edit its controls write; Save and its adoption stay the person's.
  // With no card in edit, a DRAFT A SETUP LEFT that the notice offers to
  // continue: read-only, as the saved draft it is, its help naming the setup
  // where it is changed. A page that declared nothing here told the assistant
  // only "Company › Brand", and it answered that no draft existed.
  const standing = draft?.read.document;
  const standingReady = standing ? draftReadiness(standing) === undefined : false;
  useAssistantDraft<BrandDraft>(
    editing && adoption
      ? {
          kind: "client",
          id: adoption.kit,
          revision: adoption.kit_version,
          step: editing.card,
          help: BRAND_ASSISTANT_HELP,
          scope: `${BRAND_CARDS[editing.card].title} you are editing (not saved)`,
          value: editing.edit,
          fields: brandDraftFields(new Set([editing.card])),
          apply: (next) =>
            applyBrandDraft(
              editing.edit,
              next,
              (change) => setEditing((was) => (was ? { ...was, edit: change(was.edit) } : was)),
              setNotice,
            ),
          blocked: () => (confirming ? "The change is being saved." : null),
        }
      : draft && standing && adoption !== undefined && mayContinueDraft(canAdopt, adoption)
        ? {
            kind: "server",
            id: draft.kit,
            revision: draft.version,
            help: brandStandingDraftHelp(standingReady),
            scope: "The brand draft in progress (saved, not published)",
            value: { document: standing, files: new Map() },
            fields: brandDraftFields(new Set()),
          }
        : null,
  );
  useLeaveGuard(
    dirty
      ? (_to, leave) => {
          setLeaving({ leave });
          return true;
        }
      : null,
  );

  const refreshed = () => {
    setEditing(null);
    setConfirming(null);
    setReviewing(null);
    brand.refetch();
  };

  const facts: Fact[] = adoption
    ? [
        // TWO FACTS, EACH ONE SHORT LINE: the day and the person as one
        // value wrapped to three lines in its track ("Published / Oct 24,
        // 2026 by / Maya Chen") on a wide page.
        { label: "Published", value: fmtDate(adoption.adopted_at) },
        { label: "Published by", value: actorName(adoption.adopted_by, people.seatName) },
        {
          label: "Version",
          value: String(adoption.kit_version),
          // A LINK ONLY WHERE IT OPENS: the kit an agent drafted may be
          // shared with nobody, and a reader who cannot see it would land
          // on Not Found. An administrator opens any artifact.
          ...(canAdopt
            ? {
                path: artifactPath(adoption.kit),
                query: { version: String(adoption.kit_version) },
              }
            : {}),
        },
      ]
    : [];

  return (
    <Stack gap={5}>
      <ObjectHeader
        kind="Company"
        icon="swatch-book"
        title="Brand"
        description={DESCRIPTION}
        facts={facts}
      />
      {canAdopt && adoption ? (
        <BarActions adoption={adoption} onClear={() => setConfirming("clear")} />
      ) : null}
      {notice ? (
        <Callout
          variant="warning"
          action={
            <Button
              size="small"
              variant="ghost"
              leadingIcon={<XGlyph size="sm" />}
              onClick={() => setNotice(null)}
            >
              Dismiss
            </Button>
          }
        >
          {notice}
        </Callout>
      ) : null}
      {draft && adoption !== undefined ? (
        <DraftNotice
          adoption={adoption}
          draft={draft}
          edited={draftEdited(draft, adoption, people.seatName)}
          canAdopt={canAdopt}
          canDiscard={mayDiscardDraft(viewer, adoption, draft)}
          onReview={() => setReviewing(draft.version)}
          onDiscard={() => setConfirming("discard")}
        />
      ) : null}
      {adoption === undefined || (adoption === null && company === undefined) ? (
        <div aria-busy="true">
          <Card>
            <Skeleton variant="text" rows={6} label="Loading the company's brand" />
          </Card>
        </div>
      ) : adoption === null && standing ? (
        <BrandBoard document={standing} assets={assets} drawn="draft" />
      ) : adoption === null && draft?.read.loading ? (
        <div aria-busy="true">
          <Card>
            <Skeleton variant="text" rows={6} label="Loading the brand draft" />
          </Card>
        </div>
      ) : adoption === null ? (
        <NoBrand canAdopt={canAdopt} drafting={draft !== null} />
      ) : (
        <Cards
          adoption={adoption}
          editing={editing}
          assets={assets}
          canAdopt={canAdopt}
          onEdit={setEditing}
          onSave={() => setConfirming("save")}
        />
      )}

      {confirming === "save" && editing && adoption ? (
        <SaveDialog
          adoption={adoption}
          editing={editing}
          onClose={() => setConfirming(null)}
          onSaved={(text) => {
            setNotice(text);
            refreshed();
          }}
        />
      ) : null}
      {confirming === "discard" && draft ? (
        <DiscardDialog
          draft={draft}
          name={standing?.name.trim() ?? ""}
          onClose={() => setConfirming(null)}
          onDiscarded={() => {
            setConfirming(null);
            draft.refetch();
          }}
        />
      ) : null}
      {confirming === "clear" && adoption ? (
        <ClearDialog
          adoption={adoption}
          onClose={() => setConfirming(null)}
          onCleared={refreshed}
        />
      ) : null}
      {reviewing !== null && draft && adoption !== undefined ? (
        <ReviewAdopt
          kit={draft.kit}
          version={reviewing}
          adoption={adoption}
          onClose={() => setReviewing(null)}
          onAdopted={refreshed}
        />
      ) : null}
      <ConfirmModal
        open={leaving !== null}
        onClose={() => setLeaving(null)}
        onConfirm={() => {
          const go = leaving?.leave;
          setLeaving(null);
          setEditing(null);
          go?.();
        }}
        title="Discard your changes?"
        message={
          editing
            ? `Your changes to ${BRAND_CARDS[editing.card].title} are not saved.`
            : "Your changes are not saved."
        }
        confirmLabel="Discard changes"
        cancelLabel="Keep editing"
        destructive
      />
    </Stack>
  );
}

/**
 * The administrator's controls in the page bar: the kit's history, and a way to
 * stop using the brand. ON A PHONE THE MENU FOLDS INTO THE BAR'S ONE "MORE"
 * (`usePageMenu`): beside it, its own ellipsis was a second "···" a reader
 * could not tell apart. History stays the one action in view.
 */
function BarActions({ adoption, onClear }: { adoption: Adoption; onClear: () => void }) {
  const nav = useNavigator();
  const openKit = () => nav.to(artifactPath(adoption.kit));
  // THE SETUP AGAIN, prefilled from the kit where it stands: every part in
  // one walk, where a card changes one.
  const setUp = () => nav.to(BRAND_SETUP);
  usePageMenu([
    {
      key: "setup",
      label: "Run setup again",
      icon: <WandSparklesGlyph size="sm" />,
      onSelect: setUp,
    },
    {
      key: "kit",
      label: "Open the brand kit",
      icon: <ExternalLinkGlyph size="sm" />,
      onSelect: openKit,
    },
    {
      key: "clear",
      label: "Stop using this brand",
      icon: <XGlyph size="sm" />,
      onSelect: onClear,
    },
  ]);
  return (
    <PageActions>
      <ButtonLink
        href={href(artifactPath(adoption.kit), { panel: "history" })}
        size="small"
        variant="ghost"
        leadingIcon={<RotateCcwClockGlyph size="sm" />}
      >
        History
      </ButtonLink>
      <span className="page-action-folds">
        <Menu
          label="More brand actions"
          icon={<EllipsisGlyph size="sm" />}
          triggerVariant="ghost"
          align="end"
          items={[
            {
              key: "setup",
              label: "Run setup again",
              icon: <WandSparklesGlyph size="sm" />,
              onSelect: setUp,
            },
            {
              key: "kit",
              label: "Open the brand kit",
              icon: <ExternalLinkGlyph size="sm" />,
              onSelect: openKit,
            },
            { kind: "separator", key: "sep" },
            {
              key: "clear",
              label: "Stop using this brand",
              icon: <XGlyph size="sm" />,
              danger: true,
              onSelect: onClear,
            },
          ]}
        />
      </span>
    </PageActions>
  );
}

/** Who last changed a standing draft, by name, and when, where the engine said. */
interface Edited {
  by: string;
  at: string | null;
}

/**
 * Who last changed the draft: the author of its version and the day it was
 * published, where the version was read; otherwise who published the newer
 * version, as the adoption names them to a reader the kit is not read for.
 */
function draftEdited(
  draft: CompanyKit,
  adoption: Adoption | null,
  nameOf: (handle: string) => string,
): Edited | null {
  const read = draft.read.edited;
  if (read) return { by: actorName(read.by, nameOf), at: read.at };
  if (adoption?.newer_by) return { by: actorName(adoption.newer_by, nameOf), at: null };
  return null;
}

/**
 * A draft of the kit nobody published yet: a version newer than the published
 * one, or, with none published, the draft a setup left. It says that the
 * draft is not live, who last changed it and when, whether it is ready to
 * publish, and offers the ONE next step that fits as the primary action:
 *
 * - Ready, for an administrator: "Publish brand" (the review, then the
 *   confirm), with "Edit draft" beside it.
 * - Not ready, for whoever continues it: "Finish draft", the setup, beside
 *   the sentence saying what publishing still needs.
 * - Ready, for an editor who may not publish: "Edit draft", and that an
 *   administrator publishes it.
 *
 * "Discard draft" stays quiet, for its owner or an administrator with none
 * published.
 */
function DraftNotice({
  adoption,
  draft,
  edited,
  canAdopt,
  canDiscard,
  onReview,
  onDiscard,
}: {
  adoption: Adoption | null;
  draft: CompanyKit;
  edited: Edited | null;
  canAdopt: boolean;
  canDiscard: boolean;
  onReview: () => void;
  onDiscard: () => void;
}) {
  const doc = draft.read.document;
  const readiness = doc ? draftReadiness(doc) : undefined;
  const ready = doc !== null && readiness === undefined;
  const name = doc?.name.trim() ?? "";
  const brand = name ? `the ${name} brand` : "the company's brand";
  const what = adoption
    ? `Version ${draft.version} of ${brand} is a draft. Artifacts keep using the published version ${adoption.kit_version} until this one is published.`
    : `This draft of ${brand} is not published yet, so artifacts still use Crewlet's look.`;
  const who = edited
    ? edited.at
      ? ` Last edited by ${edited.by} on ${fmtDate(edited.at)}.`
      : ` Last edited by ${edited.by}.`
    : "";
  // CONTINUING IS [mayContinueDraft]'s; PUBLISHING is held from a draft the
  // engine would refuse, and from one not read yet.
  const mayContinue = mayContinueDraft(canAdopt, adoption);
  const mayPublish = canAdopt && ready;
  const label = brandDraftLabel(ready);
  const actions = (
    <>
      {mayPublish ? (
        <Button size="small" variant="primary" onClick={onReview}>
          Publish brand
        </Button>
      ) : null}
      {mayContinue ? (
        <ButtonLink
          href={href(BRAND_SETUP)}
          size="small"
          variant={ready ? "secondary" : "primary"}
          leadingIcon={ready ? <PencilGlyph size="sm" /> : <WandSparklesGlyph size="sm" />}
        >
          {label}
        </ButtonLink>
      ) : null}
      {canDiscard ? (
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<TrashGlyph size="sm" />}
          onClick={onDiscard}
        >
          Discard draft
        </Button>
      ) : null}
    </>
  );
  return (
    <Callout
      variant="info"
      icon={<WandSparklesGlyph size="sm" />}
      action={mayPublish || mayContinue || canDiscard ? actions : undefined}
    >
      {what}
      {who}
      {readiness ? ` ${readiness}` : ""}
      {canAdopt || readiness ? "" : " An administrator can review and publish it."}
    </Callout>
  );
}

/**
 * No brand is published and no draft is drawn: the way to set one up for an
 * administrator, and what that means for everyone else. While a draft is in
 * progress but cannot be read, its notice carries the way on, and the empty
 * state offers no second one.
 */
function NoBrand({ canAdopt, drafting }: { canAdopt: boolean; drafting: boolean }) {
  return (
    <>
      {canAdopt && !drafting ? (
        <Callout variant="info" icon={<WandSparklesGlyph size="sm" />}>
          Add your logo, colours and type, and every doc, deck and page your agents make will look
          like yours.
        </Callout>
      ) : null}
      <Card>
        <EmptyState
          icon={<SwatchBookGlyph size="lg" />}
          title="No brand yet"
          description="Artifacts use Crewlet's look until an administrator publishes the company's logo, colours and type."
          action={
            canAdopt && !drafting ? (
              <ButtonLink
                href={href(BRAND_SETUP)}
                variant="primary"
                leadingIcon={<PlusGlyph size="sm" />}
              >
                Set up your brand
              </ButtonLink>
            ) : undefined
          }
        />
      </Card>
    </>
  );
}

/** The four cards and the preview beside them, each card read or changed in place. */
function Cards({
  adoption,
  editing,
  assets,
  canAdopt,
  onEdit,
  onSave,
}: {
  adoption: Adoption;
  editing: Editing | null;
  assets: BrandAssets;
  canAdopt: boolean;
  onEdit: (next: Editing | null) => void;
  onSave: () => void;
}) {
  const document = editing?.edit.document ?? adoption.document;
  const cardEdit = (card: BrandCardKey): BrandCardEdit | undefined => {
    if (!canAdopt) return undefined;
    const title = BRAND_CARDS[card].title;
    if (editing?.card === card) {
      const Editor = EDITORS[card];
      const hold = cardProblem(card, editing.edit);
      return {
        editor: (
          <Editor edit={editing.edit} onEdit={(edit) => onEdit({ card, edit })} assets={assets} />
        ),
        footer: (
          <>
            <Button size="small" variant="ghost" onClick={() => onEdit(null)}>
              Cancel
            </Button>
            <Button size="small" variant="primary" disabledReason={hold} onClick={onSave}>
              Save
            </Button>
          </>
        ),
      };
    }
    return {
      actions: (
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<PencilGlyph size="sm" />}
          aria-label={`Edit ${title}`}
          disabledReason={
            editing
              ? `Save or cancel your changes to ${BRAND_CARDS[editing.card].title} first.`
              : undefined
          }
          onClick={() => onEdit({ card, edit: { document: adoption.document, files: new Map() } })}
        >
          Edit
        </Button>
      ),
    };
  };
  return (
    <BrandBoard
      document={document}
      assets={assets}
      drawn={editing ? "changes" : "brand"}
      edit={cardEdit}
    />
  );
}

/** Saving one card's change: publish a version of the kit and adopt it, after the publish dialog. */
function SaveDialog({
  adoption,
  editing,
  onClose,
  onSaved,
}: {
  adoption: Adoption;
  editing: Editing;
  onClose: () => void;
  /** Published (no words), or a version saved and not published, in its words. */
  onSaved: (notice: string | null) => void;
}) {
  const toast = useToast();
  const save = useBrandSave();
  const [failed, setFailed] = useState<string | null>(null);
  const base = useMemo(() => adoptionSource(adoption), [adoption]);
  const draft = useMemo(() => draftSource(base, editing.edit), [base, editing.edit]);
  const doc = editing.edit.document;

  const confirm = async () => {
    setFailed(null);
    let files;
    try {
      files = await saveFiles(base, editing.edit);
    } catch (err) {
      setFailed(err instanceof Error ? err.message : String(err));
      return;
    }
    const answer = await save.run({
      kit: adoption.kit,
      // THE KIT'S HEAD: the newest version, which the engine answers an
      // administrator beside the adopted one.
      head: adoption.newer_version ?? adoption.kit_version,
      title: `${doc.name.trim()} brand`,
      document: doc,
      files,
    });
    if (answer?.kind === "adopted") {
      toast.ok(`The ${doc.name.trim()} brand is published`);
      onSaved(null);
    } else if (answer?.kind === "published") {
      onSaved(answer.text);
    }
  };

  const answer = save.answer;
  return (
    <AdoptDialog
      open
      onClose={onClose}
      title={`Save and publish this as the ${doc.name.trim()} brand?`}
      before={adoption.document}
      after={doc}
      digests={{ before: logoDigests(base), after: logoDigests(draft) }}
      confirmLabel="Save and publish"
      busy={save.busy}
      answer={
        failed ??
        (answer && (answer.kind === "refused" || answer.kind === "unknown")
          ? answer.text
          : undefined)
      }
      onConfirm={() => void confirm()}
    />
  );
}

/** Stopping the adopted brand: artifacts return to Crewlet's look. The kit stays. */
function ClearDialog({
  adoption,
  onClose,
  onCleared,
}: {
  adoption: Adoption;
  onClose: () => void;
  onCleared: () => void;
}) {
  const toast = useToast();
  const gesture = useKeyedGesture();
  const name = adoption.document.name;
  return (
    <ConfirmModal
      open
      onClose={onClose}
      onConfirm={async () => {
        const answer = await gesture.run(clearBrand());
        if (answer?.kind === "done") {
          toast.ok(`The ${name} brand is no longer used`);
          onCleared();
        }
      }}
      title={`Stop using the ${name} brand?`}
      message={
        gesture.answer && gesture.answer.kind !== "done"
          ? gesture.answer.text
          : "Docs and dashboards return to Crewlet's look now, and pages and designs when they are next published. Existing decks keep their look, and the brand kit stays in Artifacts to publish again."
      }
      confirmLabel="Stop using it"
      cancelLabel="Keep it"
      destructive
      submitting={gesture.busy}
    />
  );
}

/**
 * Discarding a standing draft: the kit moves to the trash (`DELETE
 * /artifacts/{id}`, decided by `artifacts.trash`: its owner's or an
 * administrator's, a person's), stops opening at once, and is purged after
 * the trash term unless its owner restores it. Nothing was published, so
 * artifacts keep Crewlet's look; the next setup continues another brand kit
 * the reader may write, or starts a new one.
 */
function DiscardDialog({
  draft,
  name,
  onClose,
  onDiscarded,
}: {
  draft: CompanyKit;
  name: string;
  onClose: () => void;
  onDiscarded: () => void;
}) {
  const toast = useToast();
  const gesture = useKeyedGesture();
  const brand = name ? `the ${name} brand draft` : "the brand draft";
  return (
    <ConfirmModal
      open
      onClose={onClose}
      onConfirm={async () => {
        const answer = await gesture.run(trashArtifact(draft.kit));
        if (answer?.kind === "done") {
          toast.ok(`Discarded ${brand}`);
          onDiscarded();
        }
      }}
      title="Discard this draft?"
      message={
        gesture.answer && gesture.answer.kind !== "done"
          ? gesture.answer.text
          : `This moves ${brand} to the trash in Artifacts. Nothing was published, so artifacts keep Crewlet's look. ${purgeSentence(new Date().toISOString())}`
      }
      confirmLabel="Discard draft"
      cancelLabel="Keep draft"
      destructive
      submitting={gesture.busy}
    />
  );
}
