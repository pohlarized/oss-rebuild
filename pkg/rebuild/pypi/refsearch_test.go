// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/go-cmp/cmp"
	"github.com/google/oss-rebuild/internal/gitx/gitxtest"
	"github.com/google/oss-rebuild/internal/httpx/httpxtest"
	"github.com/google/oss-rebuild/pkg/archive"
	"github.com/google/oss-rebuild/pkg/archive/archivetest"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	pypireg "github.com/google/oss-rebuild/pkg/registry/pypi"
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
		name    string
		commits []gitxtest.Commit
		version string
		want    string // commit ID, or "" for a rejection
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
				{ID: "v1", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("acme", "0.0.0"), "acme/core.py": coreV1, "acme/util.py": util}},
				{ID: "v2", Time: day(2), Parent: "v1", Files: gitxtest.FileContent{"acme/core.py": coreV2}},
			},
			version: "2.0.0",
			want:    "",
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
		{
			name: "attr directive tie, earliest neutral",
			commits: []gitxtest.Commit{
				{ID: "a1", Time: day(1), Files: gitxtest.FileContent{"setup.cfg": "[metadata]\nname = acme\nversion = attr: acme.__version__\n", "acme/core.py": coreV2, "acme/util.py": util}},
				{ID: "a2", Time: day(2), Parent: "a1", Files: gitxtest.FileContent{"README.md": "doc change\n"}},
			},
			version: "2.0.0",
			want:    "a1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepo(tc.commits, nil))
			ref, err := matchArchiveBlobs(ctx, hashes, "acme", tc.version, repo.Repository)
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
	if _, err := matchArchiveBlobs(ctx, []plumbing.Hash{plumbing.ZeroHash}, "acme", "2.0.0", repo.Repository); err == nil {
		t.Errorf("matchArchiveBlobs(no matching blobs) = nil error, want a rejection")
	}
}

func TestSourceArchives(t *testing.T) {
	tests := []struct {
		name      string
		artifacts []pypireg.Artifact
		want      []string
	}{
		{
			name: "PureWheelThenSdist",
			artifacts: []pypireg.Artifact{
				{Filename: "foo-1.0.0.tar.gz"},
				{Filename: "foo-1.0.0-py3-none-any.whl"},
				{Filename: "foo-1.0.0-cp312-cp312-manylinux_2_28_x86_64.whl"},
			},
			want: []string{"foo-1.0.0-py3-none-any.whl", "foo-1.0.0.tar.gz"},
		},
		{
			name: "PlatformWheelsAndSdist",
			artifacts: []pypireg.Artifact{
				{Filename: "lru_dict-1.4.1-cp312-cp312-manylinux_2_28_x86_64.whl"},
				{Filename: "lru_dict-1.4.1.tar.gz"},
			},
			want: []string{"lru_dict-1.4.1.tar.gz"},
		},
		{
			name: "ZipSdist",
			artifacts: []pypireg.Artifact{
				{Filename: "foo-1.0.0.zip"},
			},
			want: []string{"foo-1.0.0.zip"},
		},
		{
			name: "UnsupportedSdistSkipped",
			artifacts: []pypireg.Artifact{
				{Filename: "foo-1.0.0.tar.bz2"},
			},
			want: nil,
		},
		{
			name:      "NoCandidates",
			artifacts: nil,
			want:      nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, a := range sourceArchives(tt.artifacts) {
				got = append(got, a.Filename)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("sourceArchives() diff (-want +got):\n%s", diff)
			}
		})
	}
}

func tarEntry(name, body string) archive.TarEntry {
	return archive.TarEntry{
		Header: &tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o644},
		Body:   []byte(body),
	}
}

func TestArchiveContentRef(t *testing.T) {
	const (
		readmeV1 = "lru-dict original readme contents\n"
		readmeV2 = "lru-dict updated readme contents calling for maintainers\n"
		cSrc     = "int lru_init(void) { return 0; }\n"
	)
	day := func(n int) time.Time { return time.Date(2025, time.October, n, 0, 0, 0, 0, time.UTC) }
	repo := must(gitxtest.CreateRepo([]gitxtest.Commit{
		{ID: "bump", Time: day(1), Files: gitxtest.FileContent{"pyproject.toml": pyprojectTOML("lru-dict", "1.4.1"), "README.rst": readmeV1, "src/lru.c": cSrc}},
		{ID: "readme", Time: day(2), Parent: "bump", Files: gitxtest.FileContent{"README.rst": readmeV2}},
	}, nil))
	tgzBuf := must(archivetest.TgzFile([]archive.TarEntry{
		tarEntry("lru_dict-1.4.1/pyproject.toml", pyprojectTOML("lru-dict", "1.4.1")),
		tarEntry("lru_dict-1.4.1/README.rst", readmeV2),
		tarEntry("lru_dict-1.4.1/src/lru.c", cSrc),
		tarEntry("lru_dict-1.4.1/PKG-INFO", "Metadata-Version: 2.1\nName: lru-dict\nVersion: 1.4.1\n"),
	}))
	sdistURL := "https://files.pythonhosted.org/lru_dict-1.4.1.tar.gz"
	release := &pypireg.Release{
		Info: pypireg.Info{Name: "lru-dict", Version: "1.4.1"},
		Artifacts: []pypireg.Artifact{
			{Filename: "lru_dict-1.4.1-cp312-cp312-musllinux_1_2_x86_64.whl", Size: 100},
			{Filename: "lru_dict-1.4.1.tar.gz", URL: sdistURL, Size: int64(tgzBuf.Len())},
		},
	}
	releaseJSON := fmt.Sprintf(`{"info":{"name":"lru-dict","version":"1.4.1"},"urls":[{"filename":"lru_dict-1.4.1.tar.gz","url":%q,"size":%d}]}`, sdistURL, tgzBuf.Len())
	mux := rebuild.RegistryMux{
		PyPI: pypireg.HTTPRegistry{
			Client: &httpxtest.MockClient{
				Calls: []httpxtest.Call{
					{URL: "https://pypi.org/pypi/lru-dict/1.4.1/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(releaseJSON)}},
					{URL: sdistURL, Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(tgzBuf.String())}},
				},
				URLValidator: httpxtest.NewURLValidator(t),
			},
		},
	}
	got, err := archiveContentRef(context.Background(), mux, "lru-dict", "1.4.1", release, repo.Repository)
	if err != nil {
		t.Fatalf("archiveContentRef() error = %v", err)
	}
	want := repo.Commits["readme"].String()
	if got != want {
		t.Errorf("archiveContentRef() = %q, want %q", got, want)
	}
}
