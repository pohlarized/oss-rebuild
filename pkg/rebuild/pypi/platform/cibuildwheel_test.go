// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/oss-rebuild/internal/semver"
)

func TestCibuildwheelImageAt(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2025, time.January, d, 0, 0, 0, 0, time.UTC) }
	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	releases := []cibuildwheelRelease{
		{
			Version:   "1.0.0",
			Published: day(1),
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "t1", Digest: digest("1")},
				ImageMusllinux1_1X86_64:  {Tag: "m1", Digest: digest("2")},
			},
		},
		{
			Version:   "1.1.0",
			Published: day(3),
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "expired"},
			},
		},
		{
			Version:   "2.0.0",
			Published: day(10),
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "t2", Digest: digest("3")},
				ImageMusllinux1_1X86_64:  {Tag: "m2", Digest: digest("4")},
			},
		},
		{
			Version:   "3.0.0",
			Published: day(20),
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Digest: digest("5")},
				ImageManylinux2_34X86_64: {Tag: "n3", Digest: digest("6")},
			},
		},
		{
			Version:   "2.0.1",
			Published: day(22),
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "t21", Digest: digest("7")},
				ImageMusllinux1_1X86_64:  {Tag: "m21", Digest: digest("8")},
			},
		},
	}
	type result struct {
		Ref, Version string
		OK           bool
	}
	tests := []struct {
		name string
		repo string
		t    time.Time
		want result
	}{
		{
			name: "BeforeFirstRelease",
			repo: ImageManylinux2014X86_64,
			t:    day(0),
			want: result{},
		},
		{
			name: "ExpiredTagHasNoFallback",
			repo: ImageManylinux2014X86_64,
			t:    day(5),
			want: result{},
		},
		{
			name: "AtPublishTime",
			repo: ImageManylinux2014X86_64,
			t:    day(10),
			want: result{Ref: ImageManylinux2014X86_64 + ":t2@" + digest("3"), Version: "2.0.0", OK: true},
		},
		{
			name: "BetweenReleases",
			repo: ImageManylinux2014X86_64,
			t:    day(15),
			want: result{Ref: ImageManylinux2014X86_64 + ":t2@" + digest("3"), Version: "2.0.0", OK: true},
		},
		{
			name: "DigestOnlyPin",
			repo: ImageManylinux2014X86_64,
			t:    day(21),
			want: result{Ref: ImageManylinux2014X86_64 + "@" + digest("5"), Version: "3.0.0", OK: true},
		},
		{
			name: "MaintenanceReleaseOfOlderMajor",
			repo: ImageManylinux2014X86_64,
			t:    day(25),
			want: result{Ref: ImageManylinux2014X86_64 + "@" + digest("5"), Version: "3.0.0", OK: true},
		},
		{
			name: "DroppedRepoUsesHighestPinningVersion",
			repo: ImageMusllinux1_1X86_64,
			t:    day(25),
			want: result{Ref: ImageMusllinux1_1X86_64 + ":m21@" + digest("8"), Version: "2.0.1", OK: true},
		},
		{
			name: "RepoNotPinnedYet",
			repo: ImageManylinux2_34X86_64,
			t:    day(15),
			want: result{},
		},
		{
			name: "RepoNeverPinned",
			repo: ImageMusllinux1_2X86_64,
			t:    day(25),
			want: result{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got result
			got.Ref, got.Version, got.OK = cibuildwheelImageAt(releases, tc.repo, tc.t)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("cibuildwheelImageAt(%q, %v) returned diff (-want +got):\n%s", tc.repo, tc.t, diff)
			}
		})
	}
}

func TestCibuildwheelImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	releases := []cibuildwheelRelease{
		{
			Version: "1.0.0",
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "expired"},
			},
		},
		{
			Version: "2.0.0",
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "t2", Digest: digest},
			},
		},
	}
	type result struct {
		Ref string
		OK  bool
	}
	tests := []struct {
		name    string
		repo    string
		version string
		want    result
	}{
		{
			name:    "KnownVersion",
			repo:    ImageManylinux2014X86_64,
			version: "2.0.0",
			want:    result{Ref: ImageManylinux2014X86_64 + ":t2@" + digest, OK: true},
		},
		{
			name:    "UnknownVersion",
			repo:    ImageManylinux2014X86_64,
			version: "3.0.0",
			want:    result{},
		},
		{
			name:    "RepoNotPinned",
			repo:    ImageMusllinux1_2X86_64,
			version: "2.0.0",
			want:    result{},
		},
		{
			name:    "ExpiredPin",
			repo:    ImageManylinux2014X86_64,
			version: "1.0.0",
			want:    result{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got result
			got.Ref, got.OK = cibuildwheelImage(releases, tc.repo, tc.version)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("cibuildwheelImage(%q, %q) returned diff (-want +got):\n%s", tc.repo, tc.version, diff)
			}
		})
	}
}

