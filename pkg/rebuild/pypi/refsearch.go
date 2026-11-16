// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"io"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	pypiresolver "github.com/google/oss-rebuild/pkg/rebuild/pypi/parsing"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	pypireg "github.com/google/oss-rebuild/pkg/registry/pypi"
	"github.com/google/oss-rebuild/pkg/vcs/gitscan"
	"github.com/msuozzo/go-git2"
	"github.com/pkg/errors"
)

type tagRank int

const (
	tagNone          tagRank = iota // untagged or naming a different release
	tagUnversioned                  // tag carrying no version or build number
	tagMatchesTarget                // tag naming the target version or build number
)

var (
	epochPrefixPat      = regexp.MustCompile(`^\d+!`)
	baseReleasePat      = regexp.MustCompile(`^\d+(?:\.\d+)*`)
	plainReleasePat     = regexp.MustCompile(`^\d+(?:\.\d+)*$`)
	bareDigitsPat       = regexp.MustCompile(`^\d+$`)
	dottedTagVersionPat = regexp.MustCompile(`(?i)\d+(?:\.\d+)+(?:[._-]?(?:alpha|beta|preview|pre|post|dev|rc|a|b|c)\d*|\+[a-z0-9._-]+)*`)
	vPrefixSingleVerPat = regexp.MustCompile(`(?i)^v\d+(?:[._-]?(?:alpha|beta|preview|pre|post|dev|rc|a|b|c)\d*)*$`)
)

func shortHash(h string) string {
	if len(h) > 9 {
		return h[:9]
	}
	return h
}

// archiveContentRef matches a release archive's file blobs against commit
// trees: an archive built from commit C contains C's file contents verbatim,
// so C's tree shares those blob hashes. It reads the pure wheel, else the
// sdist, which holds the most source files, else a platform wheel. Works
// without a declared version.
func archiveContentRef(ctx context.Context, mux rebuild.RegistryMux, pkg, version string, release *pypireg.Release, repo *git2.Repository) (string, error) {
	a, err := firstReleaseArchive(release.Artifacts, FindPureWheel, FindSourceDist, FindPlatformWheel)
	if err != nil {
		return "", err
	}
	hashes, err := downloadArchiveBlobs(ctx, mux, pkg, version, a)
	if err != nil {
		return "", err
	}
	return matchArchiveBlobs(ctx, hashes, pkg, version, a.UploadTime, repo)
}

// resolveContradictingTag checks a tag commit whose static metadata declares
// another version against the release's archive blobs, preferring the sdist.
// It returns a version-consistent archive match when one exists, keeps
// tagCommit when an sdist confirms it has the best positive blob overlap
// within the upload window (as in CI-stamped releases with a stale committed
// version), and returns an error otherwise.
func resolveContradictingTag(ctx context.Context, mux rebuild.RegistryMux, pkg, version string, release *pypireg.Release, repo *git2.Repository, tagCommit *git2.Commit) (string, error) {
	a, err := firstReleaseArchive(release.Artifacts, FindSourceDist, FindPureWheel, FindPlatformWheel)
	if err != nil {
		return "", err
	}
	hashes, err := downloadArchiveBlobs(ctx, mux, pkg, version, a)
	if err != nil {
		return "", err
	}
	candidates, matched, total, err := searchClosestCommits(ctx, repo, hashes)
	if err != nil {
		return "", err
	}
	tagHasBestOverlap := matched > 0 && slices.ContainsFunc(candidates, func(c *git2.Commit) bool { return c.Hash == tagCommit.Hash })
	var tagged map[git2.Hash]tagRank
	if len(candidates) > 1 {
		if tagged, err = taggedCommitsForVersion(repo, pkg, version); err != nil {
			return "", err
		}
	}
	pick, err := pickByDeclaredVersion(ctx, candidates, pkg, version, a.UploadTime, tagged)
	if err != nil {
		return "", err
	}
	if pick != nil {
		ref := pick.Hash.String()
		log.Printf("archive-content match [pkg=%s,ver=%s,blobs=%d/%d,ties=%d,ref=%s]\n", pkg, version, matched, total, len(candidates), shortHash(ref))
		return ref, nil
	}
	isSdist := strings.HasSuffix(a.Filename, ".tar.gz")
	inUploadWindow := a.UploadTime.IsZero() || !tagCommit.Author.When.After(a.UploadTime.Add(24*time.Hour))
	if isSdist && tagHasBestOverlap && inUploadWindow {
		return tagCommit.Hash.String(), nil
	}
	return "", errors.Errorf("no version-consistent blob overlap candidate [best=%d,total=%d,ties=%d]", matched, total, len(candidates))
}

