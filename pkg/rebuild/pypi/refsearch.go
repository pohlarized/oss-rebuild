// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"io"
	"log"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/oss-rebuild/internal/versionx"
	pypiresolver "github.com/google/oss-rebuild/pkg/rebuild/pypi/parsing"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	pypireg "github.com/google/oss-rebuild/pkg/registry/pypi"
	"github.com/google/oss-rebuild/pkg/vcs/gitscan"
	"github.com/pkg/errors"
)

func shortHash(h string) string {
	if len(h) > 9 {
		return h[:9]
	}
	return h
}

// findWheel returns the target artifact if it is a wheel, or the pure wheel,
// or any wheel in the release.
func findWheel(artifacts []pypireg.Artifact, target string) (*pypireg.Artifact, error) {
	if target != "" && strings.HasSuffix(target, ".whl") {
		for _, a := range artifacts {
			if a.Filename == target {
				return &a, nil
			}
		}
	}
	if pure, err := FindPureWheel(artifacts); err == nil {
		return pure, nil
	}
	for _, a := range artifacts {
		if strings.HasSuffix(a.Filename, ".whl") {
			return &a, nil
		}
	}
	return nil, errors.New("no wheel in release")
}

// archiveContentRef matches the wheel's file blobs against commit trees:
// a wheel built from commit C contains C's file contents verbatim, so C's tree
// shares those blob hashes. Works without a declared version.
func archiveContentRef(ctx context.Context, mux rebuild.RegistryMux, pkg, version string, release *pypireg.Release, target, tagRef string, repo *git.Repository) (string, error) {
	wheel, err := findWheel(release.Artifacts, target)
	if err != nil {
		return "", errors.Wrap(err, "no wheel")
	}
	rc, err := mux.PyPI.Artifact(ctx, pkg, version, wheel.Filename)
	if err != nil {
		return "", errors.Wrap(err, "downloading wheel")
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return "", errors.Wrap(err, "reading wheel")
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", errors.Wrap(err, "opening wheel zip")
	}
	hashes, err := gitscan.BlobHashesFromZip(zr)
	if err != nil {
		return "", errors.Wrap(err, "hashing wheel contents")
	}
	return matchArchiveBlobs(ctx, hashes, pkg, version, tagRef, repo)
}

// matchArchiveBlobs returns the commit whose tree contains the most of the
// given blobs, via gitscan.ExactTreeCount, as vetted and tie-broken by
// pickByDeclaredVersion. If tagRef is among the best-overlap commits, it is
// preferred; otherwise, the best-overlap commit declaring the version is preferred.
func matchArchiveBlobs(ctx context.Context, hashes []plumbing.Hash, pkg, version, tagRef string, repo *git.Repository) (string, error) {
	closest, matched, total, err := gitscan.ExactTreeCount{}.Search(ctx, repo, hashes)
	if err != nil {
		return "", errors.Wrap(err, "searching trees for blob overlap")
	}
	if tagRef != "" && slices.Contains(closest, tagRef) {
		log.Printf("tag matches best blob overlap [pkg=%s,ver=%s,blobs=%d/%d,ref=%s]\n", pkg, version, matched, total, shortHash(tagRef))
		return tagRef, nil
	}
	candidates := make([]*object.Commit, 0, len(closest))
	for _, h := range closest {
		c, err := repo.CommitObject(plumbing.NewHash(h))
		if err != nil {
			return "", errors.Wrap(err, "resolving best-overlap commit")
		}
		candidates = append(candidates, c)
	}
	pick := pickByDeclaredVersion(ctx, candidates, pkg, version, tagRef)
	if pick == nil {
		if tagRef != "" {
			return tagRef, nil
		}
		return "", errors.Errorf("no version-consistent blob overlap candidate [best=%d,total=%d,ties=%d]", matched, total, len(candidates))
	}
	ref := pick.Hash.String()
	if tagRef != "" {
		log.Printf("tag %s had fewer blob matches than archive-content commit %s [pkg=%s,ver=%s,blobs=%d/%d,ties=%d]\n", shortHash(tagRef), shortHash(ref), pkg, version, matched, total, len(candidates))
	} else {
		log.Printf("archive-content match [pkg=%s,ver=%s,blobs=%d/%d,ties=%d,ref=%s]\n", pkg, version, matched, total, len(candidates), shortHash(ref))
	}
	return ref, nil
}

// pickByDeclaredVersion chooses among commits tied on blob overlap by the version
// each one's build file declares, read where the previous candidate's build
// file was found and then anywhere in its tree. A build file confirming
// version keeps its commit in the running, one declaring none leaves it
// neutral, and one naming another version drops it. Commit time then orders
// the survivors: the latest confirming candidate, the version's final state,
// else the latest neutral one before tag when tagRef is set (or earliest when untagged), else nil.
// Spellings are compared under the approximate ordering, since PyPI
// canonicalizes them.
func pickByDeclaredVersion(ctx context.Context, candidates []*object.Commit, pkg, version, tagRef string) *object.Commit {
	var confirming, neutral []*object.Commit
	var dir string
	for _, c := range candidates {
		var declared, found string
		if tree, err := c.Tree(); err == nil {
			declared, found = pypiresolver.FindDeclaredVersion(ctx, tree, dir, pkg)
		}
		dir = cmp.Or(found, dir)
		switch {
		case declared == "":
			neutral = append(neutral, c)
		case versionx.ApproxCompare(declared, version) == 0:
			confirming = append(confirming, c)
		}
	}
	switch {
	case len(confirming) > 0:
		return slices.MaxFunc(confirming, byCommitTime)
	case len(neutral) > 0:
		if tagRef != "" {
			return slices.MaxFunc(neutral, byCommitTime)
		}
		return slices.MinFunc(neutral, byCommitTime)
	}
	return nil
}

func byCommitTime(a, b *object.Commit) int {
	return a.Committer.When.Compare(b.Committer.When)
}
