package atlas

import "slices"

// Vendor is who provides an account, a model or a resource. A closed set:
// a provider the engine does not know yet is [VendorOther] with a name, which
// is display text and decides nothing.
type Vendor string

// The vendors the engine knows.
const (
	VendorAWS          Vendor = "aws"
	VendorAnthropic    Vendor = "anthropic"
	VendorAtlassian    Vendor = "atlassian"
	VendorCloudflare   Vendor = "cloudflare"
	VendorCursor       Vendor = "cursor"
	VendorDatadog      Vendor = "datadog"
	VendorDigitalOcean Vendor = "digitalocean"
	VendorE2B          Vendor = "e2b"
	VendorExpo         Vendor = "expo"
	VendorGitHub       Vendor = "github"
	VendorGitLab       Vendor = "gitlab"
	VendorGitlet       Vendor = "gitlet"
	VendorGoogle       Vendor = "google"
	VendorGoogleCloud  Vendor = "google_cloud"
	VendorMattermost   Vendor = "mattermost"
	VendorOpenAI       Vendor = "openai"
	VendorScaleway     Vendor = "scaleway"
	VendorSlack        Vendor = "slack"
	VendorXAI          Vendor = "xai"
	VendorOther        Vendor = "other"
)

// Vendors is every vendor, [VendorOther] last.
var Vendors = []Vendor{
	VendorAWS, VendorAnthropic, VendorAtlassian, VendorCloudflare, VendorCursor,
	VendorDatadog, VendorDigitalOcean, VendorE2B, VendorExpo, VendorGitHub,
	VendorGitLab, VendorGitlet, VendorGoogle, VendorGoogleCloud, VendorMattermost,
	VendorOpenAI, VendorScaleway, VendorSlack, VendorXAI, VendorOther,
}

// Valid reports whether v is a member of the closed set.
func (v Vendor) Valid() bool { return slices.Contains(Vendors, v) }

// Capability is something a vendor account does for the company. "Which CDN,
// which monitoring, where does the code live" is answered by capabilities,
// never by a kind per product.
type Capability string

// The capabilities a vendor account can carry.
const (
	CapabilityChat          Capability = "chat"
	CapabilityTracker       Capability = "tracker"
	CapabilityKnowledge     Capability = "knowledge"
	CapabilityCodeHosting   Capability = "code_hosting"
	CapabilityCI            Capability = "ci"
	CapabilityMonitoring    Capability = "monitoring"
	CapabilityAlerting      Capability = "alerting"
	CapabilityDNS           Capability = "dns"
	CapabilityCDN           Capability = "cdn"
	CapabilityCompute       Capability = "compute"
	CapabilityObjectStorage Capability = "object_storage"
	CapabilityDatabase      Capability = "database"
	CapabilityEmail         Capability = "email"
	CapabilityModelAPI      Capability = "model_api"
	CapabilityAppDelivery   Capability = "app_delivery"
	CapabilityIdentity      Capability = "identity"
)

// Capabilities is every capability.
var Capabilities = []Capability{
	CapabilityChat, CapabilityTracker, CapabilityKnowledge, CapabilityCodeHosting,
	CapabilityCI, CapabilityMonitoring, CapabilityAlerting, CapabilityDNS, CapabilityCDN,
	CapabilityCompute, CapabilityObjectStorage, CapabilityDatabase, CapabilityEmail,
	CapabilityModelAPI, CapabilityAppDelivery, CapabilityIdentity,
}

// Valid reports whether c is a member of the closed set.
func (c Capability) Valid() bool { return slices.Contains(Capabilities, c) }
