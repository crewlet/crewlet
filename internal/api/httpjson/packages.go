package httpjson

// goPackages is the name of every Go package in this module that has non-test
// sources, which is what a leading "name: " in an error's text is: the package
// prefix Go errors carry ("statelog: node-4 may not be evicted"). [Sentence]
// leaves such a prefix lowercase, because capitalising it names a package that
// does not exist, and stripping it is the source's job rather than the
// boundary's.
//
// A TABLE, HELD TO THE TREE: TestThePackagePrefixesAreTheModulesPackages
// reads every package clause in the module and fails, naming the difference,
// when a package is added, renamed or removed without this table following.
var goPackages = map[string]bool{
	"a2a": true, "a2atest": true, "adr": true, "api": true,
	"argval": true, "toolargs": true, "artifactdata": true, "artifactdataapi": true, "artifacts": true,
	"artifactsapi": true, "artifactsdomain": true, "artifactserve": true,
	"artifactspike": true, "artifactupload": true,
	"approval": true, "approvaltest": true, "assistant": true, "assistantapi": true, "assistantdomain": true,
	"assistanttest": true, "cards": true, "catalog": true, "evals": true, "exec": true,
	"live": true, "proposal": true, "tabular": true, "wire": true,
	"atlassian": true, "atlassianhost": true, "auth": true, "authapi": true,
	"authevents": true, "authz": true, "auxspend": true, "backoff": true,
	"backup": true, "brandformat": true, "brokertest": true, "builtin": true, "chain": true, "changefeed": true,
	"canon": true, "canvasformat": true, "caveat": true, "caveattest": true, "edits": true,
	"cliagent": true, "clientsource": true,
	"cliprofile": true, "codingagent": true, "colleague": true, "collect": true,
	"compact": true, "config": true, "configapi": true, "configplane": true, "crewlet": true,
	"confluence": true, "coord": true,
	"coordtest": true, "costlimit": true, "credential": true, "cron": true,
	"dashspec": true, "datadog": true, "datasetsapi": true, "decision": true,
	"docindex": true, "doclet": true, "docsgate": true, "e2e": true, "embeddings": true,
	"engine": true, "envfile": true, "envref": true, "estate": true,
	"eventfan": true, "eventfantest": true, "events": true, "execstate": true,
	"extension": true, "externaltest": true, "fleetsecrets": true, "flock": true,
	"foldername": true, "github": true, "gitlab": true, "hostbox": true,
	"httpjson": true, "httprelay": true, "httpx": true, "httpxtest": true, "iam": true,
	"iamapi": true, "iamdomain": true, "inbox": true, "integration": true,
	"integrationtest": true, "jetstream": true, "jetstreamtest": true,
	"jira": true, "jsapi": true, "jsinflight": true, "jsoncarry": true, "jsonform": true,
	"jsprovision": true, "jwks": true, "knowledge": true, "kv": true,
	"learning": true, "ledger": true, "ledgerfit": true, "ledgerstore": true,
	"livestate": true, "llm": true, "llmtest": true, "logging": true, "maintenance": true,
	"lint": true, "manifest": true, "mcprelay": true, "mediatype": true,
	"mattermost": true, "mcp": true, "mcpbridge": true, "memobj": true, "modelnote": true,
	"memory": true, "memread": true, "memsync": true, "metrics": true, "migrate": true,
	"natsobj": true, "node": true, "notify": true, "objstore": true,
	"objstoretest": true, "observe": true, "operator": true,
	"objectives": true, "query": true,
	"opkey": true, "org": true, "orgchart": true, "paced": true, "pagepolicy": true,
	"period": true, "phase": true, "placement": true,
	"prefetch": true, "procgroup": true, "procgrouptest": true, "prompts": true,
	"presence": true, "provenance": true,
	"provision": true, "queries": true, "queue": true, "queuetest": true,
	"redact": true, "references": true, "runner": true, "runtoken": true,
	"s3fake": true, "s3obj": true, "sandbox": true, "sandboxtest": true,
	"schedule": true, "scheduletest": true, "search": true, "seat": true,
	"secrets": true, "secretsapi": true, "session": true, "setup": true,
	"setupapi": true, "setuptest": true, "signedask": true, "skills": true,
	"skillsync": true, "slack": true, "slideformat": true, "solo": true,
	"sourcetree": true, "sqlledger": true,
	"statelog": true, "statelogtest": true, "static": true, "steer": true,
	"steersourcetest": true, "store": true, "storetest": true, "stream": true, "structured": true,
	"subagent": true, "svgsafe": true, "textcut": true, "textindex": true, "tokens": true,
	"toolloop": true, "tools": true, "toolsession": true, "toolsessiontest": true,
	"topics": true, "tracing": true, "turn": true, "turnctx": true, "types": true,
	"ticket": true, "trashreap": true,
	"usage": true, "version": true, "webhooks": true, "whsec": true,
	"workapi": true, "workkey": true, "worklet": true,
}
