package metrics

// servingInstruments is the instruments of what a node serves: authentication, alarms, the object store's
// bucket, artifacts, the Dashboard Assistant, presence and the service objectives.
func servingInstruments() []Instrument {
	return []Instrument{
		// ---- authentication -------------------------------------------
		{
			Name: AuthAttemptsFailed, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"method", "outcome"},
			Shows: "Every failed attempt to prove an identity, as it happens: " +
				"a sign-in, a step-up, an invitation or password reset " +
				"link that did not prove itself, or a bearer presented " +
				"to a route that " +
				"needs one and refused — a credential an unguarded route " +
				"verifies itself, such as a webhook relay's own JWT, is " +
				"never counted (`method`), and whether the attempt was " +
				"verified and wrong or answered 429 by the throttle's " +
				"curve (`outcome`). It is " +
				"the ONLY per-attempt record there is, by design: an " +
				"unauthenticated caller authors this rate, so the event log " +
				"gets one `iam_login_failures` row per node per minute, " +
				"counting each source, and the rate itself lives here, where " +
				"a guessing run costs a counter increment rather than a row " +
				"in every backup.",
		},
		// ---- alarms ---------------------------------------------------
		{
			Name: AlarmActive, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"kind"},
			Shows: "Whether each named alarm is firing right now, 0 or 1 — " +
				"for one that stands on several logs, whether it stands on " +
				"any; its `domains` and its log lines say which. It is the same table " +
				"the operator record renders and the CLI exits non-zero on, " +
				"so a collector and a person see one answer.",
		},
		{
			Name: AlarmTransitions, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"change"},
			Shows: "Alarm transitions this node published for the seats whose " +
				"role carries `health_tools` to be woken by, once the broker " +
				"took each, `fired` or `cleared`; a restart says only what " +
				"changed while the node was down. A gauge flipping " +
				"while this stands still is a node whose wakes are not getting " +
				"out: its `alarm_transition_unpublished` line names the error.",
		},
		{
			Name: AlarmTransitionsDropped, Kind: KindCounter, Unit: UnitCount,
			Shows: "Alarm transitions this node dropped unpublished: its outbox " +
				"holds 2048 while the broker refuses them and drops the oldest " +
				"past that, so no seat is woken by those. Any rise is a broker " +
				"this node could not publish to for a long while; " +
				"`GET /fleet` and the seats' health tools still read what is " +
				"firing now.",
		},
		// ---- the object store's bucket ----------------------------------
		{
			Name: ObjectsBucketBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "How many bytes the fleet's own object bucket on the broker " +
				"holds (OBJ_crewlet_files: the company's files, their " +
				"metadata and any upload in flight), as the object " +
				"collector's duty holder read it; only that node reports it, " +
				"and only on the nats backend. Read against " +
				"`crewlet.objects.bucket.max_bytes`: a full bucket refuses " +
				"every upload, and `objects_bucket_near_ceiling` fires at " +
				"four fifths.",
		},
		{
			Name: ObjectsBucketMaxBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "The byte reservation the broker holds the object bucket " +
				"to (store.objects.nats_max_bytes, or the share the node " +
				"that created the bucket derived): a data node raises it to " +
				"a larger store.objects.nats_max_bytes at boot, and nothing " +
				"lowers it.",
		},
		// ---- artifact uploads and the publish pipeline -------------------
		{
			Name: ArtifactsUploadBytes, Kind: KindCounter, Unit: UnitBytes,
			Attributes: []string{"transport"},
			Shows: "Bytes of artifact files that reached the object store, by " +
				"how they got there: streamed through this node (`engine`, " +
				"which counts what the pipeline writes itself: a linted page, " +
				"an edit's result, a copied CDN file) or put straight to an " +
				"S3 bucket on a presigned URL and read back here (`direct`). " +
				"A fleet whose engine share stays high on s3 is streaming " +
				"uploads its bucket could have taken.",
		},
		{
			Name: ArtifactsUploadReservedBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "Bytes the company's upload reservations hold, as this node " +
				"last wrote the reservation ledger: bytes in the object store, " +
				"or on their way to it, that no record names yet. Read against " +
				"`artifacts.limits.pending_bytes` times four, the company's " +
				"bound: a value that stays near it is clients abandoning " +
				"uploads, each held until the object collector could have " +
				"deleted its bytes.",
		},
		{
			Name: ArtifactsPublishDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long one create, publish or asset finalize took through " +
				"the pipeline (read back, edits, lint, CDN copies, the " +
				"domain's decide), by `outcome`: `published`, `refused` (the " +
				"request was judged and turned down) or `failed` (no outcome " +
				"could be established, which is a store or a broker not " +
				"answering). The publish latency objective is read from it.",
		},
		{
			Name: ArtifactsPublishRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Uploads and publishes refused, by `reason` from a closed set " +
				"(internal/artifactupload.Refusals): a client sending the " +
				"wrong bytes (`upload_mismatch`), a page breaking the policy " +
				"(`policy_refused`), a company out of quota (`quota_exceeded`) " +
				"or reservations (`uploads_outstanding`), an object store with " +
				"no room left (`store_full`, the operator's to make), an " +
				"agent's turn past its guardrails (`turn_quota`), a moved head " +
				"(`version_moved`) and the rest. Each reason is a different " +
				"person to call.",
		},
		// ---- the artifact origin ----------------------------------------
		{
			Name: ArtifactsServeRequests, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"dest", "status"},
			Shows: "Every request the artifact origin (the api.artifacts " +
				"listener) answered, by what it was (`dest`: `document`, a " +
				"page decided again on every request; `subresource`, a file " +
				"a page loads; `runtime` and `kit`, the embedded public " +
				"files; `brand`, the company's adopted brand's files) and by " +
				"status class (`status`: `2xx` to `5xx`, or `abandoned`: the " +
				"client went away before it was answered, a frame torn down " +
				"or a browser gone, which is no fault of the node's). A " +
				"rise in `document` `5xx` is viewers whose frames this node " +
				"would not serve: a copy of the artifacts or identity log it " +
				"could not vouch for, or an object store that did not answer. " +
				"The artifact id is never an attribute; per-artifact detail " +
				"is in the log lines.",
		},
		{
			Name: ArtifactsServeDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"dest"},
			Shows: "How long the artifact origin took to answer, by `dest`, " +
				"the whole response included. A document's tail is the time " +
				"to first byte a viewer waits on before a frame can render, " +
				"which the service objective for viewing reads; a slow " +
				"subresource tail with fast documents is the object store.",
		},
		{
			Name: ArtifactsServeBytes, Kind: KindCounter, Unit: UnitBytes,
			Shows: "Body bytes the artifact origin sent, compressed where " +
				"they were sent compressed: what artifact viewing costs this " +
				"node in egress, per node and never per artifact.",
		},
		{
			Name: ArtifactsTicketRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "View tickets the artifact origin refused, by `reason`: " +
				"`invalid` (forged, malformed, or signed under a key this " +
				"node does not hold), `expired` (a frame past its hour, " +
				"which the dashboard re-mints), `host` (a ticket presented " +
				"on a host other than its artifact's), `navigation` (a " +
				"ticket URL opened in a tab of its own), `revoked` (the " +
				"share, the credential, the artifact or the version is " +
				"gone) and `stale` (this node could not vouch for what it " +
				"would decide from, and answered 503). `invalid` above zero " +
				"sustained is somebody presenting tickets this fleet never " +
				"minted, or a keyring rotated past a key still in use.",
		},
		{
			Name: ArtifactsAuthzPeerChecks, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Authorizations the artifact origin decided through " +
				"another data node because its own artifacts copy was out of " +
				"service or lagging, by `outcome` (`served`, `refused`, " +
				"`unavailable`). Any at all is a node whose copy is not " +
				"answering; `unavailable` is a viewer refused because no " +
				"data node could decide for them.",
		},
		// ---- the artifact datastore ---------------------------------------
		{
			Name: ArtifactsDataWriteDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"verdict"},
			Shows: "How long a datastore write takes from the request to its verdict " +
				"read back from this node's rows, by verdict (applied, conflict, " +
				"invalid, forbidden, quota, gone, or pending where this node had not " +
				"applied it yet, rate_limited and refused_early before anything was " +
				"appended). The datastore op objective is a p95 of 250 ms; a rise in " +
				"pending reads as this node's applier falling behind.",
		},
		{
			Name: ArtifactsDataWrites, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"verdict"},
			Shows: "Datastore writes by verdict. A climb in conflict is people editing " +
				"the same rows over each other; in forbidden, a page or an agent writing " +
				"what its collection's rules refuse; in rate_limited, one client " +
				"spending this node's write budget.",
		},
		{
			Name: ArtifactsDataFootprintBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "The company's datastore footprint in real bytes held on every data " +
				"node (each document row with its index and field-index entries, and hot " +
				"history, as the applier meters what the rows cost on disk), sampled " +
				"after each committed batch. Read against `artifacts.limits.data_bytes`: " +
				"a store or a company at its limit refuses every write that grows it.",
		},
		// ---- the Dashboard Assistant -------------------------------------
		{
			Name: AssistantTurns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Ask Crewlet turn segments this node ran, by how each ended " +
				"(`completed`, `waiting_for_approval`, `stopped`, `failed`, " +
				"`interrupted`, `budget_exhausted`). A climb in `failed` is a model " +
				"chain or a tool route failing people; in `budget_exhausted`, the " +
				"company's windows refusing Ask Crewlet.",
		},
		{
			Name: AssistantTurnDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long an Ask Crewlet turn segment runs from the question to its " +
				"end, by outcome. A three-step read is held to 15 s at the median; a " +
				"distribution reaching the turn's own timeout is a model or a harness too " +
				"slow for an interactive drawer.",
		},
		{
			Name: AssistantFirstOutputLatency, Kind: KindHistogram, Unit: UnitMilliseconds,
			Shows: "How long a person waits from asking to the first step or text the " +
				"drawer shows. Held to 4 s at the median; a rise is a harness's cold start " +
				"or its slots queued behind other turns.",
		},
		{
			Name: AssistantSteps, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"state"},
			Shows: "Ask Crewlet tool steps by how each ended (`done`, `failed`, " +
				"`refused`, `stopped`, `waiting`). A climb in `refused` is the model " +
				"asking for what its people may not read; in `failed`, a route or the " +
				"dispatcher breaking under Ask Crewlet.",
		},
		{
			Name: AssistantBriefs, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Brief answers Ask Crewlet's model wrote, by what the engine did with " +
				"each (`written`, `refused`). A `refused` answer is written again once; a " +
				"climb in it is a model that cannot keep to the brief's shape.",
		},
		{
			Name: ToolPolicyRefusalLoops, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"tool"},
			Shows: "Runs of policy refusals a seat's or a worker's write tool answered in a " +
				"row on one target until it reached the limit (internal/tools." +
				"PolicyRefusalLimit), each counted once when it did, by `tool`: the model was " +
				"told to stop retrying and raise a blocker. A climb names a tool whose " +
				"contract the model cannot see or cannot meet.",
		},
		{
			Name: ToolArgsOutsideSchema, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"tool"},
			Shows: "Calls a first-party tool answered successfully although their arguments " +
				"were outside the schema it advertises (an argument it does not take, a value " +
				"outside its closed set or its bounds), by `tool`. The model was told on the " +
				"answer. A tool that keeps climbing here is one whose schema and reading " +
				"disagree, and the drift a later refusal at the surface would have to fix first.",
		},
		{
			Name: AssistantBriefRowsDropped, Kind: KindCounter, Unit: UnitCount,
			Shows: "Records a brief cited (a row, or its recommendation's press) that the " +
				"engine left out because the record was not in what its turn read, or " +
				"the person may not open it. A steady climb is a model citing records " +
				"it did not read.",
		},
		{
			Name: AssistantCardsShownAsNotice, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"type"},
			Shows: "Stored Ask Crewlet cards read back as a plain notice because this build's " +
				"contract refuses their shape, by card `type`. Expected for briefs recorded " +
				"before the brief's current shape, and during a rolling upgrade when an older " +
				"node reads a newer node's card. A climb outside both is a card written past " +
				"its own validation.",
		},
		{
			Name: AssistantStreamsActive, Kind: KindGauge, Unit: UnitCount,
			Shows: "Ask Crewlet live streams this node is serving. Each holds a " +
				"connection and a subscription to one turn's frame log; a count that " +
				"never falls is streams whose clients went away without the " +
				"connection closing.",
		},
		// ---- presence and the artifact rooms' pushes ----------------------
		{
			Name: PresenceFrames, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"direction"},
			Shows: "Presence frames this node handled, by `direction`: `client` (a " +
				"socket's presence frame taken), `peer` (a sealed frame from another " +
				"node admitted) and `push` (a room's coalesced fan-out to this node's " +
				"sockets). Presence is told, never stored, so this is the whole of " +
				"its cost: a `peer` climb with flat `client` is a busy room held on " +
				"other nodes, and `push` is bounded at five a second per room.",
		},
		{
			Name: PresenceRooms, Kind: KindGauge, Unit: UnitCount,
			Shows: "Artifact rooms this node holds a member of, a socket's or an agent's. " +
				"Each costs a map in memory and, while one of this node's sockets is in " +
				"it, one listen on the broker.",
		},
		{
			Name: ArtifactsPushLag, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"kind"},
			Shows: "How long after the broker stored a committed record its push was " +
				"handed to this node's sockets, by `kind`: `changed` for an artifact's " +
				"movement (`artifact_changed`) and `data` for a datastore's documents " +
				"(`artifact_data`) — the applier's lag plus the fan-out, measured " +
				"whether or not a socket is in the room. A change is held to reach " +
				"the sockets watching it within a second (the `change_visibility` " +
				"objective, which counts only the pushes some socket was handed).",
		},
		// ---- service objectives -------------------------------------------
		{
			Name: ArtifactsReadyDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long a frame took from its view ticket being minted to the " +
				"page's bridge hello, as the viewer's browser reported it through " +
				"`POST /artifacts/ready`, by `outcome` (`ready`, or `failed` for a frame " +
				"that never said hello). The only artifacts instrument that sees an " +
				"outage keeping requests from reaching the engine at all (the artifact " +
				"domain's DNS or certificate, an edge, a corporate filter): every " +
				"server-side instrument reads such an outage as a quiet node. The " +
				"`artifact_ready` objective holds 99.5% of opens within 3 s.",
		},
		{
			Name: ObjectivesEvents, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"objective", "outcome"},
			Shows: "Every service objective's events, by `objective` (the closed set " +
				"in docs/reference/service-objectives.md) and `outcome` (`good`: " +
				"answered without fault on the engine's side within the objective's " +
				"bound; `bad`: anything else, but an event whose caller went away " +
				"inside the bound is not counted). `bad` over all, per objective, against " +
				"one minus its target is the error budget's burn rate, which the " +
				"`objective_burn` alarm judges per node over an hour and five minutes " +
				"and over six hours and thirty minutes; summed across the fleet it is " +
				"the company's.",
		},
	}
}
