package codescan

import (
	"regexp"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/atlas"
)

// THE CLOSED TABLES the scan reads a name against. A name off a table says
// nothing: the scan never guesses a vendor or an engine from a similar name.

// engine is what a well-known image or chart runs as: a database or a queue,
// and the atlas's word for its engine.
type engine struct {
	kind atlas.Kind
	word string
}

// engines is every image or chart name (its last path segment, lowercase)
// the scan reads as a database or a queue.
var engines = map[string]engine{
	"postgres": {atlas.KindDatastore, "postgres"}, "postgresql": {atlas.KindDatastore, "postgres"},
	"postgis": {atlas.KindDatastore, "postgres"}, "timescaledb": {atlas.KindDatastore, "postgres"},
	"pgvector": {atlas.KindDatastore, "postgres"},
	"mysql":    {atlas.KindDatastore, "mysql"}, "mariadb": {atlas.KindDatastore, "mariadb"},
	"redis": {atlas.KindDatastore, "redis"}, "valkey": {atlas.KindDatastore, "valkey"},
	"mongo": {atlas.KindDatastore, "mongodb"}, "mongodb": {atlas.KindDatastore, "mongodb"},
	"elasticsearch": {atlas.KindDatastore, "elasticsearch"}, "opensearch": {atlas.KindDatastore, "opensearch"},
	"clickhouse": {atlas.KindDatastore, "clickhouse"}, "clickhouse-server": {atlas.KindDatastore, "clickhouse"},
	"nats": {atlas.KindQueue, "nats"}, "kafka": {atlas.KindQueue, "kafka"}, "cp-kafka": {atlas.KindQueue, "kafka"},
	"redpanda": {atlas.KindQueue, "kafka"}, "rabbitmq": {atlas.KindQueue, "rabbitmq"},
}

// operated is what a resource an operator reconciles asks for: the API group
// its apiVersion starts with, its kind, and what the atlas calls it. A bucket
// has no engine.
type operated struct {
	group, kind string
	as          engine
}

var operators = []operated{
	{"postgresql.cnpg.io", "Cluster", engine{atlas.KindDatastore, "postgres"}},
	{"acid.zalan.do", "postgresql", engine{atlas.KindDatastore, "postgres"}},
	{"mysql.oracle.com", "InnoDBCluster", engine{atlas.KindDatastore, "mysql"}},
	{"k8s.mariadb.com", "MariaDB", engine{atlas.KindDatastore, "mariadb"}},
	{"mongodbcommunity.mongodb.com", "MongoDBCommunity", engine{atlas.KindDatastore, "mongodb"}},
	{"redis.redis.opstreelabs.in", "Redis", engine{atlas.KindDatastore, "redis"}},
	{"redis.redis.opstreelabs.in", "RedisCluster", engine{atlas.KindDatastore, "redis"}},
	{"elasticsearch.k8s.elastic.co", "Elasticsearch", engine{atlas.KindDatastore, "elasticsearch"}},
	{"opensearch.opster.io", "OpenSearchCluster", engine{atlas.KindDatastore, "opensearch"}},
	{"clickhouse.altinity.com", "ClickHouseInstallation", engine{atlas.KindDatastore, "clickhouse"}},
	{"kafka.strimzi.io", "Kafka", engine{atlas.KindQueue, "kafka"}},
	{"kafka.strimzi.io", "KafkaTopic", engine{atlas.KindQueue, "kafka"}},
	{"rabbitmq.com", "RabbitmqCluster", engine{atlas.KindQueue, "rabbitmq"}},
	{"objectbucket.io", "ObjectBucketClaim", engine{kind: atlas.KindBucket}},
	{"s3.aws.upbound.io", "Bucket", engine{kind: atlas.KindBucket}},
	{"storage.gcp.upbound.io", "Bucket", engine{kind: atlas.KindBucket}},
}

