// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"path"
	re "regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/google/oss-rebuild/internal/gitx"
	"github.com/google/oss-rebuild/internal/uri"
	"github.com/google/oss-rebuild/internal/versionx"
	pypiresolver "github.com/google/oss-rebuild/pkg/rebuild/pypi/parsing"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/platform"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/sysdeps"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	pypireg "github.com/google/oss-rebuild/pkg/registry/pypi"
	"github.com/pkg/errors"
)

// These are commonly used in PyPi metadata to point to the project git repo, using a map as a set.
// Some people capitalize these differently, or add/remove spaces. We normalized to lower, no space.
// This list is ordered, we will choose the first occurrence.
var commonRepoLinks = []string{
	"source",
	"sourcecode",
	"repository",
	"project",
	"github",
	"code",
}

var distInfoFieldPat = re.MustCompile(`[-_.]+`)

// InferRepo reads the repository from the links the release was published
// with, falling back to the project's current links, which follow its newest
// release. A link named as a source at either level beats a repository scraped
// from a description. The first hit wins:
//
//	source links, release then project:
//	  - the home page, on a known repo host
//	  - a link named in commonRepoLinks, on a known repo host
//	  - a link named in commonRepoLinks, on any host
//	other mentions, release then project:
//	  - the first repository named after the package
//	  - the first repository cited in the description
//	  - a link under any other name, on a known repo host, sponsors excluded
func (Rebuilder) InferRepo(ctx context.Context, t rebuild.Target, mux rebuild.RegistryMux) (string, error) {
	var infos []pypireg.Info
	if release, err := mux.PyPI.Release(ctx, t.Package, t.Version); err != nil {
		log.Printf("Release metadata unavailable [pkg=%s,ver=%s]: %v", t.Package, t.Version, err)
	} else {
		infos = append(infos, release.Info)
	}
	project, err := mux.PyPI.Project(ctx, t.Package)
	if err != nil {
		return "", errors.Wrap(err, "fetching pypi metadata")
	}
	infos = append(infos, project.Info)
	for _, info := range infos {
		if repo := repoFromSourceLinks(info); repo != "" {
			return uri.CanonicalizeRepoURI(repo)
		}
	}
	var candidates []string
	for _, info := range infos {
		candidates = append(candidates, otherLinkCandidates(info)...)
	}
	// NOTE: Descriptions often cite other projects, such as an upstream commit,
	// so a repository named after the package beats the first one mentioned.
	if i := slices.IndexFunc(candidates, func(repo string) bool {
		return normalizeName(uri.RepoName(repo)) == normalizeName(t.Package)
	}); i != -1 {
		return uri.CanonicalizeRepoURI(candidates[i])
	}
	if len(candidates) != 0 {
		return uri.CanonicalizeRepoURI(candidates[0])
	}
	return "", errors.New("no git repo")
}

// repoFromSourceLinks picks the repository out of the links named as a source:
// the home page or a source link on a known repo host, else any source link.
func repoFromSourceLinks(info pypireg.Info) string {
	byName := make(map[string]string, len(info.ProjectURLs))
	for name, url := range info.ProjectURLs {
		byName[strings.ReplaceAll(strings.ToLower(name), " ", "")] = url
	}
	var sources []string
	for _, name := range commonRepoLinks {
		if url, ok := byName[name]; ok {
			sources = append(sources, url)
		}
	}
	for _, url := range append([]string{info.Homepage, byName["homepage"]}, sources...) {
		if repo := uri.FindCommonRepo(url); repo != "" {
			return repo
		}
	}
	if len(sources) != 0 {
		return sources[0]
	}
	return ""
}

// otherLinkCandidates returns repositories cited in the description followed by
// repositories linked under non-source project URL names, with sponsor links
// excluded.
func otherLinkCandidates(info pypireg.Info) []string {
	var candidates []string
	for _, r := range uri.FindCommonRepos(info.Description) {
		if !strings.Contains(r, "sponsors") {
			candidates = append(candidates, r)
		}
	}
	keys := make([]string, 0, len(info.ProjectURLs))
	for k := range info.ProjectURLs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		url := info.ProjectURLs[k]
		if strings.Contains(url, "sponsors") {
			continue
		}
		if repo := uri.FindCommonRepo(url); repo != "" {
			candidates = append(candidates, repo)
		}
	}
	return candidates
}

