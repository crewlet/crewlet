package queries

import "github.com/crewlet/crewlet/internal/config"

// DelegatedLimitRows are the rows `delegated_decisions` answers its limits as,
// read off c.
func DelegatedLimitRows(c *config.Company) []LimitRow { return limitRows(c, delegatedLimits) }
