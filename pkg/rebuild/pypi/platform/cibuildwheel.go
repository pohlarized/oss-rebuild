// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"time"

	"github.com/google/oss-rebuild/internal/semver"
)

//go:generate go run gen_cibuildwheel_images.go

// cibuildwheelRelease records the build images that a cibuildwheel release pins.
type cibuildwheelRelease struct {
	// Version is the release version, such as "3.4.1".
	Version string
	// Published is when the release was uploaded to PyPI.
	Published time.Time
	// Commit is the hash of the commit that the release tag points to.
	Commit string
	// Images maps the supported image repositories that the release pins to
	// the pinned images.
	Images map[string]pinnedImage
}

// pinnedImage is a build image that a cibuildwheel release pins.
type pinnedImage struct {
	// Tag is the pinned tag. It is empty if the release pins by digest only.
	Tag string
	// Digest is the manifest digest of the image. It is empty if the tag had
	// expired before its digest was recorded.
	Digest string
}

// CibuildwheelImageAt returns the digest-pinned reference to the repo image
// that the highest cibuildwheel version published at or before t pins, along
// with that version. An unpinned install at t resolves to that version, which
// later maintenance releases of older major versions do not change.
//
// Releases that do not pin repo are skipped, so a repository that cibuildwheel
// stopped pinning, such as musllinux_1_1, resolves to the highest version that
// still pins it. ok is false if no release pins repo or if the digest of the
// selected pin is unknown.
func CibuildwheelImageAt(repo string, t time.Time) (ref, version string, ok bool) {
	return cibuildwheelImageAt(cibuildwheelReleases, repo, t)
}

func cibuildwheelImageAt(releases []cibuildwheelRelease, repo string, t time.Time) (ref, version string, ok bool) {
	var best *cibuildwheelRelease
	for i, rel := range releases {
		if _, pinned := rel.Images[repo]; !pinned || rel.Published.After(t) {
			continue
		}
		if best == nil || semver.Cmp(rel.Version, best.Version) > 0 {
			best = &releases[i]
		}
	}
	if best == nil {
		return "", "", false
	}
	// NOTE: A lower version is no fallback for an unknown digest because it
	// pins a different image.
	img := best.Images[repo]
	if img.Digest == "" {
		return "", "", false
	}
	return img.reference(repo), best.Version, true
}

// CibuildwheelImage returns the digest-pinned reference to the repo image that
// cibuildwheel version pins. ok is false if the version is unknown, does not
// pin repo or if the digest of the pin is unknown.
func CibuildwheelImage(repo, version string) (ref string, ok bool) {
	return cibuildwheelImage(cibuildwheelReleases, repo, version)
}

func cibuildwheelImage(releases []cibuildwheelRelease, repo, version string) (ref string, ok bool) {
	for _, rel := range releases {
		if rel.Version != version {
			continue
		}
		img, pinned := rel.Images[repo]
		if !pinned || img.Digest == "" {
			return "", false
		}
		return img.reference(repo), true
	}
	return "", false
}

// CibuildwheelVersionTaggedAt returns the cibuildwheel version whose release
// tag points to commit, a full commit hash. ok is false if no release in the
// table is tagged at commit.
func CibuildwheelVersionTaggedAt(commit string) (version string, ok bool) {
	return cibuildwheelVersionTaggedAt(cibuildwheelReleases, commit)
}

func cibuildwheelVersionTaggedAt(releases []cibuildwheelRelease, commit string) (version string, ok bool) {
	for _, rel := range releases {
		if rel.Commit == commit {
			return rel.Version, true
		}
	}
	return "", false
}

// manylinux2014MinCibuildwheelVersion is the first cibuildwheel release whose
// manylinux2014_x86_64 image (2024.08.03-1) includes the final CentOS 7.9.2009
// release package, avoiding a centos-release upgrade during yum update that
// restores defunct mirrorlist.centos.org repo definitions.
const manylinux2014MinCibuildwheelVersion = "2.20.0"

// CibuildwheelSupportsPython reports whether a cibuildwheel release pins
// container images that ship the given CPython tag. cibuildwheel 3.0.0 dropped
// cp36 and cp37, and cibuildwheel 4.0.0 dropped cp38.
func CibuildwheelSupportsPython(version, pythonTag string) bool {
	switch pythonTag {
	case "cp36", "cp37":
		return semver.Cmp(version, "3.0.0") < 0
	case "cp38":
		return semver.Cmp(version, "4.0.0") < 0
	default:
		return true
	}
}

// CibuildwheelImageForPython returns the digest-pinned reference to the repo
// image that cibuildwheel version pins if that version supports pythonTag. On
// manylinux2014_x86_64, versions prior to 2.20.0 are clamped to 2.20.0 so that
// yum update does not overwrite custom repository definitions.
func CibuildwheelImageForPython(repo, version, pythonTag string) (ref string, ok bool) {
	return cibuildwheelImageForPython(cibuildwheelReleases, repo, version, pythonTag)
}

func cibuildwheelImageForPython(releases []cibuildwheelRelease, repo, version, pythonTag string) (ref string, ok bool) {
	if !CibuildwheelSupportsPython(version, pythonTag) {
		return "", false
	}
	if repo == ImageManylinux2014X86_64 && semver.Cmp(version, manylinux2014MinCibuildwheelVersion) < 0 {
		version = manylinux2014MinCibuildwheelVersion
	}
	return cibuildwheelImage(releases, repo, version)
}

// CibuildwheelImageAtForPython returns the digest-pinned reference to the repo
// image that the highest cibuildwheel version compatible with pythonTag and
// published at or before t pins, along with that version.
func CibuildwheelImageAtForPython(repo, pythonTag string, t time.Time) (ref, version string, ok bool) {
	return cibuildwheelImageAtForPython(cibuildwheelReleases, repo, pythonTag, t)
}

func cibuildwheelImageAtForPython(releases []cibuildwheelRelease, repo, pythonTag string, t time.Time) (ref, version string, ok bool) {
	var best *cibuildwheelRelease
	for i, rel := range releases {
		if !CibuildwheelSupportsPython(rel.Version, pythonTag) {
			continue
		}
		if _, pinned := rel.Images[repo]; !pinned || rel.Published.After(t) {
			continue
		}
		if best == nil || semver.Cmp(rel.Version, best.Version) > 0 {
			best = &releases[i]
		}
	}
	if best == nil {
		return "", "", false
	}
	if repo == ImageManylinux2014X86_64 && semver.Cmp(best.Version, manylinux2014MinCibuildwheelVersion) < 0 {
		ref, ok := cibuildwheelImage(releases, repo, manylinux2014MinCibuildwheelVersion)
		if !ok {
			return "", "", false
		}
		return ref, manylinux2014MinCibuildwheelVersion, true
	}
	img := best.Images[repo]
	if img.Digest == "" {
		return "", "", false
	}
	return img.reference(repo), best.Version, true
}

// reference returns the reference to the image in repo. The digest takes
// precedence over the tag, which is kept for readability.
func (p pinnedImage) reference(repo string) string {
	if p.Tag == "" {
		return repo + "@" + p.Digest
	}
	return repo + ":" + p.Tag + "@" + p.Digest
}