func (Rebuilder) CloneRepo(ctx context.Context, t rebuild.Target, repoURI string, ropt *gitx.RepositoryOptions) (r rebuild.RepoConfig, err error) {
	r.URI = repoURI
	repo, err := rebuild.LoadRepo(ctx, t.Package, ropt.Storer, ropt.Worktree, git.CloneOptions{URL: r.URI, RecurseSubmodules: git.NoRecurseSubmodules, NoCheckout: true})
	switch err {
	case nil:
		r.Repo = *repo
		return r, nil
	case transport.ErrAuthenticationRequired:
		return r, errors.Errorf("repo invalid or private [repo=%s]", r.URI)
	default:
		return r, errors.Wrapf(err, "clone failed [repo=%s]", r.URI)
	}
}

// findGitRef resolves the commit a release was built from: a tag naming the
// version when one exists, else the commit whose tree matches the pure wheel or
// sdist file blobs. A fallback that errors internally is logged and skipped,
// never aborting the chain.
func findGitRef(ctx context.Context, mux rebuild.RegistryMux, pkg, version string, release *pypireg.Release, rcfg *rebuild.RepoConfig) (string, error) {
	tagHeuristic, err := rebuild.FindTagMatch(pkg, version, rcfg.Repository)
	if err != nil {
		return "", errors.Wrapf(err, "[INTERNAL] tag heuristic error")
	}
	log.Printf("Version: %s, tag hash: \"%s\"", version, tagHeuristic)
	if tagHeuristic != "" {
		_, err = rcfg.Repository.CommitObject(plumbing.NewHash(tagHeuristic))
		if err != nil {
			switch err {
			case plumbing.ErrObjectNotFound:
				return "", errors.Errorf("[INTERNAL] Commit ref from tag heuristic not found in repo [repo=%s,ref=%s]", rcfg.URI, tagHeuristic)
			default:
				return "", errors.Wrapf(err, "Checkout failed [repo=%s,ref=%s]", rcfg.URI, tagHeuristic)
			}
		}
		return tagHeuristic, nil
	}
	if ref, err := archiveContentRef(ctx, mux, pkg, version, release, rcfg.Repository); err != nil {
		log.Printf("archive-content search failed [pkg=%s,ver=%s]: %v", pkg, version, err)
	} else if ref != "" {
		log.Printf("using archive-content ref: %s", shortHash(ref))
		return ref, nil
	}
	return "", errors.New("no git ref")
}

// FindPureWheel returns the pure wheel artifact from the given version's releases.
func FindPureWheel(artifacts []pypireg.Artifact) (*pypireg.Artifact, error) {
	for _, r := range artifacts {
		if strings.HasSuffix(r.Filename, "none-any.whl") {
			return &r, nil
		}
	}
	return nil, fs.ErrNotExist
}

func FindSourceDist(artifacts []pypireg.Artifact) (*pypireg.Artifact, error) {
	for _, r := range artifacts {
		if strings.HasSuffix(r.Filename, ".tar.gz") {
			return &r, nil
		}
	}
	return nil, fs.ErrNotExist
}

// FindPlatformWheel returns the first platform-specific wheel artifact from the given version's releases.
func FindPlatformWheel(artifacts []pypireg.Artifact) (*pypireg.Artifact, error) {
	for _, r := range artifacts {
		if strings.HasSuffix(r.Filename, ".whl") && !strings.HasSuffix(r.Filename, "none-any.whl") {
			if _, err := platform.ParsePlatformTags(extractPlatformTag(r.Filename)); err == nil {
				return &r, nil
			}
		}
	}
	return nil, fs.ErrNotExist
}

// WheelTags contains parsed PEP 427 wheel tags.
type WheelTags struct {
	Python   string
	ABI      string
	Platform string
}

// extractWheelTags extracts python tag, abi tag, and platform tag from a wheel filename (PEP 427).
func extractWheelTags(filename string) WheelTags {
	if !strings.HasSuffix(filename, ".whl") {
		return WheelTags{}
	}
	stem := strings.TrimSuffix(filename, ".whl")
	parts := strings.Split(stem, "-")
	if len(parts) >= 5 {
		return WheelTags{
			Python:   parts[len(parts)-3],
			ABI:      parts[len(parts)-2],
			Platform: parts[len(parts)-1],
		}
	}
	return WheelTags{}
}

// extractPlatformTag extracts the platform tag from a wheel filename.
func extractPlatformTag(filename string) string {
	return extractWheelTags(filename).Platform
}

func inferRequirements(name, version string, zr *zip.Reader) ([]string, error) {
	distInfoDir, err := getDistInfoDir(name, version, zr)
	if err != nil {
		wheelPath := path.Join(expectedDistInfoDir(name, version), "WHEEL")
		return nil, errors.Wrapf(err, "[INTERNAL] Failed to extract upstream %s", wheelPath)
	}
	wheelPath := path.Join(distInfoDir, "WHEEL")
	wheel, err := getFile(wheelPath, zr)
	if err != nil {
		return nil, errors.Wrapf(err, "[INTERNAL] Failed to extract upstream %s", wheelPath)
	}
	metadataPath := path.Join(distInfoDir, "METADATA")
	metadata, err := getFile(metadataPath, zr)
	if err != nil {
		return nil, errors.Wrapf(err, "[INTERNAL] Failed to extract upstream %s", metadataPath)
	}
	reqs, err := getGenerator(wheel, metadata)
	if err != nil {
		return nil, errors.Wrapf(err, "[INTERNAL] Failed to get upstream generator")
	}
	return reqs, nil
}

