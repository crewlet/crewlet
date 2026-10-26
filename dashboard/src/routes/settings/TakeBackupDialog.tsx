/**
 * Taking a backup: `POST /backup?dir=` on the node serving this page.
 *
 * THE DIRECTORY IS ON THE ENGINE'S HOST, and the dialog says which host before
 * anything else — nothing is downloaded, and a path typed as if it were this
 * machine's lands somewhere else entirely.
 *
 * CLOSING DOES NOT CANCEL IT. The engine finishes a copy it has begun whether
 * or not anybody waits (the route runs detached from the request), so the
 * dialog can be closed while it copies — a large store takes minutes — and the
 * outcome still arrives, as a toast, and in the history the engine audits it
 * into. What a closed dialog must not do is drop a failure on the floor, so a
 * refusal that arrives after it closed is a toast too.
 */

import { useEffect, useRef, useState } from "react";
import { Button, Callout, InlineCode, Modal, useToast, ValueField } from "@crewlethq/ui";
import { DatabaseGlyph } from "@crewlethq/icons/glyphs";
import { withProblems } from "~/ui/Problems.tsx";
import { rest } from "~/protocol/index.ts";
import { fmtBytes, fmtDuration } from "~/lib/format.ts";
import {
  BACKUP_WAIT_MS,
  backupRefusal,
  isAbsoluteDir,
  takenBytes,
  type TakenBackup,
} from "~/lib/backups.ts";

export function TakeBackupDialog({
  node,
  suggested,
  onClose,
  onTaken,
}: {
  /** The node serving this page — whose disk the copy lands on. */
  node: string | undefined;
  /** A directory to start from, or "" to leave the choice blank. */
  suggested: string;
  onClose: () => void;
  /** Called once the engine answered, either way, so the history re-reads. */
  onTaken: () => void;
}) {
  const toast = useToast();
  const [dir, setDir] = useState(suggested);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ field: boolean; message: string } | null>(null);
  // WHETHER ANYBODY IS STILL LOOKING, so an answer that arrives after the
  // dialog closed is said somewhere a person will see it. Set on the way IN as
  // well as cleared on the way out, because StrictMode mounts, unmounts and
  // mounts again, and a flag only ever cleared stays cleared — the dialog then
  // never closes on success and its button reads "Copying…" for good.
  const open = useRef(true);
  useEffect(() => {
    open.current = true;
    return () => {
      open.current = false;
    };
  }, []);
  const host = node ?? "this node";
  const absolute = isAbsoluteDir(dir);

  async function take() {
    const target = dir.trim();
    if (busy || !isAbsoluteDir(target)) return;
    setBusy(true);
    setError(null);
    try {
      const answer = await rest.request("POST", "/backup", {
        query: { dir: target },
        body: {},
        timeoutMs: BACKUP_WAIT_MS,
      });
      const manifest = answer.body as TakenBackup;
      toast.show({
        variant: "success",
        title: `Backup written on ${manifest.node_id || host}`,
        message: `${target}: ${fmtBytes(takenBytes(manifest))} in ${fmtDuration(
          Date.parse(manifest.finished_at) - Date.parse(manifest.taken_at),
        )}.`,
      });
      onTaken();
      if (open.current) onClose();
    } catch (err) {
      const refusal = backupRefusal(err);
      onTaken();
      if (open.current) {
        setError(refusal);
      } else {
        toast.failed(refusal.message, { title: `No backup was written to ${target}` });
      }
    } finally {
      if (open.current) setBusy(false);
    }
  }

  return (
    <Modal
      open
      title="Take a backup"
      icon={<DatabaseGlyph size="md" />}
      onClose={onClose}
      size="md"
      stackBody
      onSubmit={() => void take()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {busy ? "Leave it copying" : "Cancel"}
          </Button>
          <Button type="submit" variant="primary" disabled={busy || !absolute}>
            {busy ? "Copying…" : "Take backup"}
          </Button>
        </>
      }
    >
      <p className="t-body secondary" style={{ margin: 0 }}>
        Copies everything <InlineCode>{host}</InlineCode> holds, credentials included, to a
        directory on <strong>its host</strong>.
      </p>
      <ValueField
        label="Directory"
        value={dir}
        onChange={(value) => {
          setDir(value);
          if (error?.field) setError(null);
        }}
        autoFocus
        required
        disabled={busy}
        placeholder="e.g. /var/backups/crewlet-20260930"
        error={withProblems(
          error?.field
            ? error.message
            : dir.trim() !== "" && !absolute
              ? "Use an absolute path on the engine’s host."
              : undefined,
        )}
        help="An absolute, empty path on the engine’s host."
      />
      {busy && (
        <Callout variant="neutral" role="status">
          Copying. You can close this; the result appears in the history.
        </Callout>
      )}
      {error && !error.field && (
        <Callout variant="danger" role="alert">
          <span>{error.message}</span>
        </Callout>
      )}
    </Modal>
  );
}
