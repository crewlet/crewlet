package appstoreconnect

import "github.com/crewlet/crewlet/internal/growth"

// Apple's roles a team key may hold, by Apple's own names, each a catalog
// role.
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

// roles are the catalog's roles, as setup explains each.
func roles() []growth.Role {
	return []growth.Role{
		{ID: RoleAdmin, Name: "Admin", Note: "Everything, including users, keys and agreements. Crewlet holds no Admin key: an act only it may make is made in App Store Connect."},
		{ID: RoleAppManager, Name: "App Manager", Note: "Apps, versions, submissions, prices, in-app purchases and TestFlight, internal and external."},
		{ID: RoleDeveloper, Name: "Developer", Note: "Builds and TestFlight reads; with access to certificates, identifiers and profiles, signing resources and devices."},
		{ID: RoleMarketing, Name: "Marketing", Note: "Listings, custom product pages, in-app events, reviews (read) and internal TestFlight groups."},
		{ID: RoleFinance, Name: "Finance", Note: "Sales, finance and analytics reports, prices (read) and agreements (read)."},
		{ID: RoleSales, Name: "Sales", Note: "Sales and analytics reports."},
		{ID: RoleCustomerSupport, Name: "Customer Support", Note: "Customer reviews and replies to them."},
		{ID: RoleAccessToReports, Name: "Access to Reports", Note: "Sales and trends reports."},
	}
}