type artifactFinder func([]pypireg.Artifact) (*pypireg.Artifact, error)

func firstReleaseArchive(artifacts []pypireg.Artifact, finders ...artifactFinder) (*pypireg.Artifact, error) {
	var err error
	for _, find := range finders {
		var a *pypireg.Artifact
		if a, err = find(artifacts); err == nil {
			return a, nil
		}
	}
	return nil, errors.Wrap(err, "no release archive")
}

func downloadArchiveBlobs(ctx context.Context, mux rebuild.RegistryMux, pkg, version string, a *pypireg.Artifact) ([]plumbing.Hash, error) {
	rc, err := mux.PyPI.Artifact(ctx, pkg, version, a.Filename)
	if err != nil {
		return nil, errors.Wrap(err, "downloading archive")
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, errors.Wrap(err, "reading archive")
	}
	hashes, err := archiveBlobs(a.Filename, body)
	if err != nil {
		return nil, errors.Wrap(err, "hashing archive contents")
	}
	return hashes, nil
}

// archiveBlobs returns the blob hashes of the files in a wheel or sdist, except
// a wheel's .dist-info, which the build generates, and empty files, which are
// often markers like py.typed and do not predict the commit.
func archiveBlobs(name string, body []byte) ([]plumbing.Hash, error) {
	var hashes []plumbing.Hash
	if strings.HasSuffix(name, ".tar.gz") {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if hashes, err = gitscan.BlobHashesFromTar(tar.NewReader(gz)); err != nil {
			return nil, err
		}
	} else {
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, err
		}
		zr.File = slices.DeleteFunc(zr.File, func(f *zip.File) bool { return strings.Contains(f.Name, ".dist-info/") })
		if hashes, err = gitscan.BlobHashesFromZip(zr); err != nil {
			return nil, err
		}
	}
	empty := plumbing.ComputeHash(plumbing.BlobObject, nil)
	return slices.DeleteFunc(hashes, func(h plumbing.Hash) bool { return h == empty }), nil
}

// matchArchiveBlobs returns the commit whose tree contains the most of the
// given blobs, via gitscan.ReachableTreeCount, as vetted and tie-broken by
// pickByDeclaredVersion for an archive uploaded at uploaded.
func matchArchiveBlobs(ctx context.Context, hashes []plumbing.Hash, pkg, version string, uploaded time.Time, repo *git2.Repository) (string, error) {
	candidates, matched, total, err := searchClosestCommits(ctx, repo, hashes)
	if err != nil {
		return "", err
	}
	var tagged map[git2.Hash]tagRank
	if len(candidates) > 1 {
		if tagged, err = taggedCommitsForVersion(repo, pkg, version); err != nil {
			return "", err
		}
	}
	pick, err := pickByDeclaredVersion(ctx, candidates, pkg, version, uploaded, tagged)
	if err != nil {
		return "", err
	}
	if pick == nil {
		return "", errors.Errorf("no version-consistent blob overlap candidate [best=%d,total=%d,ties=%d]", matched, total, len(candidates))
	}
	ref := pick.Hash.String()
	log.Printf("archive-content match [pkg=%s,ver=%s,blobs=%d/%d,ties=%d,ref=%s]\n", pkg, version, matched, total, len(candidates), shortHash(ref))
	return ref, nil
}