// normalizeName applies PyPA name normalization so that equivalent spellings
// of a distribution name, like "Flit.Core" and "flit_core", compare equal.
// https://packaging.python.org/en/latest/specifications/name-normalization/
func normalizeName(name string) string {
	return strings.ToLower(distInfoFieldPat.ReplaceAllString(name, "-"))
}

// Wheel dist-info names use escaped distribution/version components:
// https://packaging.python.org/en/latest/specifications/binary-distribution-format/#escaping-and-unicode
// Name comparisons use PyPA name normalization:
// https://packaging.python.org/en/latest/specifications/name-normalization/
func normalizeDistInfoName(name string) string {
	return strings.ReplaceAll(normalizeName(name), "-", "_")
}

func normalizeDistInfoVersion(version string) string {
	return strings.ReplaceAll(strings.ToLower(version), "-", "_")
}

func expectedDistInfoDir(name, version string) string {
	return fmt.Sprintf("%s-%s.dist-info", normalizeDistInfoName(name), normalizeDistInfoVersion(version))
}

func getDistInfoDir(name, version string, zr *zip.Reader) (string, error) {
	expectedDir := expectedDistInfoDir(name, version)
	if hasZipDir(expectedDir, zr) {
		return expectedDir, nil
	}
	// Older wheels may use equivalent but unescaped names with uppercase letters
	// or "." separators; the wheel spec requires consumers to accept them.
	for _, f := range zr.File {
		dir := path.Dir(f.Name)
		if dir == "." || path.Dir(dir) != "." {
			continue
		}
		stem, ok := strings.CutSuffix(dir, ".dist-info")
		if !ok {
			continue
		}
		dash := strings.LastIndexByte(stem, '-')
		if dash == -1 {
			continue
		}
		foundName, foundVersion := stem[:dash], stem[dash+1:]
		if normalizeDistInfoName(foundName) != normalizeDistInfoName(name) {
			continue
		}
		if normalizeDistInfoVersion(foundVersion) != normalizeDistInfoVersion(version) {
			continue
		}
		return dir, nil
	}
	return "", fs.ErrNotExist
}

func hasZipDir(dir string, zr *zip.Reader) bool {
	prefix := dir + "/"
	for _, f := range zr.File {
		if f.Name == dir || strings.HasPrefix(f.Name, prefix) {
			return true
		}
	}
	return false
}

// requirementName extracts the normalized distribution name from a dependency
// specifier, dropping extras, version specifiers, direct references and
// markers, so "SetupTools[core]<=67.7.2" yields "setuptools".
// https://packaging.python.org/en/latest/specifications/dependency-specifiers/#grammar
func requirementName(req string) string {
	fields := strings.FieldsFunc(req, func(r rune) bool { return strings.ContainsRune("=<>~!;[(@ \t", r) })
	if len(fields) == 0 {
		return ""
	}
	return normalizeName(fields[0])
}

// urlMarkerSepPat matches the separator before a direct reference's marker.
var urlMarkerSepPat = re.MustCompile(`[ \t]+;`)

// requirementMarker returns the environment marker of a dependency specifier,
// or "" if it has none.
// NOTE: URLs may contain ';', so the grammar requires whitespace before the
// marker of a direct reference. Names, extras and versions cannot contain '@'
// or ';', so an '@' before the first ';' marks a direct reference.
// https://packaging.python.org/en/latest/specifications/dependency-specifiers/#grammar
func requirementMarker(req string) string {
	semi := strings.IndexByte(req, ';')
	if at := strings.IndexByte(req, '@'); at != -1 && (semi == -1 || at < semi) {
		loc := urlMarkerSepPat.FindStringIndex(req[at:])
		if loc == nil {
			return ""
		}
		return strings.TrimSpace(req[at+loc[1]:])
	}
	if semi == -1 {
		return ""
	}
	return strings.TrimSpace(req[semi+1:])
}

// hasRequirement reports whether reqs name any of the packages under any
// specifier. The names must be normalized.
func hasRequirement(reqs []string, names ...string) bool {
	return slices.ContainsFunc(reqs, func(r string) bool { return slices.Contains(names, requirementName(r)) })
}

