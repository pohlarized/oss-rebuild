// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/google/oss-rebuild/internal/gitx"
	"github.com/google/oss-rebuild/internal/semver"
	"github.com/google/oss-rebuild/internal/uri"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	npmreg "github.com/google/oss-rebuild/pkg/registry/npm"
	"github.com/pkg/errors"
	"log"
	"os"
	"path"
	"regexp"
	"strings"
)

func getPackageJSON(tree *object.Tree, path string) (pkgJSON npmreg.PackageJSON, err error) {
	f, err := tree.File(path)
	if err != nil {
		return pkgJSON, err
	}
	p, err := f.Contents()
	if err != nil {
		return pkgJSON, err
	}
	return pkgJSON, json.Unmarshal([]byte(p), &pkgJSON)
}

func (Rebuilder) InferRepo(ctx context.Context, t rebuild.Target, mux rebuild.RegistryMux) (string, error) {
	vmeta, err := mux.NPM.Version(ctx, t.Package, t.Version)
	if err != nil {
		return "", err
	}
	return uri.CanonicalizeRepoURI(vmeta.Repository.URL)
}

func (Rebuilder) CloneRepo(ctx context.Context, t rebuild.Target, repoURI string, ropt *gitx.RepositoryOptions) (r rebuild.RepoConfig, err error) {
	r.URI = repoURI
	r.Repository, err = rebuild.LoadRepo(ctx, t.Package, ropt.Storer, ropt.Worktree, git.CloneOptions{URL: r.URI, RecurseSubmodules: git.DefaultSubmoduleRecursionDepth})
	switch err {
	case nil:
	case transport.ErrAuthenticationRequired:
		return r, errors.Errorf("repo invalid or private [repo=%s]", r.URI)
	default:
		return r, errors.Wrapf(err, "clone failed [repo=%s]", r.URI)
	}
	// Do package.json search.
	head, _ := r.Repository.Head()
	c, _ := r.Repository.CommitObject(head.Hash())
	_, pkgPath, err := findPackageJSON(r.Repository, c, t.Package)
	if err != nil {
		log.Printf("package.json path heuristic failed [pkg=%s,repo=%s]: %s\n", t.Package, r.URI, err.Error())
	}
	r.Dir = path.Dir(pkgPath)
	return r, nil
}

func PickNodeVersion(meta *npmreg.NPMVersion) (string, error) {
	version := meta.NodeVersion
	if version == "" {
		// TODO: Consider selecting based on release date.
		return "10.17.0", nil
	}
	nv, err := semver.New(version)
	if err != nil {
		return "", errors.Errorf("invalid node version: %s", version)
	}
	if nv.Compare(npmreg.UnofficialNodeReleases[0].Version) > 0 {
		// Trust the future
		return nv.String(), nil
	}
	var best npmreg.NodeRelease
	for _, r := range npmreg.UnofficialNodeReleases {
		if !r.HasMUSL {
			continue
		}
		if cmp := r.Version.Compare(nv); cmp == 0 {
			return r.Version.String(), nil
		} else if cmp < 0 {
			return best.Version.String(), nil
		}
		// Skip update if major.minor match but patch version is lower
		if !(best.Version.Major == r.Version.Major && best.Version.Minor == r.Version.Minor) {
			best = r
		}
	}
	return best.Version.String(), nil
}

func PickNPMVersion(meta *npmreg.NPMVersion) (string, error) {
	npmv := meta.NPMVersion
	if npmv == "" {
		// TODO: Guess based on upload date.
		return "", errors.New("No NPM version")
	}
	s, err := semver.New(npmv)
	if err != nil || s.Prerelease != "" || s.Build != "" {
		return "", errors.Errorf("Unsupported NPM version '%s'", npmv)
	}
	if s.Major < 5 {
		// NOTE: Upgrade all previous versions to 5.0.4 to fix incompatibilities.
		return "5.0.4", nil
	} else if s.Major == 5 && (s.Minor == 4 || s.Minor == 5) {
		// NOTE: Some versions of NPM 5 had issues with Node 9 and higher.
		// Fix: https://github.com/npm/npm/commit/c851bb503a756b7cd48d12ef0e12f39e6f30c577
		// Release: https://github.com/npm/npm/releases/tag/v5.6.0
		return "5.6.0", nil
	}
	return npmv, nil
}

