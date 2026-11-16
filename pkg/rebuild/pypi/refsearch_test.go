// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	"github.com/google/oss-rebuild/internal/gitx/gitxtest"
	"github.com/google/oss-rebuild/pkg/archive"
	"github.com/google/oss-rebuild/pkg/archive/archivetest"
	"github.com/google/oss-rebuild/pkg/vcs/gitscan"
)

func must[T any](t T, err error) T {
	if err != nil {
		panic(err)
	}
	return t
}

func wheelZip(t *testing.T, entries []archive.ZipEntry) []byte {
	t.Helper()
	buf, err := archivetest.ZipFile(entries)
	if err != nil {
		t.Fatalf("building wheel zip: %v", err)
	}
	return buf.Bytes()
}

func zipEntry(name, body string) archive.ZipEntry {
	return archive.ZipEntry{FileHeader: &zip.FileHeader{Name: name}, Body: []byte(body)}
}

func wheelHashes(t *testing.T, entries []archive.ZipEntry) []plumbing.Hash {
	t.Helper()
	whl := wheelZip(t, entries)
	zr := must(zip.NewReader(bytes.NewReader(whl), int64(len(whl))))
	hashes, err := gitscan.BlobHashesFromZip(zr)
	if err != nil {
		t.Fatalf("hashing wheel: %v", err)
	}
	return hashes
}

func pyprojectTOML(name, version string) string {
	return fmt.Sprintf("[project]\nname = \"%s\"\nversion = \"%s\"\n", name, version)
}