// mergeRequirements appends the buildReqs entries whose package does not
// already appear in reqs, also collapsing repeats within buildReqs itself.
// Entries of one package with different environment markers are all kept, as
// each takes effect in a different environment and the installer evaluates
// them against the build interpreter.
// https://packaging.python.org/en/latest/specifications/dependency-specifiers/#environment-markers
func mergeRequirements(reqs, buildReqs []string) []string {
	pinned := make(map[string]bool)
	for _, req := range reqs {
		pinned[requirementName(req)] = true
	}
	seen := make(map[string]bool)
	for _, newReq := range buildReqs {
		pkg := requirementName(newReq)
		if pkg == "" || pinned[pkg] {
			continue
		}
		if key := pkg + ";" + requirementMarker(newReq); !seen[key] {
			reqs = append(reqs, newReq)
			seen[key] = true
		}
	}
	return reqs
}

// backendRequirements returns dynamic build requirements requested by known
// backends when absent from the build image. Merge after pyproject
// requirements so project constraints take precedence.
func backendRequirements(reqs []string) []string {
	if hasRequirement(reqs, "meson-python", "scikit-build-core") {
		return []string{"ninja"}
	}
	return nil
}

func (Rebuilder) InferStrategy(ctx context.Context, t rebuild.Target, mux rebuild.RegistryMux, rcfg *rebuild.RepoConfig, hint rebuild.Strategy) (rebuild.Strategy, error) {
	name, version := t.Package, t.Version
	release, err := mux.PyPI.Release(ctx, name, version)
	if err != nil {
		return nil, err
	}
	// TODO: support different build types.
	cfg := &PureWheelBuild{}
	var a *pypireg.Artifact
	for _, art := range release.Artifacts {
		if art.Filename == t.Artifact {
			a = &art
			break
		}
	}
	if a == nil {
		return cfg, errors.Errorf("artifact %s not found in release", t.Artifact)
	}
	var ref, dir string
	lh, ok := hint.(*rebuild.LocationHint)
	if hint != nil && !ok {
		return nil, errors.Errorf("unsupported hint type: %T", hint)
	}
	if lh != nil && lh.Ref != "" {
		ref = lh.Ref
		if lh.Dir != "" {
			dir = lh.Dir
		} else {
			dir = rcfg.Dir
		}
	} else {
		ref, err = findGitRef(ctx, mux, release.Name, version, release, rcfg)
		if err != nil {
			return cfg, err
		}
		dir = rcfg.Dir
	}
	loc := rebuild.Location{Repo: rcfg.URI, Dir: dir, Ref: ref}
	s, err := inferBuild(ctx, t, mux, rcfg, release, a, loc)
	if err != nil {
		return nil, &rebuild.InferenceError{Detail: rebuild.InferenceErrorDetail{Location: loc, Published: a.UploadTime}, Err: err}
	}
	return s, nil
}

