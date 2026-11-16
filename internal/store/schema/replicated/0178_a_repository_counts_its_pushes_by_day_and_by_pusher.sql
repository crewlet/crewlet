-- A repository counts its pushes by UTC day, each day's by quarter-hour, and by pusher and UTC day.
--
-- WHO HAS TO AGREE ON IT: every data node, identically; the push apply, the history's horizon and a purge part keep both in step with code_pushes' own rows and the applier re-derives them (gitlet.DerivationVersion), so this file fills nothing.
-- At 0091's scale target (250 000 pushes a day, kept 181 days): code_push_days is a row of about 200 bytes with its key a repository and UTC day pushed on, whatever the rate (60 repositories, some 11 000 rows; 10 000 pushed daily, 1.8 million, about 360 MB); code_pusher_days one of about 170 a pusher, repository and UTC day (three a day over 10 000 repositories, 5.4 million, about 0.9 GB; at most one a push row). A push record writes one day row and one pusher row a pusher; a horizon or a purge part rewrites one of each for every day and pusher day of the rows it deletes.
-- Class: code_push_days and code_pusher_days are reproducible (gitlet.ReproducibleTables).

CREATE TABLE code_push_days (
    repo_id TEXT    NOT NULL,
    day     INTEGER NOT NULL,   -- a UTC midnight, store.EncodeTime's microseconds
    pushes  INTEGER NOT NULL,   -- the day's pushes: q00 to q95 together
    -- q00 to q95: each quarter-hour's pushes from the day's midnight, a line an hour
    q00 INTEGER NOT NULL, q01 INTEGER NOT NULL, q02 INTEGER NOT NULL, q03 INTEGER NOT NULL,
    q04 INTEGER NOT NULL, q05 INTEGER NOT NULL, q06 INTEGER NOT NULL, q07 INTEGER NOT NULL,
    q08 INTEGER NOT NULL, q09 INTEGER NOT NULL, q10 INTEGER NOT NULL, q11 INTEGER NOT NULL,
    q12 INTEGER NOT NULL, q13 INTEGER NOT NULL, q14 INTEGER NOT NULL, q15 INTEGER NOT NULL,
    q16 INTEGER NOT NULL, q17 INTEGER NOT NULL, q18 INTEGER NOT NULL, q19 INTEGER NOT NULL,
    q20 INTEGER NOT NULL, q21 INTEGER NOT NULL, q22 INTEGER NOT NULL, q23 INTEGER NOT NULL,
    q24 INTEGER NOT NULL, q25 INTEGER NOT NULL, q26 INTEGER NOT NULL, q27 INTEGER NOT NULL,
    q28 INTEGER NOT NULL, q29 INTEGER NOT NULL, q30 INTEGER NOT NULL, q31 INTEGER NOT NULL,
    q32 INTEGER NOT NULL, q33 INTEGER NOT NULL, q34 INTEGER NOT NULL, q35 INTEGER NOT NULL,
    q36 INTEGER NOT NULL, q37 INTEGER NOT NULL, q38 INTEGER NOT NULL, q39 INTEGER NOT NULL,
    q40 INTEGER NOT NULL, q41 INTEGER NOT NULL, q42 INTEGER NOT NULL, q43 INTEGER NOT NULL,
    q44 INTEGER NOT NULL, q45 INTEGER NOT NULL, q46 INTEGER NOT NULL, q47 INTEGER NOT NULL,
    q48 INTEGER NOT NULL, q49 INTEGER NOT NULL, q50 INTEGER NOT NULL, q51 INTEGER NOT NULL,
    q52 INTEGER NOT NULL, q53 INTEGER NOT NULL, q54 INTEGER NOT NULL, q55 INTEGER NOT NULL,
    q56 INTEGER NOT NULL, q57 INTEGER NOT NULL, q58 INTEGER NOT NULL, q59 INTEGER NOT NULL,
    q60 INTEGER NOT NULL, q61 INTEGER NOT NULL, q62 INTEGER NOT NULL, q63 INTEGER NOT NULL,
    q64 INTEGER NOT NULL, q65 INTEGER NOT NULL, q66 INTEGER NOT NULL, q67 INTEGER NOT NULL,
    q68 INTEGER NOT NULL, q69 INTEGER NOT NULL, q70 INTEGER NOT NULL, q71 INTEGER NOT NULL,
    q72 INTEGER NOT NULL, q73 INTEGER NOT NULL, q74 INTEGER NOT NULL, q75 INTEGER NOT NULL,
    q76 INTEGER NOT NULL, q77 INTEGER NOT NULL, q78 INTEGER NOT NULL, q79 INTEGER NOT NULL,
    q80 INTEGER NOT NULL, q81 INTEGER NOT NULL, q82 INTEGER NOT NULL, q83 INTEGER NOT NULL,
    q84 INTEGER NOT NULL, q85 INTEGER NOT NULL, q86 INTEGER NOT NULL, q87 INTEGER NOT NULL,
    q88 INTEGER NOT NULL, q89 INTEGER NOT NULL, q90 INTEGER NOT NULL, q91 INTEGER NOT NULL,
    q92 INTEGER NOT NULL, q93 INTEGER NOT NULL, q94 INTEGER NOT NULL, q95 INTEGER NOT NULL,
    PRIMARY KEY (repo_id, day)
);

CREATE TABLE code_pusher_days (
    repo_id     TEXT    NOT NULL,
    day         INTEGER NOT NULL,   -- a UTC midnight, store.EncodeTime's microseconds
    pusher_kind TEXT    NOT NULL,   -- gitlet.PusherKind
    pusher      TEXT    NOT NULL,   -- a seat's or a person's human seat's handle; '' for gitlet
    pushes      INTEGER NOT NULL,
    PRIMARY KEY (repo_id, day, pusher_kind, pusher)
);