func InferLocation(
	target rebuild.Target,
	vmeta *npmreg.NPMVersion,
	rcfg *rebuild.RepoConfig,
	strategyHint rebuild.CommitInferenceStrategyName,
) (loc rebuild.Location, versionOverride string, err error) {
	// Initialize location with repo URI from config
	loc = rebuild.Location{
		Repo: rcfg.URI,
	}
	// Determine dir for build
	if vmeta.Directory != "" {
		if rcfg.Dir != "" && rcfg.Dir != vmeta.Directory {
			log.Printf("package.json path disagreement [metadata=%s,heuristic=%s]\n", vmeta.Directory, rcfg.Dir)
		}
		loc.Dir = vmeta.Directory
	} else if rcfg.Dir != "" {
		loc.Dir = rcfg.Dir
	} else {
		loc.Dir = "."
	}
	if strategyHint != "" {
		switch strategyHint {
		case rebuild.CommitInferenceStrategyRegistry:
			fmt.Fprintf(os.Stderr, "Using registry commit inference strategy")
			registryRef := vmeta.GitHEAD
			if registryRef == "" {
				return loc, "", errors.New("strategy 'registry' failed: no registry ref")
			}
			loc.Ref = registryRef
			return loc, "", nil

		case rebuild.CommitInferenceStrategyTag:
			fmt.Fprintf(os.Stderr, "Using tag commit inference strategy")
			tagGuess, err := rebuild.FindTagMatch(target.Package, target.Version, rcfg.Repository)
			if err != nil {
				return loc, "", errors.Wrapf(err, "[INTERNAL] tag heuristic error")
			}
			if tagGuess == "" {
				return loc, "", errors.New("strategy 'tag' failed: no tag match")
			}
			loc.Ref = tagGuess
			return loc, "", nil

		case rebuild.CommitInferenceStrategyManifest:
			fmt.Fprintf(os.Stderr, "Using manifest commit inference strategy")
			// Do version heuristic search.
			refMap, err := pkgJSONSearch(
				target.Package,
				path.Join(rcfg.Dir, "package.json"),
				rcfg.Repository,
			)
			if err != nil {
				log.Printf(
					"package.json version heuristic failed [pkg=%s,repo=%s]: %s\n",
					target.Package,
					rcfg.URI,
					err.Error(),
				)
			}
			pkgJSONGuess := refMap[target.Version]
			if pkgJSONGuess == "" {
				return loc, "", errors.New("strategy 'log' failed: no git log match")
			}
			loc.Ref = pkgJSONGuess
			return loc, "", nil

		default:
			return loc, "", errors.Errorf("unsupported strategy hint for npm: %s", strategyHint)
		}
	} else {
		return loc, "", errors.New(
			"this version of OSS-Rebuild requires a commit inference strategy hint!",
		)
	}
}

func (Rebuilder) InferStrategy(
	ctx context.Context,
	t rebuild.Target,
	mux rebuild.RegistryMux,
	rcfg *rebuild.RepoConfig,
	hint rebuild.Strategy,
) (rebuild.Strategy, error) {
	name, version := t.Package, t.Version
	vmeta, err := mux.NPM.Version(ctx, name, version)
	if err != nil {
		return nil, err
	}
	npmv, err := PickNPMVersion(vmeta)
	if err != nil {
		return nil, err
	}
	var versionOverride string
	loc := rebuild.Location{Repo: rcfg.URI, Dir: rcfg.Dir}
	if lh, ok := hint.(*rebuild.LocationHint); hint != nil && !ok {
		if commitInferenceStrategyHint, ok := hint.(*rebuild.CommitInferenceStrategyHint); ok {
			loc, versionOverride, err = InferLocation(t, vmeta, rcfg, commitInferenceStrategyHint.Name)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, errors.Errorf("unsupported hint type: %T", hint)
		}
	} else if lh != nil && lh.Ref != "" {
		loc.Ref = lh.Ref
		if lh.Dir != "" {
			loc.Dir = lh.Dir
		}
	} else {
		loc, versionOverride, err = InferLocation(t, vmeta, rcfg, "")
		if err != nil {
			return nil, err
		}
	}
	return &NPMPackBuild{
		NPMVersion:      npmv,
		VersionOverride: versionOverride,
		Location:        loc,
	}, nil
}

// findAndValidatePackageJSON ensures the package config has the expected name and version,
// or finds a new version if necessary.
func findAndValidatePackageJSON(repo *git.Repository, c *object.Commit, name, version, guess string) (string, error) {
	t, _ := c.Tree()
	path := path.Join(guess, "package.json")
	orig, err := getPackageJSON(t, path)
	pkgJSON := &orig
	if err != nil || pkgJSON.Name != name {
		pkgJSON, path, err = findPackageJSON(repo, c, name)
	}
	if err == object.ErrFileNotFound {
		return path, errors.Errorf("package.json file not found [path=%s]", guess)
	} else if _, ok := err.(*json.SyntaxError); ok {
		return path, errors.Wrapf(err, "failed to parse package.json")
	} else if err != nil {
		return path, errors.Wrapf(err, "unknown package.json error")
	} else if pkgJSON.Name != name {
		return path, errors.Errorf("mismatched name [expected=%s,actual=%s,path=%s]", name, pkgJSON.Name, guess)
	} else if pkgJSON.Version != version {
		return path, errors.Errorf("mismatched version [expected=%s,actual=%s]", version, pkgJSON.Version)
	}
	return path, nil
}