// inferBuild chooses the wheel or sdist build for a resolved location.
func inferBuild(ctx context.Context, t rebuild.Target, mux rebuild.RegistryMux, rcfg *rebuild.RepoConfig, release *pypireg.Release, a *pypireg.Artifact, loc rebuild.Location) (rebuild.Strategy, error) {
	name, version := t.Package, t.Version
	ref, dir := loc.Ref, loc.Dir
	log.Printf("Downloading artifact: %s", a.URL)
	r, err := mux.PyPI.Artifact(ctx, name, version, a.Filename)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, errors.Wrapf(err, "[INTERNAL] Failed to read upstream artifact")
	}
	var reqs, buildEnv []string
	var sysdepsList []sysdeps.DependencyIdentifier
	var zr *zip.Reader
	if strings.HasSuffix(a.Filename, ".whl") {
		zr, err = zip.NewReader(bytes.NewReader(body), a.Size)
		if err != nil {
			return nil, errors.Wrapf(err, "[INTERNAL] Failed to initialize upstream zip reader")
		}
		reqs, err = inferRequirements(release.Name, version, zr)
		if err != nil {
			return nil, err
		}
		wheelSysdeps, err := sysdeps.ExtractWheelElfDependencies(zr)
		if err != nil {
			log.Println(errors.Wrap(err, "extracting wheel ELF dependencies"))
		} else {
			sysdepsList = append(sysdepsList, wheelSysdeps...)
		}
	} else if strings.HasSuffix(a.Filename, ".tar.gz") {
		// For .tar.gz files (source distributions), we don't infer requirements from the archive
		// We'll get them from pyproject.toml below
		reqs = []string{}
	}
	var sanitizeSetupCfg bool
	var tree *object.Tree
	// Extract pyproject.toml requirements.
	{
		commit, err := rcfg.Repository.CommitObject(plumbing.NewHash(ref))
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to get commit object")
		}
		tree, err = commit.Tree()
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to get tree")
		}
		newFoundDir, err := pypiresolver.DiscoverBuildDir(ctx, tree, name, version, dir)
		if err != nil {
			log.Println(errors.Wrap(err, "Failed to discover build dir."))
		} else {
			// NOTE - This should NOT overwrite the hint dir if one exists, but utilize it and return it again
			//   Test "pyproject.toml - Detect package with dir hint" showcases this
			dir = newFoundDir
		}
		if buildReqs, err := pypiresolver.ExtractRequirements(ctx, tree, dir); err != nil {
			log.Println(errors.Wrap(err, "Failed to extract reqs from build files."))
		} else {
			reqs = mergeRequirements(reqs, buildReqs)
		}
		if dynReqs, err := pypiresolver.ExtractDynamicBuildRequirements(ctx, tree, dir, extractWheelTags(a.Filename).Python); err != nil {
			log.Println(errors.Wrap(err, "extracting dynamic build requirements"))
		} else {
			reqs = mergeRequirements(reqs, dynReqs)
		}
		reqs = mergeRequirements(reqs, backendRequirements(reqs))
		if cibwDeps, err := sysdeps.ExtractCibuildwheelDependencies(ctx, tree, dir); err != nil {
			log.Println(errors.Wrap(err, "extracting cibuildwheel dependencies"))
		} else {
			sysdepsList = append(sysdepsList, cibwDeps...)
		}
		if sanitize, err := pypiresolver.NeedsSetupCfgSanitize(ctx, tree, dir, version); err != nil {
			log.Println(errors.Wrap(err, "checking setup.cfg egg_info tags"))
		} else {
			sanitizeSetupCfg = sanitize
		}
		if zr != nil && !strings.HasSuffix(a.Filename, "none-any.whl") {
			var envReqs []string
			buildEnv, envReqs = inferWheelBuildEnv(tree, dir, zr, extractWheelTags(a.Filename), reqs)
			reqs = mergeRequirements(reqs, envReqs)
		}
	}
	if strings.HasSuffix(a.Filename, ".tar.gz") {
		return &SdistBuild{
			Location: rebuild.Location{
				Repo: rcfg.URI,
				Dir:  dir,
				Ref:  ref,
			},
			PythonVersion: inferPythonVersion(reqs, a.UploadTime),
			Requirements:  reqs,
			RegistryTime:  a.UploadTime,
		}, nil
	} else if strings.HasSuffix(a.Filename, ".whl") && !strings.HasSuffix(a.Filename, "none-any.whl") {
		tags := extractWheelTags(a.Filename)
		baseImageRepo, err := platform.SelectBaseImage(tags.Platform)
		if err != nil {
			return nil, errors.Wrapf(err, "unsupported platform tag in wheel filename %s", a.Filename)
		}
		var baseImage string
		if needsLegacyPythonBaseImage(tags, baseImageRepo) {
			cibwVersion, err := extractCibuildwheelVersionForPython(tree, tags.Python)
			if err != nil {
				log.Println(errors.Wrap(err, "extracting cibuildwheel version"))
			}
			baseImage = inferLegacyPythonBaseImage(tags, baseImageRepo, cibwVersion, a.UploadTime)
		}
		if tags.Python == "cp36" && tags.ABI != "abi3" {
			reqs = capSetuptoolsForCP36(reqs)
		}
		registryTime := advanceCoReleaseRegistryTime(ctx, mux, name, version, release, reqs, a.UploadTime)
		var rustVersion string
		var maturin *MaturinBuild
		if usesRust(reqs) {
			var needsSystemLLD bool
			rustVersion, needsSystemLLD = inferRustVersion(zr, tree, dir, registryTime)
			sysdepsList = append(sysdepsList, inferRustSysdeps(tags.Platform, needsSystemLLD, tree, dir)...)
			if hasRequirement(reqs, "maturin") {
				maturin = inferMaturinBuild(tree, dir, tags)
			}
		}
		return &PlatformWheelBuild{
			Location: rebuild.Location{
				Repo: rcfg.URI,
				Dir:  dir,
				Ref:  ref,
			},
			PythonTag:        tags.Python,
			ABITag:           tags.ABI,
			PlatformTag:      tags.Platform,
			BaseImage:        baseImage,
			Requirements:     reqs,
			Env:              buildEnv,
			SystemDeps:       sysdeps.DeduplicateIdentifiers(sysdepsList),
			RustVersion:      rustVersion,
			Maturin:          maturin,
			RegistryTime:     registryTime,
			SanitizeSetupCfg: sanitizeSetupCfg,
		}, nil
	} else {
		return &PureWheelBuild{
			Location: rebuild.Location{
				Repo: rcfg.URI,
				Dir:  dir,
				Ref:  ref,
			},
			PythonVersion: inferPythonVersion(reqs, a.UploadTime),
			Requirements:  reqs,
			RegistryTime:  a.UploadTime,
			PythonTag:     pythonTag(a.Filename, reqs),
		}, nil
	}
}

