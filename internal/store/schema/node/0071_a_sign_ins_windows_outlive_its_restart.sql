-- A sign-in's limit windows outlive the node's restart.
--
-- Each row is the newest reading this node's calls heard of one of a sign-in's
-- vendor windows: its state, the share used, when it resets and when it was
-- heard. A revision's apply reads them back into the sign-in's plan, so an LLM
-- fleet balances on what its members last reported rather than on nothing;
-- one heard over half an hour ago reads as unknown.
--
-- WHO HAS TO AGREE ON IT: this node alone. A reading is what this node's own
-- calls heard; a peer keeps what its calls heard.
CREATE TABLE llm_sign_in_window (
    sign_in   TEXT    NOT NULL,
    win       TEXT    NOT NULL,
    model     TEXT    NOT NULL DEFAULT '',
    hours     INTEGER NOT NULL DEFAULT 0,
    state     TEXT    NOT NULL,
    used      REAL,
    resets_at INTEGER,
    seen      INTEGER NOT NULL,
    PRIMARY KEY (sign_in, win, model)
);
