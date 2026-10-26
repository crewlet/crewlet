-- An assistant conversation keeps its files: the images a person attaches to a
-- question and the files a deep-work run delivers (ADR-0053).
-- internal/assistantdomain is the domain.
--
-- WHO HAS TO AGREE ON IT: a row naming the file is replicated, derived from the
-- CREWLET_ASSISTANT_LOG stream like every other assistant row, and the file's
-- bytes are ONE OBJECT in the fleet's object store (ADR-0026), because a file is
-- up to five mebibytes and the estate is copied whole onto every data node. The
-- object is uploaded before the record naming it is published, and the
-- collector deletes it once no row names it: a purge or an erasure removes the
-- row with the rest of the conversation, and the object goes at the collector's
-- next pass. The table is declared in internal/objstore/references.
--
-- ## Sealed twice
--
-- `name_sealed` is a keyring envelope like every `*_sealed` column of this
-- domain. The OBJECT is sealed too: its bytes are the envelope of the file's
-- content under the fleet keyring, with associated data naming the
-- conversation and the file, so the object store (a bucket somebody else may
-- run) holds nothing a person attached or was shown in the clear.
-- `content_key` is the keyring key that envelope is sealed under, read off the
-- envelope by the writer, so a rotation counts what is still sealed under an
-- old key without fetching a byte. `hash` and `object_size` describe the SEALED
-- bytes the object holds, which every read is checked against; `size` is the
-- file's own.
--
-- `turn_id` is '' for an attachment uploaded before the question it is sent
-- with: the message that carries it names it.
--
-- No UNIQUE outside the primary key and no foreign key, for 0050's reasons.
CREATE TABLE assistant_files (
    id              TEXT    NOT NULL PRIMARY KEY,
    conversation_id TEXT    NOT NULL,
    turn_id         TEXT    NOT NULL DEFAULT '',
    owner_id        TEXT    NOT NULL,
    -- An internal/assistant/wire FileOrigin: attached or delivered.
    origin          TEXT    NOT NULL,
    name_sealed     TEXT    NOT NULL,
    -- The type sniffed from the file's bytes, which a download is served as.
    media_type      TEXT    NOT NULL,
    size            INTEGER NOT NULL,
    object          TEXT    NOT NULL,
    hash            TEXT    NOT NULL,
    object_size     INTEGER NOT NULL,
    content_key     TEXT    NOT NULL,
    created_at      INTEGER NOT NULL,
    version         INTEGER NOT NULL
);
CREATE INDEX assistant_files_conversation_idx ON assistant_files (conversation_id, origin); -- a purge's delete, and the per-conversation bound
CREATE INDEX assistant_files_object_idx ON assistant_files (object);                   -- which of a batch of objects any file names, and every named object in key order: the collector, its audit and the backup (objstore.ReferenceTable.ObjectsAmong, ReferencesAfter)

-- A user message's attachments, as the person sent them: a JSON array of
-- internal/assistant/wire FileRefs, sealed like the message's text.
ALTER TABLE assistant_messages ADD COLUMN attachments_sealed TEXT NOT NULL DEFAULT '';