func TestCibuildwheelVersionTaggedAt(t *testing.T) {
	commit := func(c string) string { return strings.Repeat(c, 40) }
	releases := []cibuildwheelRelease{
		{Version: "1.0.0", Commit: commit("1")},
		{Version: "2.0.0", Commit: commit("2")},
	}
	type result struct {
		Version string
		OK      bool
	}
	tests := []struct {
		name   string
		commit string
		want   result
	}{
		{
			name:   "TaggedCommit",
			commit: commit("2"),
			want:   result{Version: "2.0.0", OK: true},
		},
		{
			name:   "UntaggedCommit",
			commit: commit("3"),
			want:   result{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got result
			got.Version, got.OK = cibuildwheelVersionTaggedAt(releases, tc.commit)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("cibuildwheelVersionTaggedAt(%q) returned diff (-want +got):\n%s", tc.commit, diff)
			}
		})
	}
}

// TestCibuildwheelReleases guards the lookups against generated versions that
// internal/semver cannot order and against ambiguous commits.
func TestCibuildwheelReleases(t *testing.T) {
	commitPat := regexp.MustCompile(`^[0-9a-f]{40}$`)
	versions := make(map[string]bool)
	commits := make(map[string]bool)
	for _, rel := range cibuildwheelReleases {
		if _, err := semver.New(rel.Version); err != nil {
			t.Errorf("version %q is not a semantic version", rel.Version)
		}
		if versions[rel.Version] {
			t.Errorf("version %q is listed more than once", rel.Version)
		}
		versions[rel.Version] = true
		if !commitPat.MatchString(rel.Commit) {
			t.Errorf("commit %q of %s is not a full commit hash", rel.Commit, rel.Version)
		}
		if commits[rel.Commit] {
			t.Errorf("commit %q of %s is listed more than once", rel.Commit, rel.Version)
		}
		commits[rel.Commit] = true
	}
}

func TestCibuildwheelImageForPython(t *testing.T) {
	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	releases := []cibuildwheelRelease{
		{
			Version: "2.10.2",
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "2022-09-18", Digest: digest("1")},
				ImageMusllinux1_2X86_64:  {Tag: "2022-09-18", Digest: digest("2")},
			},
		},
		{
			Version: "2.20.0",
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "2024.08.03-1", Digest: digest("3")},
				ImageMusllinux1_2X86_64:  {Tag: "2024.08.03-1", Digest: digest("4")},
			},
		},
		{
			Version: "3.2.1",
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "2025.10.10-1", Digest: digest("5")},
			},
		},
		{
			Version: "4.0.0",
			Images: map[string]pinnedImage{
				ImageManylinux2014X86_64: {Tag: "2026.06.04-1", Digest: digest("6")},
			},
		},
	}
	tests := []struct {
		name      string
		repo      string
		version   string
		pythonTag string
		wantRef   string
		wantOK    bool
	}{
		{
			name:      "ClampsOldManylinux2014ToFloor",
			repo:      ImageManylinux2014X86_64,
			version:   "2.10.2",
			pythonTag: "cp37",
			wantRef:   ImageManylinux2014X86_64 + ":2024.08.03-1@" + digest("3"),
			wantOK:    true,
		},
		{
			name:      "DoesNotClampMusllinux",
			repo:      ImageMusllinux1_2X86_64,
			version:   "2.10.2",
			pythonTag: "cp37",
			wantRef:   ImageMusllinux1_2X86_64 + ":2022-09-18@" + digest("2"),
			wantOK:    true,
		},
		{
			name:      "RejectsCibuildwheel3ForCP36",
			repo:      ImageManylinux2014X86_64,
			version:   "3.2.1",
			pythonTag: "cp36",
			wantOK:    false,
		},
		{
			name:      "AcceptsCibuildwheel3ForCP38",
			repo:      ImageManylinux2014X86_64,
			version:   "3.2.1",
			pythonTag: "cp38",
			wantRef:   ImageManylinux2014X86_64 + ":2025.10.10-1@" + digest("5"),
			wantOK:    true,
		},
		{
			name:      "RejectsCibuildwheel4ForCP38",
			repo:      ImageManylinux2014X86_64,
			version:   "4.0.0",
			pythonTag: "cp38",
			wantOK:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotRef, gotOK := cibuildwheelImageForPython(releases, tc.repo, tc.version, tc.pythonTag)
			if gotOK != tc.wantOK || gotRef != tc.wantRef {
				t.Errorf("cibuildwheelImageForPython(%q, %q, %q) = (%q, %v), want (%q, %v)", tc.repo, tc.version, tc.pythonTag, gotRef, gotOK, tc.wantRef, tc.wantOK)
			}
		})
	}
}
