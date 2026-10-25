package builtin_test

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FOLDER TREE AND THE ADOPTED BRAND, as the fake domain answers them: a
// folder is visible to a viewer it is owned by or whose record, units or the
// company its top-level folder is shared with (the relation the authority
// table decides, held here only as far as these cases need it).

// folder holds a live folder under parent ("" for the top level), owned by
// owner, shared as given when it is top-level (a subfolder inherits its root's
// shares, as the domain answers them).
func (f *fakeArtifacts) folder(id, parent, name, owner string, ownerKind iam.ActorKind,
	shares ...artifacts.Share) artifactsdomain.Folder {

	f.mu.Lock()
	defer f.mu.Unlock()
	fo := artifactsdomain.Folder{ID: id, Parent: parent, Root: id, Name: name, Owner: owner,
		OwnerKind: ownerKind, RootOwner: owner, RootOwnerKind: ownerKind, Depth: 1,
		MaxDepth: artifacts.MaxFolderDepth, Visibility: artifactsdomain.FolderPrivate, Shares: shares,
		Status: artifactsdomain.FolderLive}
	if parent != "" {
		p := f.folderByID(parent)
		fo.Root, fo.Depth, fo.Shares, fo.Visibility = p.Root, p.Depth+1, p.Shares, p.Visibility
		fo.RootOwner, fo.RootOwnerKind = p.RootOwner, p.RootOwnerKind
	} else if len(shares) > 0 {
		fo.Visibility = artifactsdomain.FolderCompany
	}
	f.folders = append(f.folders, fo)
	return fo
}

func (f *fakeArtifacts) folderByID(id string) artifactsdomain.Folder {
	for _, fo := range f.folders {
		if fo.ID == id {
			return fo
		}
	}
	return artifactsdomain.Folder{}
}

// rootOf is folder fo's top-level folder.
func (f *fakeArtifacts) rootOf(fo artifactsdomain.Folder) artifactsdomain.Folder {
	return f.folderByID(fo.Root)
}

// sees reports whether v can see folder fo: by its top-level folder's owner
// and shares.
func (f *fakeArtifacts) sees(v artifactsdomain.Viewer, fo artifactsdomain.Folder) bool {
	root := f.rootOf(fo)
	if root.RootOwner == v.Record {
		return true
	}
	for _, s := range root.Shares {
		switch s.Audience.Kind {
		case artifacts.AudienceCompany:
			return true
		case artifacts.AudienceSeat:
			if s.Audience.ID == v.Record {
				return true
			}
		case artifacts.AudienceUnit:
			if slices.Contains(v.Units, s.Audience.ID) {
				return true
			}
		}
	}
	return false
}

// pathOf is folder id's path as the listing answers it, "" for none.
func (f *fakeArtifacts) pathOf(id string) string {
	if id == "" {
		return ""
	}
	var names []string
	for fo := f.folderByID(id); fo.ID != ""; fo = f.folderByID(fo.Parent) {
		names = append([]string{fo.Name}, names...)
		if fo.Parent == "" {
			break
		}
	}
	return strings.Join(names, "/") + "/"
}

// resolve is the domain's ResolveFolder over the fake tree, under f.mu.
func (f *fakeArtifacts) resolve(ref string, within []artifactsdomain.Viewer) (artifactsdomain.Folder, error) {
	visible := func(fo artifactsdomain.Folder) bool {
		for _, v := range within {
			if !f.sees(v, fo) {
				return false
			}
		}
		return fo.Status == artifactsdomain.FolderLive
	}
	for _, fo := range f.folders {
		if fo.ID == ref && visible(fo) {
			return fo, nil
		}
	}
	parent := ""
	var found artifactsdomain.Folder
	for i, name := range strings.Split(strings.Trim(ref, "/"), "/") {
		var matches []artifactsdomain.Folder
		for _, fo := range f.folders {
			if fo.Parent == parent && strings.EqualFold(fo.Name, name) && visible(fo) {
				matches = append(matches, fo)
			}
		}
		switch {
		case len(matches) == 0:
			return artifactsdomain.Folder{}, fmt.Errorf("%w: %q", artifactsdomain.ErrFolderNotFound, ref)
		case len(matches) > 1 && i == 0:
			return artifactsdomain.Folder{}, &artifactsdomain.FolderAmbiguous{Path: ref, Candidates: matches}
		}
		found, parent = matches[0], matches[0].ID
	}
	return found, nil
}

func (f *fakeArtifacts) ResolveFolder(_ context.Context, ref string, within []artifactsdomain.Viewer,
	_ statelog.Freshness) (artifactsdomain.Folder, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolve(ref, within)
}

func (f *fakeArtifacts) Folders(_ context.Context, v artifactsdomain.Viewer, _ statelog.Freshness) (
	artifactsdomain.FolderTree, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	tree := artifactsdomain.FolderTree{Complete: true}
	for _, fo := range f.folders {
		if fo.Status == artifactsdomain.FolderLive && f.sees(v, fo) {
			tree.Folders = append(tree.Folders, artifactsdomain.FolderItem{Folder: fo})
		}
	}
	return tree, nil
}

func (f *fakeArtifacts) Brand(context.Context, statelog.Freshness) (artifactsdomain.Adoption, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.adoption == nil {
		return artifactsdomain.Adoption{}, false, nil
	}
	return *f.adoption, true, nil
}

// CreateFolders makes the missing tail of in.Names beneath in.Parent, as the
// domain's `mkdir -p` does: an existing name is reused, and a chain that
// exists already writes nothing.
func (f *fakeArtifacts) CreateFolders(_ context.Context, c artifactsdomain.Call,
	in artifactsdomain.FolderCreateInput) (artifactsdomain.FolderCreated, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.folderCreates = append(f.folderCreates, in)
	out := artifactsdomain.FolderCreated{}
	parent := f.folderByID(in.Parent)
	if parent.ID == "" {
		return out, artifactsdomain.ErrFolderNotFound
	}
	for i, name := range in.Names {
		var next artifactsdomain.Folder
		for _, fo := range f.folders {
			if fo.Parent == parent.ID && strings.EqualFold(fo.Name, name) {
				next = fo
			}
		}
		if next.ID == "" {
			if parent.Depth+1 > artifacts.MaxFolderDepth {
				return artifactsdomain.FolderCreated{}, artifactsdomain.ErrFolderDepth
			}
			next = artifactsdomain.Folder{ID: artifactsdomain.FolderIDOf(c.OpKey, i).String(),
				Parent: parent.ID, Root: parent.Root, Name: name, Owner: in.Owner,
				OwnerKind: in.OwnerKind, RootOwner: parent.RootOwner, RootOwnerKind: parent.RootOwnerKind,
				Depth: parent.Depth + 1, Shares: parent.Shares, Visibility: parent.Visibility,
				Status: artifactsdomain.FolderLive}
			f.folders = append(f.folders, next)
			out.Created++
		}
		out.Folders = append(out.Folders, next)
		parent = next
	}
	if out.Created > 0 {
		out.Result = f.result()
	}
	return out, nil
}

func (f *fakeArtifacts) Move(_ context.Context, c artifactsdomain.Call, id artifacts.ID,
	folder string) (artifactsdomain.Written, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.moves = append(f.moves, [2]string{id.String(), folder})
	h := f.heads[id]
	h.Folder = folder
	f.heads[id] = h
	return artifactsdomain.Written{Result: f.result()}, nil
}