func TestMatchArchiveBlobs(t *testing.T) {
	const (
		coreV1 = "def core():\n    return 'core version one implementation body original'\n"
		coreV2 = "def core():\n    return 'core version two implementation body rewritten'\n"
		util   = "def util():\n    return 'util helper lorem ipsum dolor sit amet here'\n"
	)
	day := func(n int) time.Time { return time.Date(2024, time.January, n, 0, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	// The wheel carries v2's sources plus generated dist-info metadata; the
	// metadata blobs are absent from the repo, so the scan drops them.
	hashes := wheelHashes(t, []archive.ZipEntry{
		zipEntry("acme/core.py", coreV2),
		zipEntry("acme/util.py", util),
		zipEntry("acme-2.0.0.dist-info/METADATA", "Metadata-Version: 2.1\nName: acme\nVersion: 2.0.0"),
	})
	for _, tc := range []struct {
		name     string
		commits  []gitxtest.Commit
		version  string
		uploaded time.Time
		want     string // commit ID, or "" for a rejection
	}{
		{
			name: "unique best overlap",
			commits: []gitxtest.Commit{
				{ID: "v1", Time: day(1), Files: gitxtest.FileContent{"acme/core.py": coreV1, "acme/util.py": util}},
				{ID: "v2", Time: day(2), Parent: "v1", Files: gitxtest.FileContent{"acme/core.py": coreV2}},
			},
			version: "2.0.0",
			want:    "v2",
		},
		{
			// A build file naming another version drops its commit, even the
			// only one carrying the content.
			name: "unique best overlap declaring another version",
			commits: []gitxtest.Commit{
				{ID: "v1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "1.0.0"), "acme/core.py": coreV1, "acme/util.py": util}},
				{ID: "v2", Time: day(2), Parent: "v1", Files: gitxtest.FileContent{"acme/core.py": coreV2}},
			},
			version: "2.0.0",
			want:    "",
		},
		{
			// Placeholder versions (0.0.0, 0.0.1, 0.1.0) left for CI stamping
			// count as neutral rather than contradicting the target version.
			name: "unique best overlap declaring placeholder version",
			commits: []gitxtest.Commit{
				{ID: "v1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "0.1.0"), "acme/core.py": coreV1, "acme/util.py": util}},
				{ID: "v2", Time: day(2), Parent: "v1", Files: gitxtest.FileContent{"acme/core.py": coreV2}},
			},
			version: "2.0.0",
			want:    "v2",
		},
		{
			// A real 0.1.0 release still treats a declared 0.1.0 as confirming,
			// picking the latest confirming commit over an earlier one.
			name: "version tie on 0.1.0 target, latest confirming",
			commits: []gitxtest.Commit{
				{ID: "r1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "0.0.9"), "acme/core.py": coreV2, "acme/util.py": util}},
				{ID: "r2", Time: day(2), Parent: "r1", Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "0.1.0")}},
				{ID: "r3", Time: day(3), Parent: "r2", Files: gitxtest.FileContent{"README.md": "doc change\n"}},
			},
			version: "0.1.0",
			want:    "r3",
		},
		{
			// Wheels carry no build files, so commits differing only in the
			// declared version tie on blobs; the latest confirming commit wins.
			name: "version tie, latest confirming",
			commits: []gitxtest.Commit{
				{ID: "r1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "1.0.0"), "acme/core.py": coreV2, "acme/util.py": util}},
				{ID: "r2", Time: day(2), Parent: "r1", Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "2.0.0")}},
				{ID: "r3", Time: day(3), Parent: "r2", Files: gitxtest.FileContent{"README.md": "doc change\n"}},
			},
			version: "2.0.0",
			want:    "r3",
		},
		{
			// A commit made after the upload cannot have produced the wheel.
			name: "version tie, latest confirming before upload",
			commits: []gitxtest.Commit{
				{ID: "r1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "1.0.0"), "acme/core.py": coreV2, "acme/util.py": util}},
				{ID: "r2", Time: day(2), Parent: "r1", Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "2.0.0")}},
				{ID: "r3", Time: day(5), Parent: "r2", Files: gitxtest.FileContent{"README.md": "doc change\n"}},
			},
			version:  "2.0.0",
			uploaded: day(3),
			want:     "r2",
		},
		{
			name: "version tie, earlier confirming",
			commits: []gitxtest.Commit{
				{ID: "r1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "1.0.0"), "acme/core.py": coreV2, "acme/util.py": util}},
				{ID: "r2", Time: day(2), Parent: "r1", Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "2.0.0")}},
			},
			version: "1.0.0",
			want:    "r1",
		},
		{
			// A version no tied commit declares yields no match rather than a
			// wrong one.
			name: "version tie, none confirming",
			commits: []gitxtest.Commit{
				{ID: "r1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "1.0.0"), "acme/core.py": coreV2, "acme/util.py": util}},
				{ID: "r2", Time: day(2), Parent: "r1", Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "2.0.0")}},
			},
			version: "3.0.0",
			want:    "",
		},
		{
			name: "version tie, neutral over contradicting",
			commits: []gitxtest.Commit{
				{ID: "r1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "1.0.0"), "acme/core.py": coreV2, "acme/util.py": util}},
				{ID: "r2", Time: day(2), Parent: "r1", Files: gitxtest.FileContent{"pyproject.toml": "[project]\nname = \"acme\"\ndynamic = [\"version\"]\n"}},
			},
			version: "3.0.0",
			want:    "r2",
		},
		{
			// PyPI publishes 2.0.0-beta.1 as 2.0.0b1, and the build file's
			// spelling still confirms.
			name: "version tie, confirming under PEP 440 spelling",
			commits: []gitxtest.Commit{
				{ID: "r1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "1.0.0"), "acme/core.py": coreV2, "acme/util.py": util}},
				{ID: "r2", Time: day(2), Parent: "r1", Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "2.0.0-beta.1")}},
			},
			version: "2.0.0b1",
			want:    "r2",
		},
		{
			// Ties among version-silent candidates resolve to the earliest,
			// where the artifact's content was introduced.
			name: "silent tie, earliest",
			commits: []gitxtest.Commit{
				{ID: "s1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": "[project]\nname = \"acme\"\ndynamic = [\"version\"]\n", "acme/core.py": coreV2, "acme/util.py": util}},
				{ID: "s2", Time: day(2), Parent: "s1", Files: gitxtest.FileContent{"README.md": "doc change\n"}},
			},
			version: "2.0.0",
			want:    "s1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepo(tc.commits, nil))
			ref, err := matchArchiveBlobs(ctx, hashes, "acme", tc.version, tc.uploaded, repo.Repository)
			if tc.want == "" {
				if err == nil {
					t.Errorf("matchArchiveBlobs = %q, nil error, want a rejection", ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("matchArchiveBlobs: %v", err)
			}
			if want := repo.Commits[tc.want].String(); ref != want {
				t.Errorf("matchArchiveBlobs = %q, want %s %q", ref, tc.want, want)
			}
		})
	}

	// A blob set absent from the repo entirely is rejected by the scan.
	repo := must(gitxtest.CreateRepo([]gitxtest.Commit{{ID: "v1", Files: gitxtest.FileContent{"acme/core.py": coreV1}}}, nil))
	if _, err := matchArchiveBlobs(ctx, []plumbing.Hash{plumbing.ZeroHash}, "acme", "2.0.0", time.Time{}, repo.Repository); err == nil {
		t.Errorf("matchArchiveBlobs(no matching blobs) = nil error, want a rejection")
	}
}