var (
	setupEnvGatePat = re.MustCompile(`os\.(?:getenv|environ\.get)\(\s*["']([A-Z0-9_]+_(?:USE_MYPYC|USE_CYTHON|CYTHON_ABI3))["'][^)]*\)\s*==\s*["']1["']`)
	cythonSpecPat   = re.MustCompile(`["'](Cython[><=~!][^"'\s]+)["']`)
)

// inferWheelBuildEnv inspects the upstream wheel and repository build files
// for environment variable gates and companion build requirements needed to
// compile mypyc or Cython extension modules.
func inferWheelBuildEnv(tree *object.Tree, dir string, zr *zip.Reader, tags WheelTags, reqs []string) ([]string, []string) {
	if tree == nil || zr == nil {
		return nil, nil
	}
	if !hasRequirement(reqs, "setuptools") || hasRequirement(reqs, "flit-core") {
		return nil, nil
	}
	var hasSO, hasMypycSO, hasABI3SO bool
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".so") || strings.Contains(f.Name, ".so.") {
			hasSO = true
			if strings.Contains(f.Name, "__mypyc") {
				hasMypycSO = true
			}
			if strings.HasSuffix(f.Name, ".abi3.so") {
				hasABI3SO = true
			}
		}
	}
	if !hasSO {
		return nil, nil
	}
	setupFile, err := tree.File(path.Join(dir, "setup.py"))
	if err != nil {
		return nil, nil
	}
	setupSrc, err := setupFile.Contents()
	if err != nil {
		return nil, nil
	}
	var env, extraReqs []string
	seenEnv := make(map[string]bool)
	usedMypyc, usedCython := false, false
	for _, m := range setupEnvGatePat.FindAllStringSubmatch(setupSrc, -1) {
		key := m[1]
		if seenEnv[key] {
			continue
		}
		switch {
		case strings.HasSuffix(key, "_USE_MYPYC"):
			if !hasMypycSO {
				continue
			}
			usedMypyc = true
		case strings.HasSuffix(key, "_USE_CYTHON"):
			usedCython = true
		case strings.HasSuffix(key, "_CYTHON_ABI3"):
			if tags.ABI != "abi3" && !hasABI3SO {
				continue
			}
		}
		seenEnv[key] = true
		env = append(env, key+"=1")
	}
	if usedMypyc && !hasRequirement(reqs, "mypy") {
		if _, err := tree.File(path.Join(dir, "mypyc/build.py")); err != nil {
			extraReqs = append(extraReqs, "mypy")
		}
	}
	if usedCython && !hasRequirement(reqs, "cython") {
		extraReqs = append(extraReqs, findCythonRequirement(tree, dir, setupSrc))
	}
	return env, extraReqs
}

// findCythonRequirement returns a version-constrained Cython requirement from
// setup.py or an in-tree build backend when present, defaulting to "Cython".
func findCythonRequirement(tree *object.Tree, dir, setupSrc string) string {
	if m := cythonSpecPat.FindStringSubmatch(setupSrc); m != nil {
		return m[1]
	}
	for _, rel := range []string{"_build_hook/backend.py", "packaging/pep517_backend/hooks.py"} {
		f, err := tree.File(path.Join(dir, rel))
		if err != nil {
			continue
		}
		src, err := f.Contents()
		if err != nil {
			continue
		}
		if m := cythonSpecPat.FindStringSubmatch(src); m != nil {
			return m[1]
		}
	}
	return "Cython"
}

// needsLegacyPythonBaseImage reports whether a platform wheel requires a
// historical container image pin because its CPython tag was removed from the
// unpinned latest PyPA image. abi3 wheels can build with newer interpreters,
// and musllinux_1_1_x86_64:latest remains frozen with cp36 through cp310.
func needsLegacyPythonBaseImage(tags WheelTags, baseImageRepo string) bool {
	if tags.ABI == "abi3" || baseImageRepo == platform.ImageMusllinux1_1X86_64 {
		return false
	}
	switch tags.Python {
	case "cp27", "cp36", "cp37", "cp38":
		return true
	default:
		return false
	}
}

