package codescan

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/deployfiles"
)

// MaxClaimsPerFile is how many claims one file may make. A hand-written
// compose file of thirty services makes about a hundred; a file past this is
// generated, and what it says past the bound is counted, never recorded. It
// also bounds a repository's standing, which names every claim's slot.
const MaxClaimsPerFile = 128

// Parsed is what one file declares: its claims in the repository's voice,
// how many values the grammars refused, how many claims fell past
// [MaxClaimsPerFile], and whether the file could be read as its format.
type Parsed struct {
	Claims    []atlas.Claim
	Withheld  int
	Truncated int
	Unparsed  bool
}

// errUnparsed is a file that is not its format: counted on the standing,
// never quoted, since what a parser says of a file repeats its text.
var errUnparsed = errors.New("not a file of its format")

// Repo is the repository a file is read from, as the parsers name it.
type Repo struct {
	// Voice is the name its files' claims are made under ([atlas.CodeSource]).
	Voice string
	// Name is its short name, which a service it builds is named for.
	Name string
	// Key is its key in the atlas, which what it builds is built from.
	Key string
}

// GitletRepo is the Gitlet repository named name.
func GitletRepo(name string) Repo {
	return Repo{Voice: name, Name: name, Key: atlas.GitletPrefix + "/" + name}
}

// ForgeRepo is the repository a forge holds under the atlas key key
// (github.com/acme/api), and false for one whose key is no voice's name.
//
// ITS VOICE IS ITS KEY WITH ':' FOR EVERY '/', since a source's name takes no
// slash: exact both ways, because no host and no part of a forge's path holds
// a colon, and never a Gitlet repository's name, which holds neither. Its
// short name is the last part of its path, which a key holds in lower case.
func ForgeRepo(key string) (Repo, bool) {
	ref, err := atlas.RecordedRef(atlas.KindRepository, key)
	if err != nil || ref.Key != key || ref.OnGitlet() || strings.Contains(key, ":") {
		return Repo{}, false
	}
	voice := strings.ReplaceAll(key, "/", ":")
	// THE VOICE'S OWN GRAMMAR, asked of the vocabulary: a key too long or
	// too strange to be a source's name is a repository that is not read.
	if _, err := atlas.Exists(ref, atlas.CodeSource(voice), time.Unix(0, 0).UTC()); err != nil {
		return Repo{}, false
	}
	return Repo{Voice: voice, Name: key[strings.LastIndexByte(key, '/')+1:], Key: key}, true
}

// Parse reads one listed file of repo as its format and answers what it
// declares. raw is the file's bytes, nil for a format that is listed by its
// presence. PURE, and it never fails: a file that is not its format declares
// nothing and says so.
//
// THE PARSERS DECODE INTO CLOSED TYPES: a field nobody declared is never
// decoded, so an environment block, a command line, a secret's data and a
// dependency's version have no field to land in
// (TestNoParserDeclaresAFieldForAnEnvironmentValue).
func Parse(repo Repo, file string, format deployfiles.Format, raw []byte, at time.Time) (out Parsed) {
	f := &finder{b: collect.NewBuilderFor(atlas.CodeSource(repo.Voice), at), repoName: repo.Name, repoKey: repo.Key,
		file: file}
	// A PARSER PANIC IS AN UNREADABLE FILE, never the node's: the bytes are
	// anybody's with a push, and this runs in the home's loop.
	defer func() {
		if recover() != nil {
			out = Parsed{Unparsed: true}
		}
	}()
	var err error
	switch format {
	case deployfiles.Dockerfile, deployfiles.Procfile:
		f.builtHere(f.repoName)
	case deployfiles.Compose:
		err = f.compose(raw)
	case deployfiles.Kubernetes:
		err = f.kubernetes(raw)
	case deployfiles.HelmChart:
		err = f.helmChart(raw)
	case deployfiles.Terraform:
		f.terraform(raw)
	case deployfiles.Fly:
		err = f.fly(raw)
	case deployfiles.Render:
		err = f.render(raw)
	case deployfiles.PackageJSON:
		err = f.packageJSON(raw)
	case deployfiles.GoMod:
		f.goMod(raw)
	case deployfiles.Requirements:
		f.requirements(raw)
	case deployfiles.Pyproject:
		err = f.pyproject(raw)
	case deployfiles.Gemfile:
		f.gemfile(raw)
	case deployfiles.Cargo:
		err = f.cargo(raw)
	default:
		err = errUnparsed
	}
	read := f.b.Read()
	out = Parsed{Claims: read.Claims, Withheld: len(read.Withheld), Unparsed: err != nil}
	if len(out.Claims) > MaxClaimsPerFile {
		out.Truncated, out.Claims = len(out.Claims)-MaxClaimsPerFile, out.Claims[:MaxClaimsPerFile]
	}
	return out
}

// finder gathers one file's claims through the vocabulary's grammars
// ([collect.Builder]), which refuse a value or a key shaped like a credential
// and count it by where it sat.
type finder struct {
	b        *collect.Builder
	repoName string
	repoKey  string
	file     string
}

