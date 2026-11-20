package gitlet

// The reader's statements, each registered in plan_test.go so the pinned
// driver's planner is held to the index each is written for.
const (
	repoViewSQL = `SELECT id, name, status, managed_by, kind, owner_kind, owner, visibility, search,
		mirror_from != '', default_branch, description, deleted_at, last_activity_at, refs, branches, tags,
		tombstones, live_packs,
		live_bytes, live_objects, drop_epoch, tombstones_through, retired_through, layers_position, pushes,
		last_push_at, created_at FROM code_repos WHERE id = ?`
	packColumns       = `pack_id, part, object, hash, size, index_bytes, reverse_bytes, git_checksum, objects, kind, state, added_position, content_position, arrived_at`
	packsFullFirstSQL = `SELECT ` + packColumns + ` FROM code_packs WHERE repo_id = ?
		ORDER BY pack_id, part LIMIT ?`
	packsFullAfterSQL = `SELECT ` + packColumns + ` FROM code_packs INDEXED BY ` + packsKey + `
		WHERE repo_id = ? AND pack_id >= ? AND (pack_id > ? OR part > ?)
		ORDER BY pack_id, part LIMIT ?`
	packsAddedFirstSQL = `SELECT ` + packColumns + ` FROM code_packs WHERE repo_id = ?
		AND added_position > ? ORDER BY added_position, pack_id, part LIMIT ?`
	packsAddedAfterSQL = `SELECT ` + packColumns + ` FROM code_packs WHERE repo_id = ?
		AND added_position >= ? AND (added_position > ? OR pack_id > ? OR (pack_id = ? AND part > ?))
		ORDER BY added_position, pack_id, part LIMIT ?`
	refsFullFirstSQL = `SELECT ref, target, position FROM code_refs WHERE repo_id = ? AND target != ''
		ORDER BY ref LIMIT ?`
	refsFullAfterSQL = `SELECT ref, target, position FROM code_refs INDEXED BY ` + refsKey + `
		WHERE repo_id = ? AND target != '' AND ref > ? ORDER BY ref LIMIT ?`
	refsDeltaFirstSQL = `SELECT ref, target, position FROM code_refs WHERE repo_id = ? AND position > ?
		ORDER BY position, ref LIMIT ?`
	refsDeltaAfterSQL = `SELECT ref, target, position FROM code_refs WHERE repo_id = ?
		AND position >= ? AND (position > ? OR ref > ?) ORDER BY position, ref LIMIT ?`
	// retiredAboveSQL is a BOUNDED COUNT of a repository's retirements above
	// ?2, at most ?3 deep over the retired phase's index ([choose]).
	retiredAboveSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_packs_retired
		INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ?1 AND retired_position > ?2 LIMIT ?3)`
	// deltaRowsSQL is a BOUNDED COUNT of the refs a delta from ?2 moves and
	// the packs it adds, each table counted at most ?3 deep over the index
	// the delta's pages read (deltaOutweighs).
	deltaRowsSQL = `SELECT
		(SELECT COUNT(*) FROM (SELECT 1 FROM code_refs WHERE repo_id = ?1 AND position > ?2 LIMIT ?3))
		+ (SELECT COUNT(*) FROM (SELECT 1 FROM code_packs WHERE repo_id = ?1 AND added_position > ?2 LIMIT ?3))`
	// THE RETIRED PHASE, over code_packs_retired_repo_idx in its order —
	// those in (?2, ?3] first, and after a cursor (?3 the upper bound, ?4 to
	// ?7 the cursor) with the >= beside the OR that keeps the range seek.
	retiredColumns  = `pack_id, part, git_checksum, retired_at, retired_position`
	retiredFirstSQL = `SELECT ` + retiredColumns + ` FROM code_packs_retired
		INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ? AND retired_position > ?
		AND retired_position <= ? ORDER BY retired_position, pack_id, part LIMIT ?`
	retiredAfterSQL = `SELECT ` + retiredColumns + ` FROM code_packs_retired
		INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ? AND retired_position >= ?
		AND retired_position <= ? AND (retired_position > ? OR pack_id > ? OR (pack_id = ? AND part > ?))
		ORDER BY retired_position, pack_id, part LIMIT ?`
	pushColumns = `push_id, repo_id, position, at, actor, actor_kind, actor_seat, via, turn_id,
		work_backend, work_key, model, run_id, harness, node, atomic, commands, pack_id, pack_bytes,
		introduced, introduced_count, offered, received_at`
	pushSQL        = `SELECT ` + pushColumns + ` FROM code_pushes WHERE push_id = ?`
	pushesFirstSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		ORDER BY position DESC, push_id DESC LIMIT ?`
	pushesAfterSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position <= ? AND (position < ? OR push_id < ?)
		ORDER BY position DESC, push_id DESC LIMIT ?`
	reposColumns = `SELECT r.id, r.name, r.status, r.managed_by, r.kind, r.owner_kind, r.owner, r.visibility,
		r.search, r.mirror_from != '', r.default_branch, r.description, r.deleted_at, r.last_activity_at, r.refs,
		r.live_packs,
		r.live_bytes, r.live_objects, r.drop_epoch, r.pushes, r.last_push_at, h.target`
	reposHead = ` LEFT JOIN code_refs h INDEXED BY ` + refsKey + `
			ON h.repo_id = r.id AND h.ref = 'refs/heads/' || r.default_branch AND h.target != ''`
	// reposFrom is a listing's rows, by name, and protectedReposFrom those a
	// rule protects ([ReadQuery.Protected]), walked by [protectedReposKey],
	// NAMED since code_repos_name_idx serves the same order, its predicate
	// repeated verbatim. Each ends at the name range the statements below spell.
	reposFrom = reposColumns + `
		FROM code_repos r` + reposHead + `
		WHERE r.status IN (SELECT value FROM json_each(?)) AND `
	protectedReposFrom = reposColumns + `
		FROM code_repos r INDEXED BY ` + protectedReposKey + reposHead + `
		WHERE r.` + protectedRepos + ` AND r.status IN (SELECT value FROM json_each(?)) AND `
	// reposSQL is a page of the whole listing, past its cursor, and
	// protectedReposSQL a page of the protected ones.
	reposSQL          = reposFrom + `r.name > ? ORDER BY r.name LIMIT ?`
	protectedReposSQL = protectedReposFrom + `r.name > ? ORDER BY r.name LIMIT ?`
	// THE NAMES A PREFIX BEGINS ([ReadQuery.Prefix]), below [prefixEnd]: FROM
	// the prefix on a first page (a name may be it whole), PAST the cursor
	// after. Two statements, since with both lower bounds the planner may seek
	// by the empty cursor and walk every name below the prefix.
	reposUnderFirstSQL          = reposFrom + `r.name >= ? AND r.name < ? ORDER BY r.name LIMIT ?`
	reposUnderAfterSQL          = reposFrom + `r.name > ? AND r.name < ? ORDER BY r.name LIMIT ?`
	protectedReposUnderFirstSQL = protectedReposFrom + `r.name >= ? AND r.name < ? ORDER BY r.name LIMIT ?`
	protectedReposUnderAfterSQL = protectedReposFrom + `r.name > ? AND r.name < ? ORDER BY r.name LIMIT ?`
	// pushRulesOfSQL is a listing page's rules as a push is judged by them,
	// in one statement ([protectionsOf]) — pushRulesSQL for every repository
	// the page keeps, never selecting the gate.
	pushRulesOfSQL = `SELECT repo_id, pattern, allow_force, allow_delete, direct_push
		FROM code_protections WHERE repo_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
		ORDER BY repo_id, pattern`
	// repoGrantSQL is what a grant reads of one repository's row, and its
	// status ([grantRefIn]).
	repoGrantSQL = `SELECT name, status, owner_kind, owner, visibility FROM code_repos WHERE id = ?`
	// liveRefSQL is one live ref's target, the position it last moved at, that
	// move's instant and the seat that set it, by its whole name: a
	// repository's head, and a ref a RefQuery names (liveRefIn).
	liveRefSQL = `SELECT target, position, at, seat FROM code_refs INDEXED BY ` + refsKey + `
		WHERE repo_id = ? AND ref = ? AND target != ''`
	// refsUnderSQL is a page of the live refs between two names, by name —
	// over the live refs' partial index, its predicate verbatim
	// (refsListing).
	refsUnderSQL = `SELECT ref, target, position, at FROM code_refs INDEXED BY ` + liveRefsKey + `
		WHERE repo_id = ? AND ref > ? AND ref < ? AND target != '' ORDER BY ref LIMIT ?`
	// THE PUSHES IN LOG ORDER over code_pushes_repo_idx (replicated 0084):
	// (?2, ?3] oldest first, first and after a cursor (?4, ?5), and below ?2
	// newest first, paged by the history's own pushesAfterSQL.
	pushesAboveSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position > ? AND position <= ? ORDER BY position, push_id LIMIT ?`
	pushesAboveAfterSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position >= ? AND position <= ? AND (position > ? OR push_id > ?)
		ORDER BY position, push_id LIMIT ?`
	pushesBelowSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?
		AND position < ? ORDER BY position DESC, push_id DESC LIMIT ?`
	// AN EXECUTOR'S PUSHES TO ONE REPOSITORY over code_pushes_repo_idx, after
	// a PLACE (?2 position, ?3 push id; the >= beside the OR keeps the range
	// seek). The window is weighed by covering seeks: the next ?4 pushes'
	// count and weight (runWeighSQL), each weight (runWeightsSQL), the last
	// one's place (runLastSQL). Then the RUN'S OWN in that window
	// (runPushesInSQL, ?5/?6 its last push inclusive), each row looked up to
	// test the run, which the index does not carry.
	runWeighSQL = `SELECT COUNT(*), COALESCE(SUM(row_bytes), 0) FROM (SELECT row_bytes FROM code_pushes
		WHERE repo_id = ?1 AND position >= ?2 AND (position > ?2 OR push_id > ?3)
		ORDER BY position, push_id LIMIT ?4)`
	runWeightsSQL = `SELECT row_bytes FROM code_pushes WHERE repo_id = ?1 AND position >= ?2
		AND (position > ?2 OR push_id > ?3) ORDER BY position, push_id LIMIT ?4`
	runLastSQL = `SELECT position, push_id FROM code_pushes WHERE repo_id = ?1 AND position >= ?2
		AND (position > ?2 OR push_id > ?3) ORDER BY position, push_id LIMIT 1 OFFSET ?4`
	runPushesInSQL = `SELECT ` + pushColumns + ` FROM code_pushes WHERE repo_id = ?1 AND run_id = ?2
		AND position >= ?3 AND (position > ?3 OR push_id > ?4) AND position <= ?5
		AND (position < ?5 OR push_id <= ?6) ORDER BY position, push_id`
	// THE RETENTION PASS'S TWO PHASES, each on its own partial index, the
	// predicate repeated verbatim, keyset on (deleted_at, id): the >= beside
	// the OR keeps the range seek a bare OR gives up.
	livesSQL = `SELECT id, name, status, managed_by FROM code_repos
		WHERE name > ? AND status IN ('importing', 'active', 'archived', 'deleted')
		ORDER BY name LIMIT ?`
	duePurgingSQL = `SELECT id, name, deleted_at FROM code_repos
		WHERE status = 'purging' AND deleted_at >= ? AND (deleted_at > ? OR id > ?)
		ORDER BY deleted_at, id LIMIT ?`
	dueDeletedSQL = `SELECT id, name, deleted_at FROM code_repos
		WHERE status = 'deleted' AND deleted_at <= ? AND deleted_at >= ? AND (deleted_at > ? OR id > ?)
		ORDER BY deleted_at, id LIMIT ?`
	purgeStateSQL = `SELECT
		(SELECT COUNT(*) FROM code_repos WHERE id = ?),
		(SELECT COUNT(*) FROM code_deletions WHERE repo_id = ?)`
	// THE COMPACTOR'S DUE LISTING over code_repos_packs_idx from its top, its
	// predicate repeated verbatim, the <= beside the OR keeping the range seek
	// (planner.go).
	compactionDueFirstSQL = `SELECT id, name, live_packs, live_objects, version FROM code_repos
		INDEXED BY code_repos_packs_idx
		WHERE status IN ('active', 'archived') AND live_packs >= ?
		ORDER BY live_packs DESC, id DESC LIMIT ?`
	compactionDueAfterSQL = `SELECT id, name, live_packs, live_objects, version FROM code_repos
		INDEXED BY code_repos_packs_idx
		WHERE status IN ('active', 'archived') AND live_packs >= ? AND live_packs <= ?
			AND (live_packs < ? OR id < ?)
		ORDER BY live_packs DESC, id DESC LIMIT ?`
	// THE PUBLISHER'S DUE LISTING over code_repos_unpublished_idx, its
	// predicate repeated verbatim, from the column's floor 0 (never published)
	// and after a cursor (planner.go). expunge_id = '' is a residual predicate
	// ([Reader.publicationDue]).
	publicationDueColumns = `id, name, live_packs, published_packs, live_objects, published_objects,
		last_push_at, published_at, publish_took_ms, version`
	publicationDueFirstSQL = `SELECT ` + publicationDueColumns + ` FROM code_repos
		INDEXED BY code_repos_unpublished_idx
		WHERE live_packs > published_packs AND status IN ('active', 'archived') AND expunge_id = ''
			AND published_at >= 0
		ORDER BY published_at, id LIMIT ?`
	publicationDueAfterSQL = `SELECT ` + publicationDueColumns + ` FROM code_repos
		INDEXED BY code_repos_unpublished_idx
		WHERE live_packs > published_packs AND status IN ('active', 'archived') AND expunge_id = ''
			AND published_at >= ? AND (published_at > ? OR id > ?)
		ORDER BY published_at, id LIMIT ?`
	// THE EXPUNGES OWED, by id over code_repos_expunging_idx — its partial
	// predicate repeated verbatim — after the cursor, "" from its start.
	expungeDueSQL = `SELECT ` + expungeDueColumns + ` FROM code_repos INDEXED BY code_repos_expunging_idx
		WHERE (expunge_id != '' OR expunge_request != '') AND status IN ('active', 'archived') AND id > ?
		ORDER BY id LIMIT ?`
	// THE EXPUNGES THE WEEKLY ARM ASKS ABOUT over code_repos_expunge_due_idx,
	// its predicate repeated verbatim, up to the instant; the open arm's are
	// left out.
	expungeWeeklySQL = `SELECT ` + expungeDueColumns + ` FROM code_repos INDEXED BY code_repos_expunge_due_idx
		WHERE status IN ('active', 'archived') AND expunge_scanned_at <= ? AND expunge_id = ''
			AND expunge_request = ''
		ORDER BY expunge_scanned_at, id LIMIT ?`
	expungeWeeklyAfterSQL = `SELECT ` + expungeDueColumns + ` FROM code_repos
		INDEXED BY code_repos_expunge_due_idx
		WHERE status IN ('active', 'archived') AND expunge_scanned_at <= ? AND expunge_scanned_at >= ?
			AND (expunge_scanned_at > ? OR id > ?) AND expunge_id = '' AND expunge_request = ''
		ORDER BY expunge_scanned_at, id LIMIT ?`
	expungeDueColumns = `id, name, expunge_id, expunge_request, expunge_request_position, expunged_at,
		expunge_scanned_at, live_packs, live_bytes, live_objects, version`
	// THE REPOSITORIES SEARCHED BY INDEX, by name over code_repos_search_idx
	// — its partial predicate repeated verbatim — after the cursor, "" from
	// its start.
	indexedSQL = `SELECT id, name FROM code_repos INDEXED BY code_repos_search_idx
		WHERE search = ? AND status IN ('active', 'archived') AND name > ? ORDER BY name LIMIT ?`
	// THE ACTIVE PROJECT REPOSITORIES' DEFAULT TIPS, by name over
	// code_repos_name_idx, the tip joined as a listing's is (reposHead).
	defaultTipsSQL = `SELECT r.id, r.name, r.visibility, h.target FROM code_repos r INDEXED BY code_repos_name_idx` +
		reposHead + ` WHERE r.name > ? AND r.status = 'active' AND r.kind = ? ORDER BY r.name LIMIT ?`
	// AND ONE OF THEM, by its id: no row for a repository that is not one.
	defaultTipSQL = `SELECT r.id, r.name, r.visibility, h.target FROM code_repos r` + reposHead + `
		WHERE r.id = ? AND r.status = 'active' AND r.kind = ?`
	// THE IMPORTS OWED, by id over code_repos_importing_idx — its partial
	// predicate repeated verbatim — after the cursor, "" from its start.
	importsDueSQL = `SELECT id, name, import_source, import_seq, version FROM code_repos
		INDEXED BY code_repos_importing_idx WHERE status = 'importing' AND id > ? ORDER BY id LIMIT ?`
	// THE PUSH MIRRORS, by id over code_repos_mirror_to_idx, live ones only:
	// an importing one has nothing to push, a deleted one is going.
	pushMirrorsSQL = `SELECT id, name, mirror_to FROM code_repos INDEXED BY code_repos_mirror_to_idx
		WHERE mirror_to != '' AND status IN ('active', 'archived') AND id > ? ORDER BY id LIMIT ?`
	// THE PULL MIRRORS, least recently synced first over
	// code_repos_mirrored_idx, keyset after (instant, id): the instant seeks
	// the index, and the id breaks a tie inside one instant.
	pullMirrorsSQL = `SELECT id, name, mirror_from, mirror_synced_at, version FROM code_repos
		INDEXED BY code_repos_mirrored_idx WHERE mirror_from != '' AND status = 'active'
			AND mirror_synced_at >= ? AND (mirror_synced_at > ? OR id > ?)
		ORDER BY mirror_synced_at, id LIMIT ?`
	// AN EXPUNGE'S TIPS: a repository's ref drops from an instant, in the
	// order of code_ref_drops' key, which leads with the instant — the first
	// page from the instant, a later one after the last drop read.
	expungeTipsSQL = `SELECT at, position, ref, old FROM code_ref_drops INDEXED BY ` + dropsKey + `
		WHERE repo_id = ? AND at >= ? ORDER BY at, position, ref, old LIMIT ?`
	expungeTipsAfterSQL = `SELECT at, position, ref, old FROM code_ref_drops INDEXED BY ` + dropsKey + `
		WHERE repo_id = ?1 AND at >= ?2
		AND (at > ?2 OR position > ?3 OR (position = ?3 AND (ref > ?4 OR (ref = ?4 AND old > ?5))))
		ORDER BY at, position, ref, old LIMIT ?6`
	// AN EXPUNGE'S IN-FLIGHT TIPS (M6-D2), each phase from its start and after
	// a place, in its index's order ([Reader.expungeTips]); a live change's
	// revisions are TWO seeks, never a join ([Reader.revisionTips]).
	liveChangeTipsSQL = `SELECT id, source_ref FROM code_changes INDEXED BY code_changes_open_idx
		WHERE repo_id = ?1 AND ` + liveChanges + ` ORDER BY source_ref LIMIT ?2`
	liveChangeTipsFromSQL = `SELECT id, source_ref FROM code_changes INDEXED BY code_changes_open_idx
		WHERE repo_id = ?1 AND source_ref >= ?2 AND ` + liveChanges + ` ORDER BY source_ref LIMIT ?3`
	liveChangeTipsAfterSQL = `SELECT id, source_ref FROM code_changes INDEXED BY code_changes_open_idx
		WHERE repo_id = ?1 AND source_ref > ?2 AND ` + liveChanges + ` ORDER BY source_ref LIMIT ?3`
	changeRevisionTipsSQL = `SELECT n, head_oid FROM code_revisions WHERE change_id = ?1 AND n > ?2
		ORDER BY n LIMIT ?3`
	queueTipsSQL = `SELECT target_ref, seq, head_oid FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ?1 AND state IN ('waiting', 'testing') ORDER BY target_ref, seq LIMIT ?2`
	queueTipsAfterSQL = `SELECT target_ref, seq, head_oid FROM code_queue INDEXED BY code_queue_live_idx
		WHERE repo_id = ?1 AND state IN ('waiting', 'testing') AND target_ref >= ?2
			AND (target_ref > ?2 OR seq > ?3)
		ORDER BY target_ref, seq LIMIT ?4`
	candidateTipsSQL = `SELECT target_ref, train, ordinal, tip_oid
		FROM code_candidates INDEXED BY code_candidates_train_idx
		WHERE repo_id = ?1 ORDER BY target_ref, train, ordinal LIMIT ?2`
	candidateTipsAfterSQL = `SELECT target_ref, train, ordinal, tip_oid
		FROM code_candidates INDEXED BY code_candidates_train_idx
		WHERE repo_id = ?1 AND target_ref >= ?2
			AND (target_ref > ?2 OR train > ?3 OR (train = ?3 AND ordinal > ?4))
		ORDER BY target_ref, train, ordinal LIMIT ?5`
	greenTipsSQL = `SELECT target_ref, commit_oid FROM code_green WHERE repo_id = ?1
		ORDER BY target_ref LIMIT ?2`
	greenTipsAfterSQL = `SELECT target_ref, commit_oid FROM code_green WHERE repo_id = ?1 AND target_ref > ?2
		ORDER BY target_ref LIMIT ?3`
	// A SNAPSHOT'S LAYERS PHASE: every part of a repository's chain, in its
	// key's order, joined to its layer (layersOfSQL) in Go.
	layerPartsOfSQL = `SELECT layer_id, part, object, hash, size FROM code_layer_parts
		WHERE repo_id = ? ORDER BY layer_id, part`
)
