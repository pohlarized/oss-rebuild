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
	"fmt"
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

// NOTE: Bounds memory and scan time when hashing an archive for the ref heuristic.
const maxArchiveContentSize = 256 << 20

type archiveFormat int

const (
	zipFormat archiveFormat = iota + 1
	tarGzFormat
)

func archiveFormatOf(filename string) (archiveFormat, bool) {
	switch {
	case strings.HasSuffix(filename, ".whl"), strings.HasSuffix(filename, ".zip"):
		return zipFormat, true
	case strings.HasSuffix(filename, ".tar.gz"):
		return tarGzFormat, true
	default:
		return 0, false
	}
}

// sourceArchives returns the release archives suitable for content-based commit
// matching: the pure wheel first to keep existing refs stable, followed by the
// source distribution (.tar.gz then .zip).
func sourceArchives(artifacts []pypireg.Artifact) []pypireg.Artifact {
	var out []pypireg.Artifact
	if wheel, err := FindPureWheel(artifacts); err == nil {
		out = append(out, *wheel)
	}
	if sdist, err := FindSourceDist(artifacts); err == nil {
		out = append(out, *sdist)
	}
	for _, r := range artifacts {
		if strings.HasSuffix(r.Filename, ".zip") {
			out = append(out, r)
			break
		}
	}
	return out
}

func archiveBlobHashes(ctx context.Context, mux rebuild.RegistryMux, pkg, version string, a pypireg.Artifact) ([]plumbing.Hash, error) {
	if a.Size > maxArchiveContentSize {
		return nil, errors.Errorf("archive exceeds max size [file=%s,size=%d]", a.Filename, a.Size)
	}
	format, ok := archiveFormatOf(a.Filename)
	if !ok {
		return nil, errors.Errorf("unsupported archive format [file=%s]", a.Filename)
	}
	rc, err := mux.PyPI.Artifact(ctx, pkg, version, a.Filename)
	if err != nil {
		return nil, errors.Wrapf(err, "downloading %s", a.Filename)
	}
	defer rc.Close()
	switch format {
	case zipFormat:
		body, err := io.ReadAll(rc)
		if err != nil {
			return nil, errors.Wrapf(err, "reading %s", a.Filename)
		}
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, errors.Wrapf(err, "opening zip %s", a.Filename)
		}
		hashes, err := gitscan.BlobHashesFromZip(zr)
		if err != nil {
			return nil, errors.Wrapf(err, "hashing zip %s", a.Filename)
		}
		return hashes, nil
	case tarGzFormat:
		gr, err := gzip.NewReader(rc)
		if err != nil {
			return nil, errors.Wrapf(err, "opening gzip %s", a.Filename)
		}
		defer gr.Close()
		hashes, err := gitscan.BlobHashesFromTar(tar.NewReader(gr))
		if err != nil {
			return nil, errors.Wrapf(err, "hashing tar %s", a.Filename)
		}
		return hashes, nil
	default:
		return nil, errors.Errorf("unhandled archive format [file=%s]", a.Filename)
	}
}

func shortHash(h string) string {
	if len(h) > 9 {
		return h[:9]
	}
	return h
}

// archiveContentRef matches the pure wheel or sdist file blobs against commit
// trees: an archive built from commit C contains C's file contents verbatim, so
// C's tree shares those blob hashes. Works without a declared version.
func archiveContentRef(ctx context.Context, mux rebuild.RegistryMux, pkg, version string, release *pypireg.Release, repo *git.Repository) (string, error) {
	archives := sourceArchives(release.Artifacts)
	if len(archives) == 0 {
		return "", errors.New("no source archive")
	}
	var reasons []string
	for _, a := range archives {
		hashes, err := archiveBlobHashes(ctx, mux, pkg, version, a)
		if err != nil {
			log.Printf("archive-content candidate failed [pkg=%s,ver=%s,file=%s]: %v", pkg, version, a.Filename, err)
			reasons = append(reasons, fmt.Sprintf("%s: %v", a.Filename, err))
			continue
		}
		ref, err := matchArchiveBlobs(ctx, hashes, pkg, version, repo)
		if err != nil {
			log.Printf("archive-content candidate failed [pkg=%s,ver=%s,file=%s]: %v", pkg, version, a.Filename, err)
			reasons = append(reasons, fmt.Sprintf("%s: %v", a.Filename, err))
			continue
		}
		return ref, nil
	}
	return "", errors.New(strings.Join(reasons, ", "))
}

// matchArchiveBlobs returns the commit whose tree contains the most of the
// given blobs, via gitscan.ExactTreeCount, as vetted and tie-broken by
// pickByDeclaredVersion.
func matchArchiveBlobs(ctx context.Context, hashes []plumbing.Hash, pkg, version string, repo *git.Repository) (string, error) {
	closest, matched, total, err := gitscan.ExactTreeCount{}.Search(ctx, repo, hashes)
	if err != nil {
		return "", errors.Wrap(err, "searching trees for blob overlap")
	}
	candidates := make([]*object.Commit, 0, len(closest))
	for _, h := range closest {
		c, err := repo.CommitObject(plumbing.NewHash(h))
		if err != nil {
			return "", errors.Wrap(err, "resolving best-overlap commit")
		}
		candidates = append(candidates, c)
	}
	pick := pickByDeclaredVersion(ctx, candidates, pkg, version)
	if pick == nil {
		return "", errors.Errorf("no version-consistent blob overlap candidate [best=%d,total=%d,ties=%d]", matched, total, len(candidates))
	}
	ref := pick.Hash.String()
	log.Printf("archive-content match [pkg=%s,ver=%s,blobs=%d/%d,ties=%d,ref=%s]\n", pkg, version, matched, total, len(candidates), shortHash(ref))
	return ref, nil
}

// pickByDeclaredVersion chooses among commits tied on blob overlap by the version
// each one's build file declares, read where the previous candidate's build
// file was found and then anywhere in its tree. A build file confirming
// version keeps its commit in the running, one declaring none leaves it
// neutral, and one naming another version drops it. Commit time then orders
// the survivors: the latest confirming candidate, the version's final state,
// else the earliest neutral one, where the content was introduced, else nil.
// Spellings are compared under the approximate ordering, since PyPI
// canonicalizes them.
func pickByDeclaredVersion(ctx context.Context, candidates []*object.Commit, pkg, version string) *object.Commit {
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
		return slices.MinFunc(neutral, byCommitTime)
	}
	return nil
}

func byCommitTime(a, b *object.Commit) int {
	return a.Committer.When.Compare(b.Committer.When)
}