// where places a value in the file, for the count of what was withheld.
func (f *finder) where(parts ...string) string {
	return f.file + ": " + strings.Join(parts, ".")
}

// repository is the repository the file is in, NAMED and never claimed:
// Gitlet's own rows, or its forge's read, say it exists.
func (f *finder) repository(where string) (atlas.Ref, bool) {
	return f.b.Names(where, atlas.KindRepository, f.repoKey)
}

// service claims the service keyed key.
func (f *finder) service(where, key string) (atlas.Ref, bool) {
	return f.b.Ref(where, atlas.KindService, key)
}

// builtHere claims the service keyed key and that this repository builds it.
func (f *finder) builtHere(key string) (atlas.Ref, bool) {
	where := f.where("service")
	svc, ok := f.service(where, key)
	if !ok {
		return svc, false
	}
	if repo, ok := f.repository(where); ok {
		f.b.Relates(where, svc, atlas.RelationBuiltFrom, repo)
	}
	return svc, true
}

// store claims the database, queue or bucket e names under namespace/name,
// with its engine, and its version where tag starts with one.
func (f *finder) store(where string, e engine, namespace, name, tag string) (atlas.Ref, bool) {
	r, ok := f.b.Ref(where, e.kind, namespace+"/"+name)
	if !ok {
		return r, false
	}
	if e.word != "" {
		f.b.Says(where, r, atlas.FactEngine, e.word)
		f.b.Says(where, r, atlas.FactEngineVersion, leadingVersion(tag))
	}
	return r, true
}

// account claims the company's account at v: keyed by the vendor alone, since
// a file names the vendor and never which account. uses is what the file
// says the account is used for, beside what the vendor's row says.
func (f *finder) account(where string, v vendor, uses ...atlas.Capability) (atlas.Ref, bool) {
	key := string(v.known)
	if v.known == atlas.VendorOther {
		key += "/" + v.slug
	}
	r, ok := f.b.Ref(where, atlas.KindAccount, key)
	if !ok {
		return r, false
	}
	f.b.Says(where, r, atlas.FactVendor, string(v.known))
	f.b.Says(where, r, atlas.FactName, v.name)
	f.b.Says(where, r, atlas.FactCapability, string(v.capability))
	for _, c := range uses {
		f.b.Says(where, r, atlas.FactCapability, string(c))
	}
	return r, true
}

// dependency claims the vendor whose SDK the package name is, if any.
func (f *finder) dependency(eco ecosystem, name string) {
	if v, ok := vendorOf(eco, strings.TrimSpace(name)); ok {
		f.account(f.where("dependencies"), v)
	}
}

// dirName is the name of the directory the file is in, and the repository's
// name for a file at its root: what compose itself names a project by.
func (f *finder) dirName() string {
	if dir := path.Dir(f.file); dir != "." {
		return path.Base(dir)
	}
	return f.repoName
}

// image is an image reference in parts: its registry host ("" for the
// default one), its path and its tag. A digest is dropped.
type image struct {
	host, path, tag string
}

// errTemplated is an image whose name a variable fills in: no name at all.
var errTemplated = errors.New("an image named through a variable")

// parseImage reads an image reference. A name carrying a variable is refused:
// it names whatever the variable holds. A tag carrying one is dropped.
func parseImage(raw string) (image, error) {
	ref, _, _ := strings.Cut(strings.TrimSpace(raw), "@")
	name, tag := ref, ""
	if i := strings.LastIndexByte(ref, ':'); i >= 0 && !strings.Contains(ref[i:], "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	if name == "" || strings.ContainsAny(name, "${}") {
		return image{}, errTemplated
	}
	if strings.ContainsAny(tag, "${}") {
		tag = ""
	}
	img := image{path: name, tag: tag}
	if first, rest, ok := strings.Cut(name, "/"); ok && strings.ContainsAny(first, ".:") {
		img.host, img.path = strings.ToLower(first), rest
	}
	return img, nil
}

// engine is the database or queue the image is known to run, by its name.
func (i image) engine() (engine, bool) {
	e, ok := engines()[strings.ToLower(path.Base(i.path))]
	return e, ok
}

// forgeRepository is the forge repository an image in a forge's registry is
// built from: the first two parts of its path, on that forge.
func (i image) forgeRepository() (string, bool) {
	forge, ok := registries()[i.host]
	parts := strings.Split(i.path, "/")
	if !ok || len(parts) < 2 {
		return "", false
	}
	return fmt.Sprintf("%s/%s/%s", forge, parts[0], parts[1]), true
}

var leadingVersionRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^v?(\d+(?:\.\d+){0,3})(?:$|[-_+])`)
})

// leadingVersion is the version a tag starts with ("16.2-alpine" is 16.2),
// "" for a tag that starts with none (latest, a branch, a commit).
func leadingVersion(tag string) string {
	if m := leadingVersionRE().FindStringSubmatch(tag); m != nil {
		return m[1]
	}
	return ""
}