func searchClosestCommits(ctx context.Context, repo *git2.Repository, hashes []plumbing.Hash) (candidates []*git2.Commit, matched, total int, err error) {
	blobs := make([]git2.Hash, len(hashes))
	for i, h := range hashes {
		blobs[i] = git2.Hash(h)
	}
	closest, matched, total, err := gitscan.ReachableTreeCount{}.Search(ctx, repo, blobs)
	if err != nil {
		return nil, 0, 0, errors.Wrap(err, "searching trees for blob overlap")
	}
	candidates = make([]*git2.Commit, 0, len(closest))
	for _, h := range closest {
		c, err := repo.CommitObject(git2.NewHash(h))
		if err != nil {
			return nil, 0, 0, errors.Wrap(err, "resolving best-overlap commit")
		}
		candidates = append(candidates, c)
	}
	return candidates, matched, total, nil
}

// maxVettedTies bounds how many tied commits pickByDeclaredVersion reads, so
// that files left unchanged across a long history stay cheap to vet.
const maxVettedTies = 1000

// pickByDeclaredVersion chooses among commits tied on blob overlap by the version
// each one's build file declares, read where the previous candidate's build
// file was found and then anywhere in its tree. A build file confirming
// version keeps its commit in the running, one declaring none (or a CI
// placeholder) leaves it neutral, and one naming another version drops it.
// Tag rank orders survivors ahead of commit time so git-describe builds see a
// matching tag before an unversioned tag or an untagged commit. Within the
// top rank, commit time picks the latest confirming candidate (the version's
// final state) or else the earliest neutral one (where the content was
// introduced). Spellings are compared under the approximate ordering, since
// PyPI canonicalizes them.
//
// Candidates authored over a day after uploaded are too late to have built the
// archive and are dropped, as author time survives rebases. The rest are read
// highest-ranked and newest first, up to maxVettedTies, until one confirms
// version.
func pickByDeclaredVersion(ctx context.Context, candidates []*git2.Commit, pkg, version string, uploaded time.Time, tagged map[git2.Hash]tagRank) (*git2.Commit, error) {
	candidates = slices.Clone(candidates)
	if !uploaded.IsZero() {
		cutoff := uploaded.Add(24 * time.Hour)
		candidates = slices.DeleteFunc(candidates, func(c *git2.Commit) bool { return c.Author.When.After(cutoff) })
	}
	slices.SortStableFunc(candidates, func(a, b *git2.Commit) int {
		if rc := cmp.Compare(tagged[b.Hash], tagged[a.Hash]); rc != 0 {
			return rc
		}
		return byCommitTime(b, a)
	})
	var neutral *git2.Commit
	var dir string
	for _, c := range candidates[:min(len(candidates), maxVettedTies)] {
		tree, err := c.Tree()
		if err != nil {
			return nil, errors.Wrap(err, "reading candidate tree")
		}
		declared, found := pypiresolver.FindDeclaredVersion(ctx, tree, dir, pkg)
		dir = cmp.Or(found, dir)
		// NOTE: Placeholder releases count as no declaration, as in declaredVersionContradicts,
		// unless they are the target release itself.
		if base, ok := parseBaseRelease(declared); ok && isPlaceholderVersion(base) && !pypiresolver.SameVersion(declared, version) {
			declared = ""
		}
		switch {
		case declared == "" && (neutral == nil || tagged[c.Hash] == tagged[neutral.Hash] && byCommitTime(c, neutral) < 0):
			neutral = c
		case declared != "" && pypiresolver.SameVersion(declared, version):
			return c, nil
		}
	}
	return neutral, nil
}

func byCommitTime(a, b *git2.Commit) int {
	return a.Committer.When.Compare(b.Committer.When)
}