// workloads is every Kubernetes kind that runs a service, by its API group.
var workloads = map[string]string{
	"Deployment": "apps", "StatefulSet": "apps", "DaemonSet": "apps", "CronJob": "batch",
}

// vendor is one vendor the scan names: one the engine knows, or another by
// its slug and the name a reader knows it by, with what it does where the
// table can say.
type vendor struct {
	known        atlas.Vendor
	slug, name   string
	capabilities []atlas.Capability
}

func known(v atlas.Vendor, capabilities ...atlas.Capability) vendor {
	return vendor{known: v, capabilities: capabilities}
}

func other(slug, name string, capabilities ...atlas.Capability) vendor {
	return vendor{known: atlas.VendorOther, slug: slug, name: name, capabilities: capabilities}
}

// The vendors the tables name more than once.
var (
	vendorAWS          = known(atlas.VendorAWS)
	vendorGoogleCloud  = known(atlas.VendorGoogleCloud)
	vendorCloudflare   = known(atlas.VendorCloudflare)
	vendorDigitalOcean = known(atlas.VendorDigitalOcean)
	vendorScaleway     = known(atlas.VendorScaleway)
	vendorDatadog      = known(atlas.VendorDatadog, atlas.CapabilityMonitoring)
	vendorGitHub       = known(atlas.VendorGitHub)
	vendorGitLab       = known(atlas.VendorGitLab)
	vendorSlack        = known(atlas.VendorSlack, atlas.CapabilityChat)
	vendorAnthropic    = known(atlas.VendorAnthropic, atlas.CapabilityModelAPI)
	vendorOpenAI       = known(atlas.VendorOpenAI, atlas.CapabilityModelAPI)
	vendorStripe       = other("stripe", "Stripe")
	vendorSentry       = other("sentry", "Sentry", atlas.CapabilityMonitoring)
	vendorTwilio       = other("twilio", "Twilio")
	vendorSendGrid     = other("sendgrid", "SendGrid", atlas.CapabilityEmail)
	vendorMailgun      = other("mailgun", "Mailgun", atlas.CapabilityEmail)
	vendorPostmark     = other("postmark", "Postmark", atlas.CapabilityEmail)
	vendorResend       = other("resend", "Resend", atlas.CapabilityEmail)
	vendorAuth0        = other("auth0", "Auth0", atlas.CapabilityIdentity)
	vendorSupabase     = other("supabase", "Supabase", atlas.CapabilityDatabase)
	vendorFirebase     = other("firebase", "Firebase")
	vendorAlgolia      = other("algolia", "Algolia")
	vendorLaunchDarkly = other("launchdarkly", "LaunchDarkly")
	vendorPagerDuty    = other("pagerduty", "PagerDuty", atlas.CapabilityAlerting)
	vendorNewRelic     = other("newrelic", "New Relic", atlas.CapabilityMonitoring)
	vendorPostHog      = other("posthog", "PostHog")
	vendorAzure        = other("azure", "Microsoft Azure")
	vendorHetzner      = other("hetzner", "Hetzner", atlas.CapabilityCompute)
	vendorVercel       = other("vercel", "Vercel", atlas.CapabilityAppDelivery)
	vendorHeroku       = other("heroku", "Heroku", atlas.CapabilityAppDelivery)
	vendorFly          = other("fly", "Fly.io", atlas.CapabilityAppDelivery)
	vendorRender       = other("render", "Render", atlas.CapabilityAppDelivery)
	vendorMongoAtlas   = other("mongodb-atlas", "MongoDB Atlas", atlas.CapabilityDatabase)
)

// ecosystem is which registry a dependency file names its packages in. A
// package name means a vendor only within its own registry.
type ecosystem string

const (
	npm      ecosystem = "npm"
	golang   ecosystem = "go"
	pypi     ecosystem = "pypi"
	rubygems ecosystem = "rubygems"
	crates   ecosystem = "crates"
)

