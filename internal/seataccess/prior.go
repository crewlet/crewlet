package seataccess

// PriorCredentialID is the id of the credential this op's earlier steps
// minted for field, the latest first, or "" where none did.
//
// A rotation's second step reads it to retire every credential the first one
// replaced and never the one it minted: one place reads [ApplyInput.Prior]
// for that, so no vendor spells the walk its own way.
func (in ApplyInput) PriorCredentialID(field string) string {
	for i := len(in.Prior) - 1; i >= 0; i-- {
		for _, minted := range in.Prior[i].Minted {
			if minted.Field == field && minted.CredentialID != "" {
				return minted.CredentialID
			}
		}
	}
	return ""
}