// inferLegacyPythonBaseImage selects a pinned PyPA image that still ships the
// wheel's legacy CPython interpreter.
func inferLegacyPythonBaseImage(tags WheelTags, baseImageRepo, cibwVersion string, uploadTime time.Time) string {
	if tags.Python == "cp27" {
		return platform.ImageManylinux2010CP27X86_64
	}
	if tags.Python == "cp37" && strings.Contains(tags.Platform, "manylinux2010") {
		return platform.ImageManylinux2010FinalX86_64
	}
	if cibwVersion != "" {
		if ref, ok := platform.CibuildwheelImageForPython(baseImageRepo, cibwVersion, tags.Python); ok {
			return ref
		}
	}
	if ref, _, ok := platform.CibuildwheelImageAtForPython(baseImageRepo, tags.Python, uploadTime); ok {
		return ref
	}
	return ""
}

var (
	// setuptools 66.1.0 (released 2023-01-20) was the first release whose
	// pkg_resources stopped referencing pkgutil.ImpImporter, which Python 3.12
	// removed. Older setuptools fails to import on 3.12.
	setuptoolsImpImporterFixVersion = "66.1.0"
	setuptoolsImpImporterFixDate    = time.Date(2023, time.January, 20, 0, 0, 0, 0, time.UTC)
	// setuptools 70.1.0 merged bdist_wheel from the wheel project and was the
	// first release to write "Generator: setuptools (...)". Lower versions in
	// that stamp come from an older dist-info on sys.path (for example, cvxpy
	// 1.9.2).
	setuptoolsBdistWheelVersion = "70.1.0"
	// Upper and lower bounds within a PEP 440 version specifier.
	versionCeilingPat = re.MustCompile(`(<=?|==)\s*([\d.]+)`)
	versionFloorPat   = re.MustCompile(`(>=?|==|~=|===)\s*([\d.]+)`)
)

// hasCeilingBelow reports whether reqs constrain pkg to a version below limit.
// pkg must be normalized.
func hasCeilingBelow(reqs []string, pkg, limit string) bool {
	for _, req := range reqs {
		if requirementName(req) != pkg {
			continue
		}
		for _, m := range versionCeilingPat.FindAllStringSubmatch(req, -1) {
			if c := versionx.ApproxCompare(m[2], limit); c < 0 || c == 0 && m[1] == "<" {
				return true
			}
		}
	}
	return false
}

// hasFloorAtLeast reports whether reqs constrain pkg to a version at or above limit.
// pkg must be normalized.
func hasFloorAtLeast(reqs []string, pkg, limit string) bool {
	for _, req := range reqs {
		if requirementName(req) != pkg {
			continue
		}
		spec, _, _ := strings.Cut(req, ";")
		for _, m := range versionFloorPat.FindAllStringSubmatch(spec, -1) {
			if c := versionx.ApproxCompare(m[2], limit); c > 0 || c == 0 && m[1] != ">" {
				return true
			}
		}
	}
	return false
}

// inferPythonVersion pins Python 3.11 when the build may import a setuptools
// older than the ImpImporter fix: any upload predating it, since pip or the
// backend then resolve a contemporary setuptools whatever the backend, and a
// later upload only under a setuptools ceiling below the fix.
func inferPythonVersion(reqs []string, registryTime time.Time) string {
	if registryTime.Before(setuptoolsImpImporterFixDate) || hasCeilingBelow(reqs, "setuptools", setuptoolsImpImporterFixVersion) {
		return "3.11"
	}
	return ""
}

// py2WheelPat captures a wheel's python tag when it names a non-py3 language.
var py2WheelPat = re.MustCompile(`-(py2(?:\.py3)?)-[^-]+-[^-]+\.whl$`)

// pythonTag returns the tag setuptools has to be told to give the wheel, if any.
func pythonTag(filename string, reqs []string) string {
	if m := py2WheelPat.FindStringSubmatch(filename); m != nil && hasRequirement(reqs, "setuptools", "wheel") {
		return m[1]
	}
	return ""
}

var bdistWheelPat = re.MustCompile(`^Generator: bdist_wheel \(([\d\.]+)\)`)
var setuptoolsPat = re.MustCompile(`^Generator: setuptools \(([\d\.]+)\)`)
var flitPat = re.MustCompile(`^Generator: flit ([\d\.]+)`)
var hatchlingPat = re.MustCompile(`^Generator: hatchling ([\d\.]+)`)

// poetry-core is a subset of poetry. We can treat them as different builders.
var poetryPat = re.MustCompile(`^Generator: poetry ([\d\.]+)`)
var poetryCorePat = re.MustCompile(`^Generator: poetry-core ([\d\.]+)`)
var pdmBackendPat = re.MustCompile(`^Generator: pdm-backend \(([\d\.]+)\)`)
var uvBuildPat = re.MustCompile(`^Generator: uv ([\d\.]+)`)
var mesonPat = re.MustCompile(`^Generator: meson\s*$`)
var scikitBuildCorePat = re.MustCompile(`^Generator: scikit-build-core ([\d\.]+)`)
var maturinPat = re.MustCompile(`^Generator: maturin \(([\d\.]+)\)`)