func TestArchiveBlobs(t *testing.T) {
	want := []plumbing.Hash{plumbing.ComputeHash(plumbing.BlobObject, []byte("print"))}
	var sdist bytes.Buffer
	gz := gzip.NewWriter(&sdist)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "acme-1.0/acme.py", Typeflag: tar.TypeReg, Size: 5}); err != nil {
		t.Fatal(err)
	}
	must(tw.Write([]byte("print")))
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	wheel := wheelZip(t, []archive.ZipEntry{
		zipEntry("acme.py", "print"),
		zipEntry("py.typed", ""),
		zipEntry("acme-1.0.dist-info/licenses/LICENSE", "Apache License"),
	})
	for name, body := range map[string][]byte{"acme-1.0.tar.gz": sdist.Bytes(), "acme-1.0-py3-none-any.whl": wheel} {
		got, err := archiveBlobs(name, body)
		if err != nil || !cmp.Equal(got, want) {
			t.Errorf("archiveBlobs(%s) = %v, %v, want %v", name, got, err, want)
		}
	}
}

func TestMatchArchiveBlobsTagTieBreaking(t *testing.T) {
	const (
		modSrc    = "def compute():\n    return 'opencv wrapper module body'\n"
		dynPyproj = "[project]\nname = \"opencv-python\"\ndynamic = [\"version\"]\n"
	)
	hashes := wheelHashes(t, []archive.ZipEntry{
		zipEntry("pkg/mod.py", modSrc),
	})
	t1 := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	beyondMaxVetted := func() []gitxtest.Commit {
		commits := make([]gitxtest.Commit, 0, maxVettedTies+3)
		commits = append(commits, gitxtest.Commit{
			ID:    "tagged",
			Time:  t1,
			Tag:   "84",
			Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc},
		})
		parent := "tagged"
		for i := range maxVettedTies + 2 {
			id := fmt.Sprintf("doc-%d", i)
			commits = append(commits, gitxtest.Commit{
				ID:     id,
				Time:   t1.Add(time.Duration(i+1) * time.Second),
				Parent: parent,
				Files:  gitxtest.FileContent{"README.md": fmt.Sprintf("rev %d\n", i)},
			})
			parent = id
		}
		return commits
	}()
	tests := []struct {
		name         string
		pkg          string
		version      string
		commits      []gitxtest.Commit
		annotatedTag string
		tagCommitID  string
		want         string
	}{
		{
			name:    "PrefersLightweightTaggedCommitOverEarlierTreeIdenticalCommit",
			pkg:     "opencv-python",
			version: "4.10.0.84",
			commits: []gitxtest.Commit{
				{ID: "untagged", Time: t1, Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc}},
				{ID: "tagged", Time: t2, Parent: "untagged", Tag: "84"},
			},
			want: "tagged",
		},
		{
			name:    "PrefersAnnotatedTaggedCommitOverEarlierTreeIdenticalCommit",
			pkg:     "opencv-python",
			version: "5.0.0.93",
			commits: []gitxtest.Commit{
				{ID: "untagged", Time: t1, Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc}},
				{ID: "tagged", Time: t2, Parent: "untagged"},
			},
			annotatedTag: "93",
			tagCommitID:  "tagged",
			want:         "tagged",
		},
		{
			name:    "PrefersTaggedConfirmingCommitOverNewerUntaggedCommit",
			pkg:     "acme",
			version: "1.2.0",
			commits: []gitxtest.Commit{
				{ID: "tagged", Time: t1, Tag: "v1.2.0", Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "1.2.0"), "pkg/mod.py": modSrc}},
				{ID: "post-doc", Time: t2, Parent: "tagged", Files: gitxtest.FileContent{"README.md": "post-tag doc tweak\n"}},
			},
			want: "tagged",
		},
		{
			name:    "PrefersTaggedCommitBeforeMaxVettedTiesCutoff",
			pkg:     "opencv-python",
			version: "4.10.0.84",
			commits: beyondMaxVetted,
			want:    "tagged",
		},
		{
			name:    "PrefersMatchingBareBuildTagOverEarlierBuildTag",
			pkg:     "opencv-python",
			version: "4.10.0.84",
			commits: []gitxtest.Commit{
				{ID: "prev-build", Time: t1, Tag: "83", Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc}},
				{ID: "curr-build", Time: t2, Parent: "prev-build", Tag: "84", Files: gitxtest.FileContent{"README.md": "build 84\n"}},
			},
			want: "curr-build",
		},
		{
			name:    "PrefersFinalTagOverEarlierPreReleaseTag",
			pkg:     "acme",
			version: "1.2.0",
			commits: []gitxtest.Commit{
				{ID: "rc", Time: t1, Tag: "v1.2.0rc1", Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc}},
				{ID: "final", Time: t2, Parent: "rc", Tag: "v1.2.0", Files: gitxtest.FileContent{"README.md": "final release\n"}},
			},
			want: "final",
		},
		{
			name:    "PrefersVersionedTagOverEarlierUnversionedTag",
			pkg:     "opencv-python",
			version: "4.10.0.84",
			commits: []gitxtest.Commit{
				{ID: "stable-tag", Time: t1, Tag: "stable", Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc}},
				{ID: "build-tag", Time: t2, Parent: "stable-tag", Tag: "84", Files: gitxtest.FileContent{"README.md": "build 84\n"}},
			},
			want: "build-tag",
		},
		{
			name:    "IgnoresSameBasePreReleaseTagWhenRebuildingFinal",
			pkg:     "acme",
			version: "1.2.0",
			commits: []gitxtest.Commit{
				{ID: "earliest", Time: t1, Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc}},
				{ID: "rc", Time: t2, Parent: "earliest", Tag: "v1.2.0rc1", Files: gitxtest.FileContent{"README.md": "rc docs\n"}},
			},
			want: "earliest",
		},
		{
			name:    "IgnoresContradictingVersionTagWhenBreakingTies",
			pkg:     "opencv-python",
			version: "2.5.1",
			commits: []gitxtest.Commit{
				{ID: "earliest", Time: t1, Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc}},
				{ID: "other-rc", Time: t2, Parent: "earliest", Tag: "2.5.0rc1", Files: gitxtest.FileContent{"README.md": "rc docs\n"}},
			},
			want: "earliest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepo(tc.commits, nil))
			g2 := repo.Repository
			if tc.annotatedTag != "" {
				_, err := repo.GoGit.CreateTag(tc.annotatedTag, repo.Commits[tc.tagCommitID], &git.CreateTagOptions{
					Tagger:  &object.Signature{Name: "Maintainer", Email: "m@example.com", When: t2},
					Message: "release " + tc.annotatedTag,
				})
				if err != nil {
					t.Fatalf("CreateTag(%q): %v", tc.annotatedTag, err)
				}
				g2 = must(gitxtest.Open(repo.Storer))
			}
			got, err := matchArchiveBlobs(context.Background(), hashes, tc.pkg, tc.version, time.Time{}, g2)
			if err != nil {
				t.Fatalf("matchArchiveBlobs() error = %v", err)
			}
			want := repo.Commits[tc.want].String()
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("matchArchiveBlobs() diff (-want +got):\n%s", diff)
			}
		})
	}
	t.Run("SkipsTagPointingAtBlobObject", func(t *testing.T) {
		repo := must(gitxtest.CreateRepo([]gitxtest.Commit{
			{ID: "c1", Time: t1, Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc}},
			{ID: "c2", Time: t2, Parent: "c1", Tag: "84", Files: gitxtest.FileContent{"README.md": "next\n"}},
		}, nil))
		blobEnc := repo.Storer.NewEncodedObject()
		blobEnc.SetType(plumbing.BlobObject)
		w := must(blobEnc.Writer())
		must(w.Write([]byte("pgp public key block\n")))
		must(0, w.Close())
		blobHash := must(repo.Storer.SetEncodedObject(blobEnc))
		must(0, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("gpg-pubkey"), blobHash)))
		g2 := must(gitxtest.Open(repo.Storer))
		got, err := matchArchiveBlobs(context.Background(), hashes, "opencv-python", "4.10.0.84", time.Time{}, g2)
		if err != nil {
			t.Fatalf("matchArchiveBlobs() error = %v", err)
		}
		if want := repo.Commits["c2"].String(); got != want {
			t.Errorf("matchArchiveBlobs() = %q, want %q", got, want)
		}
	})
	t.Run("PropagatesBrokenTagPeelError", func(t *testing.T) {
		repo := must(gitxtest.CreateRepo([]gitxtest.Commit{
			{ID: "c1", Time: t1, Files: gitxtest.FileContent{"pyproject.toml": dynPyproj, "pkg/mod.py": modSrc}},
			{ID: "c2", Time: t2, Parent: "c1", Files: gitxtest.FileContent{"README.md": "next\n"}},
		}, nil))
		enc := repo.Storer.NewEncodedObject()
		must(0, (&object.Tag{
			Name:       "84",
			Tagger:     object.Signature{Name: "Maintainer", Email: "m@example.com", When: t2},
			Message:    "dangling tag target",
			TargetType: plumbing.CommitObject,
			Target:     plumbing.NewHash("baadf00dbaadf00dbaadf00dbaadf00dbaadf00d"),
		}).Encode(enc))
		badTagHash := must(repo.Storer.SetEncodedObject(enc))
		must(0, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewTagReferenceName("84"), badTagHash)))
		g2 := must(gitxtest.Open(repo.Storer))
		if _, err := matchArchiveBlobs(context.Background(), hashes, "opencv-python", "4.10.0.84", time.Time{}, g2); err == nil {
			t.Fatalf("matchArchiveBlobs() = nil error, want tag peel error")
		}
	})
}