// sdk is one row of the SDK table: a package's exact name, or with prefix set
// every package whose name starts with it (a scope, a module path).
type sdk struct {
	name   string
	prefix bool
	vendor vendor
}

// sdks is every package the scan reads as a vendor's SDK, by registry.
var sdks = map[ecosystem][]sdk{
	npm: {
		{"@aws-sdk/", true, vendorAWS}, {"aws-sdk", false, vendorAWS},
		{"@google-cloud/", true, vendorGoogleCloud}, {"cloudflare", false, vendorCloudflare},
		{"dd-trace", false, vendorDatadog}, {"@datadog/", true, vendorDatadog},
		{"@octokit/", true, vendorGitHub}, {"octokit", false, vendorGitHub},
		{"@gitbeaker/", true, vendorGitLab}, {"@slack/", true, vendorSlack},
		{"@anthropic-ai/sdk", false, vendorAnthropic}, {"openai", false, vendorOpenAI},
		{"stripe", false, vendorStripe}, {"@stripe/", true, vendorStripe},
		{"@sentry/", true, vendorSentry}, {"twilio", false, vendorTwilio},
		{"@sendgrid/", true, vendorSendGrid}, {"mailgun.js", false, vendorMailgun},
		{"postmark", false, vendorPostmark}, {"resend", false, vendorResend},
		{"auth0", false, vendorAuth0}, {"@auth0/", true, vendorAuth0},
		{"@supabase/", true, vendorSupabase}, {"firebase", false, vendorFirebase},
		{"firebase-admin", false, vendorFirebase}, {"algoliasearch", false, vendorAlgolia},
		{"@launchdarkly/", true, vendorLaunchDarkly}, {"launchdarkly-node-server-sdk", false, vendorLaunchDarkly},
		{"newrelic", false, vendorNewRelic}, {"posthog-js", false, vendorPostHog},
		{"posthog-node", false, vendorPostHog}, {"@azure/", true, vendorAzure},
		{"@vercel/", true, vendorVercel},
	},
	golang: {
		{"github.com/aws/aws-sdk-go", true, vendorAWS}, {"cloud.google.com/go", true, vendorGoogleCloud},
		{"github.com/cloudflare/cloudflare-go", true, vendorCloudflare},
		{"github.com/DataDog/", true, vendorDatadog}, {"gopkg.in/DataDog/", true, vendorDatadog},
		{"github.com/digitalocean/godo", false, vendorDigitalOcean},
		{"github.com/scaleway/scaleway-sdk-go", false, vendorScaleway},
		{"github.com/google/go-github/", true, vendorGitHub},
		{"gitlab.com/gitlab-org/api/client-go", false, vendorGitLab},
		{"github.com/slack-go/slack", false, vendorSlack},
		{"github.com/anthropics/anthropic-sdk-go", false, vendorAnthropic},
		{"github.com/openai/openai-go", true, vendorOpenAI},
		{"github.com/stripe/stripe-go/", true, vendorStripe},
		{"github.com/getsentry/sentry-go", false, vendorSentry},
		{"github.com/twilio/twilio-go", false, vendorTwilio},
		{"github.com/sendgrid/sendgrid-go", false, vendorSendGrid},
		{"github.com/mailgun/mailgun-go/", true, vendorMailgun},
		{"github.com/auth0/go-auth0", false, vendorAuth0},
		{"firebase.google.com/go", true, vendorFirebase},
		{"github.com/algolia/algoliasearch-client-go/", true, vendorAlgolia},
		{"github.com/launchdarkly/go-server-sdk/", true, vendorLaunchDarkly},
		{"github.com/PagerDuty/go-pagerduty", false, vendorPagerDuty},
		{"github.com/newrelic/go-agent/", true, vendorNewRelic},
		{"github.com/posthog/posthog-go", false, vendorPostHog},
		{"github.com/Azure/azure-sdk-for-go/", true, vendorAzure},
		{"github.com/hetznercloud/hcloud-go/", true, vendorHetzner},
	},
	pypi: {
		{"boto3", false, vendorAWS}, {"botocore", false, vendorAWS}, {"google-cloud-", true, vendorGoogleCloud},
		{"cloudflare", false, vendorCloudflare}, {"ddtrace", false, vendorDatadog}, {"datadog", false, vendorDatadog},
		{"pydo", false, vendorDigitalOcean}, {"scaleway", false, vendorScaleway}, {"pygithub", false, vendorGitHub},
		{"python-gitlab", false, vendorGitLab}, {"slack-sdk", false, vendorSlack}, {"slack-bolt", false, vendorSlack},
		{"anthropic", false, vendorAnthropic}, {"openai", false, vendorOpenAI}, {"stripe", false, vendorStripe},
		{"sentry-sdk", false, vendorSentry}, {"twilio", false, vendorTwilio}, {"sendgrid", false, vendorSendGrid},
		{"resend", false, vendorResend}, {"auth0-python", false, vendorAuth0}, {"supabase", false, vendorSupabase},
		{"firebase-admin", false, vendorFirebase}, {"algoliasearch", false, vendorAlgolia},
		{"launchdarkly-server-sdk", false, vendorLaunchDarkly}, {"pdpyras", false, vendorPagerDuty},
		{"newrelic", false, vendorNewRelic}, {"posthog", false, vendorPostHog}, {"azure-", true, vendorAzure},
		{"hcloud", false, vendorHetzner},
	},
	rubygems: {
		{"aws-sdk", true, vendorAWS}, {"google-cloud-", true, vendorGoogleCloud}, {"ddtrace", false, vendorDatadog},
		{"datadog", false, vendorDatadog}, {"droplet_kit", false, vendorDigitalOcean}, {"octokit", false, vendorGitHub},
		{"gitlab", false, vendorGitLab}, {"slack-ruby-client", false, vendorSlack},
		{"anthropic", false, vendorAnthropic}, {"ruby-openai", false, vendorOpenAI}, {"stripe", false, vendorStripe},
		{"sentry-ruby", false, vendorSentry}, {"sentry-rails", false, vendorSentry}, {"twilio-ruby", false, vendorTwilio},
		{"sendgrid-ruby", false, vendorSendGrid}, {"mailgun-ruby", false, vendorMailgun},
		{"postmark", false, vendorPostmark}, {"resend", false, vendorResend}, {"auth0", false, vendorAuth0},
		{"algolia", false, vendorAlgolia}, {"launchdarkly-server-sdk", false, vendorLaunchDarkly},
		{"newrelic_rpm", false, vendorNewRelic}, {"posthog-ruby", false, vendorPostHog},
	},
	crates: {
		{"aws-sdk-", true, vendorAWS}, {"aws-config", false, vendorAWS}, {"google-cloud-", true, vendorGoogleCloud},
		{"cloudflare", false, vendorCloudflare}, {"octocrab", false, vendorGitHub}, {"gitlab", false, vendorGitLab},
		{"async-openai", false, vendorOpenAI}, {"async-stripe", false, vendorStripe}, {"sentry", false, vendorSentry},
		{"azure_core", false, vendorAzure},
	},
}