// getGenerator returns the pins identifying the wheel's build backend from its
// Generator line. bdist_wheel names the wheel packaging tool, not setuptools,
// whose version is instead bounded by the metadata era.
func getGenerator(wheel, metadata []byte) (reqs []string, err error) {
	var eol int
	for i := 0; i < len(wheel); i = eol + 1 {
		eol = bytes.IndexRune(wheel[i:], '\n')
		line := wheel[i : i+eol+1]
		sep := bytes.IndexRune(line, ':')
		if sep == -1 {
			// Each line in a WHEEL file has a `key: value` format.
			return nil, errors.New("Unexpected file format")
		}
		key, value := line[:sep], bytes.TrimSpace(line[sep+1:])
		if bytes.Equal(key, []byte("Generator")) {
			if matches := bdistWheelPat.FindSubmatch(line); matches != nil {
				return []string{"wheel==" + string(matches[1]), setuptoolsCeiling(metadata)}, nil
			} else if matches := setuptoolsPat.FindSubmatch(line); matches != nil {
				ver := string(matches[1])
				if versionx.ApproxCompare(ver, setuptoolsBdistWheelVersion) < 0 {
					return []string{"setuptools>=" + setuptoolsBdistWheelVersion}, nil
				}
				return []string{"setuptools==" + ver}, nil
			} else if matches := flitPat.FindSubmatch(line); matches != nil {
				return []string{"flit_core==" + string(matches[1]), "flit==" + string(matches[1])}, nil
			} else if matches := hatchlingPat.FindSubmatch(line); matches != nil {
				return []string{"hatchling==" + string(matches[1])}, nil
			} else if matches := poetryPat.FindSubmatch(line); matches != nil {
				return []string{"poetry==" + string(matches[1])}, nil
			} else if matches := poetryCorePat.FindSubmatch(line); matches != nil {
				return []string{"poetry-core==" + string(matches[1])}, nil
			} else if matches := pdmBackendPat.FindSubmatch(line); matches != nil {
				return []string{"pdm-backend==" + string(matches[1])}, nil
			} else if matches := uvBuildPat.FindSubmatch(line); matches != nil {
				return []string{"uv-build==" + string(matches[1])}, nil
			} else if mesonPat.Match(line) {
				// meson-python writes a bare "Generator: meson" without a version.
				// Return no pins so pyproject.toml constraints take effect.
				return []string{}, nil
			} else if matches := scikitBuildCorePat.FindSubmatch(line); matches != nil {
				return []string{"scikit-build-core==" + string(matches[1])}, nil
			} else if matches := maturinPat.FindSubmatch(line); matches != nil {
				return []string{"maturin==" + string(matches[1])}, nil
			} else {
				return nil, errors.Errorf("unsupported generator: %s", value)
			}
		}
	}
	return nil, errors.New("no generator found")
}

// setuptoolsCeiling bounds a bdist_wheel build's setuptools by its metadata era.
// NOTE: These version ceilings pin with <= rather than == as timewarp will
// fail to resolve equality constraints where registry_time is configured
// before that version's release (which has been observed).
func setuptoolsCeiling(metadata []byte) string {
	switch {
	case !bytes.Contains(metadata, []byte("License-File")):
		// License-File was introduced in later versions, bounding this above.
		return "setuptools<=56.2.0"
	case bytes.Contains(metadata, []byte("Platform: UNKNOWN")):
		// Later versions omit the unknown platform, so this is an older setuptools.
		return "setuptools<=57.5.0"
	default:
		return "setuptools<=67.7.2"
	}
}

// setuptools 59.6.0 is the final release supporting Python 3.6.
const setuptoolsCP36MaxVersion = "59.6.0"

// capSetuptoolsForCP36 tightens the default bdist_wheel setuptools ceiling to
// the last Python 3.6-compatible release so pip 18.1 with --upgrade resolves a
// version that imports on Python 3.6.
func capSetuptoolsForCP36(reqs []string) []string {
	out := slices.Clone(reqs)
	for i, req := range out {
		if req == "setuptools<=67.7.2" {
			out[i] = "setuptools<=" + setuptoolsCP36MaxVersion
		}
	}
	return out
}

func getFile(fname string, zr *zip.Reader) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == fname {
			fi, err := zr.Open(f.Name)
			if err != nil {
				return nil, err
			}
			return io.ReadAll(fi)
		}
	}
	return nil, fs.ErrNotExist
}