func findPackageJSON(repo *git.Repository, c *object.Commit, pkg string) (*npmreg.PackageJSON, string, error) {
	t, _ := c.Tree()
	wellKnownPaths := []string{
		"package.json",
		path.Join("packages", pkg[strings.IndexRune(pkg, '/')+1:], "package.json"),
	}
	for _, path := range wellKnownPaths {
		pkgJSON, err := getPackageJSON(t, path)
		if err != nil {
			if err == object.ErrFileNotFound {
				continue
			}
			if _, ok := err.(*json.SyntaxError); ok {
				continue
			}
			return nil, "", err
		}
		if pkg == pkgJSON.Name {
			return &pkgJSON, path, nil
		}
	}
	grs, err := repo.Grep(&git.GrepOptions{
		CommitHash: c.Hash,
		PathSpecs:  []*regexp.Regexp{regexp.MustCompile(".*/package.json$")},
		Patterns:   []*regexp.Regexp{regexp.MustCompile(fmt.Sprintf(`"name":\s*"%s"`, pkg))},
	})
	if err != nil {
		return nil, "", err
	}
	var names []string
	var pkgJSONs []npmreg.PackageJSON
	for _, gr := range grs {
		pkgJSON, err := getPackageJSON(t, gr.FileName)
		if err != nil {
			continue
		}
		if pkg == pkgJSON.Name {
			names = append(names, gr.FileName)
			pkgJSONs = append(pkgJSONs, pkgJSON)
		}
	}
	if len(names) > 0 {
		if len(names) > 1 {
			log.Printf("Multiple package.json file candidates [pkg=%s,ref=%s,matches=%v]\n", pkg, c.Hash.String(), names)
		}
		return &pkgJSONs[0], names[0], nil
	}
	return nil, "", errors.Errorf("package.json heuristic found no matches")
}

func pkgJSONSearch(pkg, pkgJSONPath string, repo *git.Repository) (tm map[string]string, err error) {
	fmt.Fprintf(os.Stderr, "Creating pkg json ref map")
	tm = make(map[string]string)
	commitIter, err := repo.Log(&git.LogOptions{
		Order:      git.LogOrderCommitterTime,
		PathFilter: func(s string) bool { return s == pkgJSONPath },
		All:        true,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "searching for commits touching package.json")
	}
	duplicates := make(map[string]string)
	err = commitIter.ForEach(func(c *object.Commit) error {
		t, err := c.Tree()
		if err != nil {
			return errors.Wrapf(err, "fetching tree")
		}
		pkgJSON, err := getPackageJSON(t, pkgJSONPath)
		if _, ok := err.(*json.SyntaxError); ok {
			return nil // unable to parse
		} else if errors.Is(err, object.ErrFileNotFound) {
			return nil // file deleted at this commit
		} else if err != nil {
			return errors.Wrapf(err, "fetching package.json")
		}
		if pkgJSON.Name != pkg {
			// TODO: Handle the case where the package name has changed.
			log.Printf("Package name mismatch [expected=%s,actual=%s,path=%s,ref=%s]\n", pkg, pkgJSON.Name, pkgJSONPath, c.Hash.String())
			return nil
		}
		ver := pkgJSON.Version
		if ver == "" {
			return nil
		}
		// If any are the same, return nil. (merges would create duplicates.)
		var foundMatch bool
		err = c.Parents().ForEach(func(c *object.Commit) error {
			t, err := c.Tree()
			if err != nil {
				return errors.Wrapf(err, "fetching tree")
			}
			pkgJSON, err := getPackageJSON(t, pkgJSONPath)
			if err != nil {
				// TODO: Detect and record file moves.
				return nil
			}
			if pkgJSON.Name == pkg && pkgJSON.Version == ver {
				foundMatch = true
			}
			return nil
		})
		if err != nil {
			return errors.Wrapf(err, "comparing against parent package.json")
		}
		if !foundMatch {
			if tm[ver] != "" {
				// NOTE: This ignores commits processed later sequentially. Empirically, this seems to pick the better commit.
				if duplicates[ver] != "" {
					duplicates[ver] = fmt.Sprintf("%s,%s", duplicates[ver], c.Hash.String())
				} else {
					duplicates[ver] = fmt.Sprintf("%s,%s", tm[ver], c.Hash.String())
				}
			} else {
				tm[ver] = c.Hash.String()
			}
		}
		return nil
	})
	if len(duplicates) > 0 {
		for ver, dupes := range duplicates {
			log.Printf("Multiple matches found [pkg=%s,ver=%s,refs=%v]\n", pkg, ver, dupes)
		}
	}
	return
}