// vendorOf is the vendor whose SDK the package name is in its registry, and
// false for every package off the table. A Python name is compared the way
// its registry normalises one: lowercase, with runs of . _ - as one hyphen.
func vendorOf(eco ecosystem, name string) (vendor, bool) {
	if eco == pypi {
		name = pypiName(name)
	}
	for _, row := range sdks[eco] {
		if name == row.name || (row.prefix && strings.HasPrefix(name, row.name)) {
			return row.vendor, true
		}
	}
	return vendor{}, false
}

var pypiSeparators = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[-_.]+`) })

func pypiName(name string) string {
	return pypiSeparators().ReplaceAllString(strings.ToLower(name), "-")
}

// providers is every Terraform or OpenTofu provider the scan reads as a
// vendor, by the provider's own name. A provider off the table (random,
// null, kubernetes) names no vendor.
var providers = map[string]vendor{
	"aws": vendorAWS, "awscc": vendorAWS, "google": vendorGoogleCloud, "google-beta": vendorGoogleCloud,
	"cloudflare": vendorCloudflare, "digitalocean": vendorDigitalOcean, "scaleway": vendorScaleway,
	"datadog": vendorDatadog, "github": vendorGitHub, "gitlab": vendorGitLab,
	"azurerm": vendorAzure, "hcloud": vendorHetzner, "vercel": vendorVercel, "heroku": vendorHeroku,
	"fly": vendorFly, "render": vendorRender, "mongodbatlas": vendorMongoAtlas, "sentry": vendorSentry,
	"pagerduty": vendorPagerDuty, "newrelic": vendorNewRelic, "stripe": vendorStripe, "auth0": vendorAuth0,
	"launchdarkly": vendorLaunchDarkly, "supabase": vendorSupabase,
}

// resourceCapabilities is what a resource type says its provider's account
// does for the company, by the type's prefix after the provider's name. The
// first matching row wins, so a longer prefix is listed before a shorter one.
var resourceCapabilities = map[string][]struct {
	prefix     string
	capability atlas.Capability
}{
	"aws": {
		{"s3_", atlas.CapabilityObjectStorage}, {"db_", atlas.CapabilityDatabase}, {"rds_", atlas.CapabilityDatabase},
		{"dynamodb_", atlas.CapabilityDatabase}, {"elasticache_", atlas.CapabilityDatabase},
		{"instance", atlas.CapabilityCompute}, {"ecs_", atlas.CapabilityCompute}, {"eks_", atlas.CapabilityCompute},
		{"lambda_", atlas.CapabilityCompute}, {"route53_", atlas.CapabilityDNS},
		{"cloudfront_", atlas.CapabilityCDN}, {"ses_", atlas.CapabilityEmail}, {"sesv2_", atlas.CapabilityEmail},
		{"cloudwatch_", atlas.CapabilityMonitoring}, {"cognito_", atlas.CapabilityIdentity},
	},
	"google": {
		{"storage_bucket", atlas.CapabilityObjectStorage}, {"sql_", atlas.CapabilityDatabase},
		{"compute_", atlas.CapabilityCompute}, {"container_", atlas.CapabilityCompute},
		{"cloud_run_", atlas.CapabilityCompute}, {"dns_", atlas.CapabilityDNS},
		{"monitoring_", atlas.CapabilityMonitoring},
	},
	"cloudflare": {
		{"record", atlas.CapabilityDNS}, {"dns_record", atlas.CapabilityDNS}, {"zone", atlas.CapabilityDNS},
		{"r2_bucket", atlas.CapabilityObjectStorage}, {"pages_", atlas.CapabilityAppDelivery},
		{"worker", atlas.CapabilityCompute},
	},
	"digitalocean": {
		{"droplet", atlas.CapabilityCompute}, {"kubernetes_", atlas.CapabilityCompute},
		{"app", atlas.CapabilityAppDelivery}, {"database_", atlas.CapabilityDatabase},
		{"spaces_bucket", atlas.CapabilityObjectStorage}, {"domain", atlas.CapabilityDNS},
		{"record", atlas.CapabilityDNS},
	},
	"scaleway": {
		{"instance_", atlas.CapabilityCompute}, {"k8s_", atlas.CapabilityCompute},
		{"rdb_", atlas.CapabilityDatabase}, {"object_bucket", atlas.CapabilityObjectStorage},
		{"domain_", atlas.CapabilityDNS},
	},
	"datadog": {{"monitor", atlas.CapabilityMonitoring}},
}

// registries is every image registry whose path is a forge repository's: an
// image there is built from the repository of the same path on that forge.
var registries = map[string]string{"ghcr.io": "github.com", "registry.gitlab.com": "gitlab.com"}
