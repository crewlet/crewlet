package appstoreconnect

import "github.com/crewlet/crewlet/internal/growth"

// Apple's roles a team key may hold, by Apple's own names, each a key slot of
// the company document (config.AppStoreConnectKeys) and a catalog role.
const (
	RoleAdmin           = "ADMIN"
	RoleAppManager      = "APP_MANAGER"
	RoleDeveloper       = "DEVELOPER"
	RoleMarketing       = "MARKETING"
	RoleFinance         = "FINANCE"
	RoleSales           = "SALES"
	RoleCustomerSupport = "CUSTOMER_SUPPORT"
	RoleAccessToReports = "ACCESS_TO_REPORTS"
)

// slotRole maps a document slot to its Apple role.
var slotRole = map[string]string{
	"admin": RoleAdmin, "app_manager": RoleAppManager, "developer": RoleDeveloper, "marketing": RoleMarketing,
	"finance": RoleFinance, "sales": RoleSales, "customer_support": RoleCustomerSupport,
	"access_to_reports": RoleAccessToReports,
}

// leastFirst is every role from the least it may change to the most: a call
// goes through the first connected role its operation allows, so a key that
// leaks from the engine reaches the least.
var leastFirst = []string{RoleAccessToReports, RoleSales, RoleFinance, RoleCustomerSupport, RoleMarketing,
	RoleDeveloper, RoleAppManager, RoleAdmin}

// roles are the catalog's roles, as setup explains each.
func roles() []growth.Role {
	return []growth.Role{
		{ID: RoleAdmin, Name: "Admin", Note: "Everything, including users, keys and agreements: account changes are held for an administrator."},
		{ID: RoleAppManager, Name: "App Manager", Note: "Apps, versions, submissions, prices, in-app purchases and TestFlight, internal and external."},
		{ID: RoleDeveloper, Name: "Developer", Note: "Builds and TestFlight reads; with access to certificates, identifiers and profiles, signing resources and devices."},
		{ID: RoleMarketing, Name: "Marketing", Note: "Listings, custom product pages, in-app events, reviews (read) and internal TestFlight groups."},
		{ID: RoleFinance, Name: "Finance", Note: "Sales, finance and analytics reports, prices (read) and agreements (read)."},
		{ID: RoleSales, Name: "Sales", Note: "Sales and analytics reports."},
		{ID: RoleCustomerSupport, Name: "Customer Support", Note: "Customer reviews and replies to them."},
		{ID: RoleAccessToReports, Name: "Access to Reports", Note: "Sales and trends reports."},
	}
}

// adminSlot is the Admin role's slot. Its key manages the team's users and
// keys, so it makes an admin-class act alone, which an administrator allowed:
// never a read, a placement search, a pass or an operate act. An operation
// only Admin may make is classed admin (operations.go).
const adminSlot = "admin"

// keyFor is the slot of the least-privileged connected key op allows, false
// for none; the Admin key only where admin is true.
func keyFor(op growth.Operation, connected []string, admin bool) (string, bool) {
	for _, role := range leastFirst {
		if (role == RoleAdmin && !admin) || !growth.Permits(op, []string{role}) {
			continue
		}
		for _, slot := range connected {
			if slotRole[slot] == role {
				return slot, true
			}
		}
	}
	return "", false
}
