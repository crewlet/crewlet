package derive

import (
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/config"
)

// GitletRepo is one Gitlet repository as the atlas reads it: the fields of its
// row a map needs, and nothing a person wrote about it. Its description is
// somebody's words, which the atlas does not carry.
type GitletRepo struct {
	Name          string
	Status        string
	DefaultBranch string
	Visibility    string
	// OwnerKind is unit or seat, Owner its key or handle.
	OwnerKind string
	Owner     string
	// MirrorFrom, MirrorTo and ImportedFrom are the other hosts its detail
	// names, "" for none or for a repository the settings declare, whose
	// other hosts the settings name; the credentials beside them are never
	// read.
	MirrorFrom   string
	MirrorTo     string
	ImportedFrom string
	// LastPush is the instant of its last push, zero for none.
	LastPush time.Time
}

// GitletCheck is one check a repository's default branch runs, at its latest
// attempt on the branch's head.
type GitletCheck struct {
	Repo     string
	Name     string
	State    string
	Required bool
	// External is a check a person's CI reported, rather than one the engine
	// ran in a box.
	External bool
}

// gitletStatuses is the repository states the atlas shows. A deleted or
// purging repository is no longer something the company runs on.
var gitletStatuses = map[string]bool{"importing": true, "active": true, "archived": true}

// Declares reports whether the settings declare the Gitlet repository name,
// and so name its other hosts: a repository they do not declare is read in
// detail for them.
func (s Settings) Declares(name string) bool {
	_, ok := s.declared(name)
	return ok
}

func (s Settings) declared(name string) (config.CodeRepo, bool) {
	if s.company == nil {
		return config.CodeRepo{}, false
	}
	for _, r := range s.company.Code.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.CodeRepo{}, false
}

// Gitlet is the claims Gitlet's own rows make: every repository the reader may
// read, the account they belong to, their owners and their other hosts, and
// the checks their default branches run. The rows are Gitlet's own read as the
// reader, so a repository only its unit may read reaches only that unit; the
// settings add the other hosts of the repositories they declare, and only of
// those the rows list.
func (s Settings) Gitlet(repos []GitletRepo, checks []GitletCheck, at time.Time) Derived {
	b := newBuilder(atlas.RecordSource(atlas.RecordGitlet), at)
	declared := newBuilder(atlas.SettingsSource(s.revision), at)
	account := atlas.MustRef(atlas.KindAccount, string(atlas.VendorGitlet))
	listed := map[string]atlas.Ref{}
	for _, repo := range repos {
		if !gitletStatuses[repo.Status] {
			continue
		}
		where := "gitlet:" + repo.Name
		r, ok := b.ref(where, atlas.KindRepository, atlas.GitletPrefix+"/"+repo.Name)
		if !ok {
			continue
		}
		listed[repo.Name] = r
		b.says(where, r, atlas.FactName, repo.Name)
		b.says(where, r, atlas.FactStatus, repo.Status)
		b.says(where, r, atlas.FactDefaultBranch, repo.DefaultBranch)
		b.says(where, r, atlas.FactVisibility, repo.Visibility)
		b.says(where+".mirror_from", r, atlas.FactPullMirrorOf, repo.MirrorFrom)
		b.says(where+".mirror_to", r, atlas.FactPushMirrorTo, repo.MirrorTo)
		b.says(where+".import", r, atlas.FactImportedFrom, repo.ImportedFrom)
		if d, ok := s.declared(repo.Name); ok {
			setting := "code.repos." + repo.Name
			if d.MirrorFrom != nil {
				declared.says(setting+".mirror_from.url", r, atlas.FactPullMirrorOf, d.MirrorFrom.URL)
			}
			if d.MirrorTo != nil {
				declared.says(setting+".mirror_to.url", r, atlas.FactPushMirrorTo, d.MirrorTo.URL)
			}
			if d.Import != nil {
				declared.says(setting+".import.url", r, atlas.FactImportedFrom, d.Import.URL)
			}
		}
		b.relates(where, r, atlas.RelationBelongsTo, account)
		switch repo.OwnerKind {
		case "unit":
			b.names(where, r, atlas.RelationOwnedBy, atlas.ExternalUnit, repo.Owner)
		case "seat":
			b.names(where, r, atlas.RelationOwnedBy, atlas.ExternalSeat, repo.Owner)
		}
	}
	if len(listed) > 0 {
		if r, ok := b.ref("gitlet", atlas.KindAccount, string(atlas.VendorGitlet)); ok {
			b.says("gitlet", r, atlas.FactVendor, string(atlas.VendorGitlet))
		}
	}
	for i, check := range checks {
		repo, ok := listed[check.Repo]
		if !ok {
			continue
		}
		where := "gitlet:" + check.Repo + ".checks[" + strconv.Itoa(i) + "]"
		r, ok := b.ref(where, atlas.KindPipeline, repo.Key+"#"+check.Name)
		if !ok {
			continue
		}
		b.says(where, r, atlas.FactName, check.Name)
		b.says(where, r, atlas.FactLastResult, check.State)
		b.says(where, r, atlas.FactRequired, yesNo(check.Required))
		runner := "engine"
		if check.External {
			runner = "external"
		}
		b.says(where, r, atlas.FactRunner, runner)
		b.relates(where, r, atlas.RelationBuiltFrom, repo)
	}
	b.out.Claims = append(b.out.Claims, declared.out.Claims...)
	b.out.Withheld = append(b.out.Withheld, declared.out.Withheld...)
	return b.out
}
