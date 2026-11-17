-- A person keeps the files of a change they viewed, each by digests under its repository's viewed key.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; a repository's viewed_key record writes its key once, from the record's own bytes, and the repository's purge deletes it; only the viewed records on a person's own subject write their marks, each a function of the record and that person's rows before it; so a replay rebuilds both. No purge removes a mark, and a mark holds no path, object id or mode, only HMAC-SHA256 digests that name nothing once the purge has deleted the key.
-- About 150 bytes a repository any file of which was ever marked (its id in the row and in the primary key, and the key's 64 hex digits), written once; about 340 bytes a mark (the person, the change and the path digest in the row and in the primary key, the file digest, the instant and the position), at most gitlet.MaxViewedFiles a person. A viewed record writes a row for each file it marks or takes back, and deletes its person's rows of the ended changes it forgets and their oldest past the cap.
-- Class: code_viewed_keys and code_viewed_files are reproducible (gitlet.ReproducibleTables).

CREATE TABLE code_viewed_keys (
    repo_id TEXT NOT NULL PRIMARY KEY,
    secret  TEXT NOT NULL    -- the key, 32 bytes in lowercase hex (gitlet.ViewedKey)
);

CREATE TABLE code_viewed_files (
    handle      TEXT    NOT NULL,
    change_id   TEXT    NOT NULL,
    path_digest TEXT    NOT NULL,   -- gitlet.ViewedKey.PathDigest of the path
    file_digest TEXT    NOT NULL,   -- gitlet.ViewedKey.FileDigest of the path, object id and mode marked
    at          INTEGER NOT NULL,
    position    INTEGER NOT NULL,
    PRIMARY KEY (handle, change_id, path_digest)
);