// taggedCommitsForVersion maps commits pointed to by version-compatible tags
// to their match rank, peeling nested annotated tags via ResolveRevision. Tags
// naming a different version, pre/post/dev suffix, or bare build number are
// excluded.
func taggedCommitsForVersion(repo *git2.Repository, pkg, version string) (map[git2.Hash]tagRank, error) {
	iter, err := repo.Tags()
	if err != nil {
		return nil, errors.Wrap(err, "listing tags")
	}
	lastSeg, hasLastSeg := trailingReleaseSegment(version)
	tagged := make(map[git2.Hash]tagRank)
	err = iter.ForEach(func(ref *git2.Reference) error {
		name := ref.Name().Short()
		rank := versionTagRank(name, pkg, version, lastSeg, hasLastSeg)
		if rank == tagNone {
			return nil
		}
		commit, err := repo.ResolveRevision(git2.Revision(ref.Hash().String()))
		if err != nil {
			if errors.Is(err, git2.ErrReferenceNotFound) && isNonCommitObject(repo, ref.Hash()) {
				return nil
			}
			return errors.Wrapf(err, "peeling tag %s", name)
		}
		tagged[*commit] = max(tagged[*commit], rank)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tagged, nil
}

func versionTagRank(tag, pkg, version string, lastSeg int, hasLastSeg bool) tagRank {
	if strict, _ := rebuild.MatchTag(tag, pkg, version); strict {
		return tagMatchesTarget
	}
	if bareDigitsPat.MatchString(tag) {
		n, err := strconv.Atoi(tag)
		if err == nil && hasLastSeg && n == lastSeg {
			return tagMatchesTarget
		}
		return tagNone
	}
	if tagVers := dottedTagVersionPat.FindAllString(tag, -1); len(tagVers) > 0 {
		if slices.ContainsFunc(tagVers, func(tv string) bool { return pypiresolver.SameVersion(tv, version) }) {
			return tagMatchesTarget
		}
		return tagNone
	}
	if vPrefixSingleVerPat.MatchString(tag) {
		if pypiresolver.SameVersion(tag[1:], version) {
			return tagMatchesTarget
		}
		return tagNone
	}
	return tagUnversioned
}

func trailingReleaseSegment(version string) (int, bool) {
	s := strings.TrimSpace(version)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	s = epochPrefixPat.ReplaceAllString(s, "")
	if !plainReleasePat.MatchString(s) {
		return 0, false
	}
	if dot := strings.LastIndexByte(s, '.'); dot >= 0 {
		s = s[dot+1:]
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// isNonCommitObject reports whether h is a present tree or blob, or an
// annotated tag directly targeting one, which ResolveRevision rejects as
// ErrReferenceNotFound despite a healthy object database.
func isNonCommitObject(repo *git2.Repository, h git2.Hash) bool {
	t, _, err := repo.ObjectHeader(h)
	if err != nil {
		return false
	}
	if t == git2.BlobObject || t == git2.TreeObject {
		return true
	}
	tag, err := repo.TagObject(h)
	return err == nil && (tag.TargetType == git2.BlobObject || tag.TargetType == git2.TreeObject)
}

// declaredVersionContradicts reports whether commit statically declares a
// non-placeholder release version whose numeric base release differs from
// version. Dynamic, missing, unparseable, or placeholder versions return false.
func declaredVersionContradicts(ctx context.Context, commit *git2.Commit, dir, pkg, version string) (bool, error) {
	targetBase, ok := parseBaseRelease(version)
	if !ok {
		return false, nil
	}
	tree, err := commit.Tree()
	if err != nil {
		return false, errors.Wrap(err, "reading commit tree")
	}
	declared, _ := pypiresolver.FindDeclaredVersion(ctx, tree, dir, pkg)
	declaredBase, ok := parseBaseRelease(declared)
	if !ok || isPlaceholderVersion(declaredBase) {
		return false, nil
	}
	return !slices.Equal(declaredBase, targetBase), nil
}

// parseBaseRelease returns the PEP 440 release segment without trailing zeros,
// ignoring pre-, post-, and dev-release suffixes so 1.2.0 and 1.2rc1 match 1.2.
func parseBaseRelease(v string) ([]int, bool) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	s = epochPrefixPat.ReplaceAllString(s, "")
	m := baseReleasePat.FindString(s)
	if m == "" {
		return nil, false
	}
	parts := strings.Split(m, ".")
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		nums[i] = n
	}
	for len(nums) > 1 && nums[len(nums)-1] == 0 {
		nums = nums[:len(nums)-1]
	}
	return nums, true
}

// isPlaceholderVersion reports whether base is a dummy release number (0.0.0,
// 0.0.1, or 0.1.0) left in build files when CI stamps the real version.
func isPlaceholderVersion(base []int) bool {
	return slices.Equal(base, []int{0}) || slices.Equal(base, []int{0, 0, 1}) || slices.Equal(base, []int{0, 1})
}
