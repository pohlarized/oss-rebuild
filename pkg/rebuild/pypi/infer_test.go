// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/oss-rebuild/internal/gitx"
	"github.com/google/oss-rebuild/internal/gitx/gitxtest"
	"github.com/google/oss-rebuild/internal/httpx/httpxtest"
	"github.com/google/oss-rebuild/pkg/archive"
	"github.com/google/oss-rebuild/pkg/archive/archivetest"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	pypireg "github.com/google/oss-rebuild/pkg/registry/pypi"
)

func TestInferPythonVersion(t *testing.T) {
	preFix := time.Date(2022, time.January, 1, 0, 0, 0, 0, time.UTC)
	postFix := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		reqs         []string
		registryTime time.Time
		want         string
	}{
		{"pre-fix with no requirements", nil, preFix, "3.11"},
		{"pre-fix unbounded setuptools", []string{"setuptools>=61", "wheel"}, preFix, "3.11"},
		{"pre-fix ceiling above fix", []string{"wheel==0.40.0", "setuptools<=67.7.2"}, preFix, "3.11"},
		{"pre-fix flit", []string{"flit_core==3.9.0", "flit==3.9.0"}, preFix, "3.11"},
		// rsa 4.9: poetry-core 1.0.x stamps "Generator: poetry", so the poetry tool
		// is installed and brings a contemporary setuptools with it.
		{"pre-fix poetry tool", []string{"poetry==1.0.7", "poetry-core>=1.0.0"}, preFix, "3.11"},
		{"post-fix with no requirements", nil, postFix, ""},
		{"post-fix unbounded setuptools", []string{"setuptools>=61", "wheel"}, postFix, ""},
		// requests 2.31.0: a post-fix upload still emitting Platform: UNKNOWN caps
		// setuptools at 57.5.0, which cannot import on 3.12.
		{"post-fix ceiling below fix", []string{"wheel==0.40.0", "setuptools<=57.5.0"}, postFix, "3.11"},
		{"post-fix ceiling above fix", []string{"wheel==0.40.0", "setuptools<=67.7.2"}, postFix, ""},
		{"exclusive bound at fix", []string{"setuptools<66.1.0"}, postFix, "3.11"},
		{"inclusive bound at fix", []string{"setuptools<=66.1.0"}, postFix, ""},
		{"exact pin below fix", []string{"setuptools==58.1.0"}, postFix, "3.11"},
		{"exact pin past fix", []string{"setuptools==70.0.0"}, postFix, ""},
		{"compound constraint", []string{"setuptools>=40.0,<60.0"}, postFix, "3.11"},
		{"case, extras and marker", []string{"SetupTools[core]<60; python_version != '3.3'"}, postFix, "3.11"},
		{"setuptools-scm is not setuptools", []string{"setuptools-scm<6.0"}, postFix, ""},
		{"post-fix flit", []string{"flit_core>=3.2,<4"}, postFix, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inferPythonVersion(tt.reqs, tt.registryTime); got != tt.want {
				t.Errorf("inferPythonVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasFloorAtLeast(t *testing.T) {
	tests := []struct {
		name  string
		reqs  []string
		pkg   string
		limit string
		want  bool
	}{
		{"ExactPinAboveLimit", []string{"setuptools==80.9.0"}, "setuptools", "76.0.0", true},
		{"ExactPinAtLimit", []string{"setuptools==76.0.0"}, "setuptools", "76.0.0", true},
		{"ExactPinBelowLimit", []string{"setuptools==75.3.3"}, "setuptools", "76.0.0", false},
		{"GreaterEqualAboveLimit", []string{"setuptools>=77.0.0"}, "setuptools", "76.0.0", true},
		{"StrictGreaterAtLimit", []string{"setuptools>76.0.0"}, "setuptools", "76.0.0", false},
		{"StrictGreaterAboveLimit", []string{"setuptools>76.1.0"}, "setuptools", "76.0.0", true},
		{"CeilingOnlyDoesNotSetFloor", []string{"setuptools<=67.7.2"}, "setuptools", "59.7.0", false},
		{"UnboundedDoesNotSetFloor", []string{"setuptools"}, "setuptools", "59.7.0", false},
		{"MarkerVersionIgnored", []string{"setuptools==50.0.0; python_version >= '80.0'"}, "setuptools", "76.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasFloorAtLeast(tt.reqs, tt.pkg, tt.limit); got != tt.want {
				t.Errorf("hasFloorAtLeast(%v, %q, %q) = %v, want %v", tt.reqs, tt.pkg, tt.limit, got, tt.want)
			}
		})
	}
}

func TestInferRequirements(t *testing.T) {
	bdist := "Wheel-Version: 1.0\nGenerator: bdist_wheel (0.40.0)\nRoot-Is-Purelib: true\nTag: py3-none-any\n"
	modern := "Metadata-Version: 2.1\nName: x\nLicense-File: LICENSE\n"
	tests := []struct {
		name     string
		wheel    string
		metadata string
		want     []string
	}{
		{"bdist_wheel without License-File", bdist, "Metadata-Version: 2.1\nName: x\n", []string{"wheel==0.40.0", "setuptools<=56.2.0"}},
		{"bdist_wheel with Platform UNKNOWN", bdist, modern + "Platform: UNKNOWN\n", []string{"wheel==0.40.0", "setuptools<=57.5.0"}},
		{"bdist_wheel with modern metadata", bdist, modern, []string{"wheel==0.40.0", "setuptools<=67.7.2"}},
		{"setuptools generator pins exactly", "Wheel-Version: 1.0\nGenerator: setuptools (70.1.0)\n", modern, []string{"setuptools==70.1.0"}},
		{"SetuptoolsGeneratorBelow70_1_0UsesFloor", "Wheel-Version: 1.0\nGenerator: setuptools (65.5.1)\n", modern, []string{"setuptools>=70.1.0"}},
		{"SetuptoolsGeneratorCvxpyBogusVersionUsesFloor", "Wheel-Version: 1.0\nGenerator: setuptools (1.9.2)\n", modern, []string{"setuptools>=70.1.0"}},
		{"flit generator takes no setuptools", "Wheel-Version: 1.0\nGenerator: flit 3.9.0\n", "Metadata-Version: 2.1\nName: x\n", []string{"flit_core==3.9.0", "flit==3.9.0"}},
		{"pdm-backend generator pins pdm-backend", "Wheel-Version: 1.0\nGenerator: pdm-backend (2.4.4)\n", modern, []string{"pdm-backend==2.4.4"}},
		{"uv generator pins uv-build", "Wheel-Version: 1.0\nGenerator: uv 0.10.0\n", modern, []string{"uv-build==0.10.0"}},
		{"maturin generator pins maturin", "Wheel-Version: 1.0\nGenerator: maturin (1.15.0)\n", modern, []string{"maturin==1.15.0"}},
		{"MesonGeneratorReturnsEmpty", "Wheel-Version: 1.0\nGenerator: meson\n", modern, []string{}},
		{"ScikitBuildCoreGeneratorPinsExact", "Wheel-Version: 1.0\nGenerator: scikit-build-core 0.12.2\n", modern, []string{"scikit-build-core==0.12.2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zr := wheelZipReader(t, "testpkg", "1.0", tt.wheel, tt.metadata)
			got, err := inferRequirements("testpkg", "1.0", zr)
			if err != nil {
				t.Fatalf("inferRequirements() error = %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("inferRequirements() diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBackendRequirements(t *testing.T) {
	tests := []struct {
		name string
		reqs []string
		want []string
	}{
		{"MesonPython", []string{"meson-python>=0.17.1"}, []string{"ninja"}},
		{"ScikitBuildCore", []string{"scikit-build-core==0.12.2"}, []string{"ninja"}},
		{"SetuptoolsOnly", []string{"setuptools>=70.1.0"}, nil},
		{"Empty", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := backendRequirements(tc.reqs)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("backendRequirements(%v) diff (-want +got):\n%s", tc.reqs, diff)
			}
		})
	}
}

func TestRequirementName(t *testing.T) {
	tests := []struct {
		req  string
		want string
	}{
		{"setuptools", "setuptools"},
		{"setuptools<=67.7.2", "setuptools"},
		{"setuptools; python_version != '3.3'", "setuptools"},
		{"setuptools[core]>=61", "setuptools"},
		{"flit_core==3.7.1", "flit-core"},
		{"Flit.Core==3.7.1", "flit-core"},
		{"setuptools-scm<6.0", "setuptools-scm"},
		{"numpy (>=1.13)", "numpy"},
		{"numpy(>=1.13)", "numpy"},
		{"pip @ https://example.com/pip.zip", "pip"},
		{"pip@https://example.com/pip.zip", "pip"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := requirementName(tt.req); got != tt.want {
			t.Errorf("requirementName(%q) = %q, want %q", tt.req, got, tt.want)
		}
	}
}

func TestRequirementMarker(t *testing.T) {
	tests := []struct {
		req  string
		want string
	}{
		{"numpy", ""},
		{"numpy==1.14.5", ""},
		{"numpy==1.14.5; python_version>='3.7'", "python_version>='3.7'"},
		{"wheel ; sys_platform == 'win32'", "sys_platform == 'win32'"},
		{"cffi~=1.17; platform_python_implementation != 'PyPy' and python_version < '3.14'", "platform_python_implementation != 'PyPy' and python_version < '3.14'"},
		{"foo; platform_version == '@'", "platform_version == '@'"},
		{"pip @ https://example.com/pip.zip;sha1=abc", ""},
		{"pip @ https://example.com/pip.zip;sha1=abc ; python_version >= '3.8'", "python_version >= '3.8'"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := requirementMarker(tt.req); got != tt.want {
			t.Errorf("requirementMarker(%q) = %q, want %q", tt.req, got, tt.want)
		}
	}
}

func TestGetDistInfoDirAcceptsEquivalentNames(t *testing.T) {
	tests := []struct {
		name    string
		pkg     string
		version string
		files   []string
		want    string
	}{
		{
			name:    "exact normalized path",
			pkg:     "friendly-bard",
			version: "1.2.3",
			files: []string{
				"friendly_bard-1.2.3.dist-info/WHEEL",
				"friendly_bard-1.2.3.dist-info/METADATA",
			},
			want: "friendly_bard-1.2.3.dist-info",
		},
		{
			name:    "lowercased historical path",
			pkg:     "128Autograder",
			version: "5.2.3",
			files: []string{
				"128autograder-5.2.3.dist-info/WHEEL",
				"128autograder-5.2.3.dist-info/METADATA",
			},
			want: "128autograder-5.2.3.dist-info",
		},
		{
			name:    "dot and hyphen equivalence",
			pkg:     "Friendly.Bard",
			version: "2.0.0",
			files: []string{
				"friendly_bard-2.0.0.dist-info/WHEEL",
				"friendly_bard-2.0.0.dist-info/METADATA",
			},
			want: "friendly_bard-2.0.0.dist-info",
		},
		{
			name:    "historical uppercase hyphenated path",
			pkg:     "friendly-bard",
			version: "3.1.4",
			files: []string{
				"Friendly-Bard-3.1.4.dist-info/WHEEL",
				"Friendly-Bard-3.1.4.dist-info/METADATA",
			},
			want: "Friendly-Bard-3.1.4.dist-info",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zr := testZipReader(t, tt.files)
			got, err := getDistInfoDir(tt.pkg, tt.version, zr)
			if err != nil {
				t.Fatalf("getDistInfoDir() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("getDistInfoDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func testZipReader(t *testing.T, files []string) *zip.Reader {
	t.Helper()

	entries := make([]archive.ZipEntry, 0, len(files))
	for _, name := range files {
		entries = append(entries, archive.ZipEntry{
			FileHeader: &zip.FileHeader{Name: name},
			Body:       []byte("data"),
		})
	}
	buf, err := archivetest.ZipFile(entries)
	if err != nil {
		t.Fatalf("ZipFile(): %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("NewReader(): %v", err)
	}
	return zr
}

func TestMergeRequirements(t *testing.T) {
	tests := []struct {
		name      string
		reqs      []string
		buildReqs []string
		want      []string
	}{
		{
			name:      "appends new packages",
			reqs:      []string{"wheel==0.36.2"},
			buildReqs: []string{"setuptools_scm"},
			want:      []string{"wheel==0.36.2", "setuptools_scm"},
		},
		{
			name:      "existing pin wins over build spec",
			reqs:      []string{"setuptools==57.5.0"},
			buildReqs: []string{"setuptools>=40"},
			want:      []string{"setuptools==57.5.0"},
		},
		{
			name:      "repeats within buildReqs collapse",
			reqs:      []string{},
			buildReqs: []string{"setuptools_scm", "setuptools_scm"},
			want:      []string{"setuptools_scm"},
		},
		{
			name:      "empty requirement ignored",
			reqs:      []string{"wheel"},
			buildReqs: []string{"", "  "},
			want:      []string{"wheel"},
		},
		{
			name:      "marker-qualified variant collapses against pin",
			reqs:      []string{"setuptools==57.5.0"},
			buildReqs: []string{"setuptools; python_version != '3.3'"},
			want:      []string{"setuptools==57.5.0"},
		},
		{
			name:      "extras variant collapses against pin",
			reqs:      []string{"cffi==1.15.0"},
			buildReqs: []string{"cffi[dev]"},
			want:      []string{"cffi==1.15.0"},
		},
		{
			name: "marker variants are all kept",
			reqs: []string{},
			buildReqs: []string{
				"wheel",
				"setuptools",
				"Cython>=0.29.2",
				"numpy==1.13.3; python_version=='3.5'",
				"numpy==1.13.3; python_version=='3.6'",
				"numpy==1.14.5; python_version>='3.7'",
			},
			want: []string{
				"wheel",
				"setuptools",
				"Cython>=0.29.2",
				"numpy==1.13.3; python_version=='3.5'",
				"numpy==1.13.3; python_version=='3.6'",
				"numpy==1.14.5; python_version>='3.7'",
			},
		},
		{
			name: "compound marker variants are all kept",
			reqs: []string{"setuptools==77.0.3"},
			buildReqs: []string{
				"cffi~=1.17; platform_python_implementation != 'PyPy' and python_version < '3.14'",
				"cffi>=2.0.0b; platform_python_implementation != 'PyPy' and python_version >= '3.14'",
				"setuptools>=77.0.0",
			},
			want: []string{
				"setuptools==77.0.3",
				"cffi~=1.17; platform_python_implementation != 'PyPy' and python_version < '3.14'",
				"cffi>=2.0.0b; platform_python_implementation != 'PyPy' and python_version >= '3.14'",
			},
		},
		{
			name:      "unconditional and marker variants are both kept",
			reqs:      []string{},
			buildReqs: []string{"numpy>=1.13", "numpy==1.14.5; python_version>='3.7'"},
			want:      []string{"numpy>=1.13", "numpy==1.14.5; python_version>='3.7'"},
		},
		{
			name:      "repeats of one marker collapse",
			reqs:      []string{},
			buildReqs: []string{"numpy; python_version>='3.7'", "numpy>=1.14; python_version>='3.7'"},
			want:      []string{"numpy; python_version>='3.7'"},
		},
		{
			name:      "all marker variants collapse against pin",
			reqs:      []string{"setuptools<=56.2.0"},
			buildReqs: []string{"setuptools<60; python_version<'3.6'", "setuptools>=61; python_version>='3.6'"},
			want:      []string{"setuptools<=56.2.0"},
		},
		{
			name:      "equivalent name spellings collapse",
			reqs:      []string{"flit_core==3.9.0"},
			buildReqs: []string{"Flit-Core>=3.2", "setuptools_scm", "SetupTools-SCM"},
			want:      []string{"flit_core==3.9.0", "setuptools_scm"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeRequirements(tc.reqs, tc.buildReqs)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("mergeRequirements() diff (-want +got):\n%s", diff)
			}
		})
	}
}

// wheelZipReader builds a wheel with the given WHEEL and METADATA contents so
// inferRequirements can read the Generator line and metadata heuristics.
func wheelZipReader(t *testing.T, pkg, version, wheel, metadata string) *zip.Reader {
	t.Helper()
	dir := expectedDistInfoDir(pkg, version)
	entries := []archive.ZipEntry{
		{FileHeader: &zip.FileHeader{Name: dir + "/WHEEL"}, Body: []byte(wheel)},
		{FileHeader: &zip.FileHeader{Name: dir + "/METADATA"}, Body: []byte(metadata)},
	}
	buf, err := archivetest.ZipFile(entries)
	if err != nil {
		t.Fatalf("ZipFile(): %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("NewReader(): %v", err)
	}
	return zr
}

func TestInferRepoPrefersReleaseLinks(t *testing.T) {
	target := rebuild.Target{Ecosystem: rebuild.PyPI, Package: "charset-normalizer", Version: "2.0.12", Artifact: "charset_normalizer-2.0.12-py3-none-any.whl"}
	for _, tc := range []struct {
		name  string
		calls []httpxtest.Call
		want  string
	}{
		{
			name: "release links win over the project's",
			calls: []httpxtest.Call{
				{URL: "https://pypi.org/pypi/charset-normalizer/2.0.12/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"charset-normalizer","version":"2.0.12","home_page":"https://github.com/ousret/charset_normalizer"}}`)}},
				{URL: "https://pypi.org/pypi/charset-normalizer/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"charset-normalizer","project_urls":{"Code":"https://github.com/jawah/charset_normalizer"}}}`)}},
			},
			want: "https://github.com/ousret/charset_normalizer",
		},
		{
			name: "a project source link beats a repository cited in the release description",
			calls: []httpxtest.Call{
				{URL: "https://pypi.org/pypi/charset-normalizer/2.0.12/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"charset-normalizer","version":"2.0.12","description":"builds on https://github.com/psf/requests","project_urls":{"Issue Tracker":"https://github.com/ousret/charset_normalizer/issues"}}}`)}},
				{URL: "https://pypi.org/pypi/charset-normalizer/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"charset-normalizer","project_urls":{"Source":"https://github.com/jawah/charset_normalizer"}}}`)}},
			},
			want: "https://github.com/jawah/charset_normalizer",
		},
		{
			name: "a release without links falls back to the project",
			calls: []httpxtest.Call{
				{URL: "https://pypi.org/pypi/charset-normalizer/2.0.12/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"charset-normalizer","version":"2.0.12"}}`)}},
				{URL: "https://pypi.org/pypi/charset-normalizer/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"charset-normalizer","project_urls":{"Code":"https://github.com/jawah/charset_normalizer"}}}`)}},
			},
			want: "https://github.com/jawah/charset_normalizer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := rebuild.RegistryMux{PyPI: pypireg.HTTPRegistry{Client: &httpxtest.MockClient{Calls: tc.calls, URLValidator: httpxtest.NewURLValidator(t)}}}
			got, err := Rebuilder{}.InferRepo(context.Background(), target, mux)
			if err != nil {
				t.Fatalf("InferRepo() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("InferRepo() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInferRepoDescriptionCandidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target rebuild.Target
		calls  []httpxtest.Call
		want   string
	}{
		{
			name:   "PrefersRepoNamedAfterPackage",
			target: rebuild.Target{Ecosystem: rebuild.PyPI, Package: "fastcluster", Version: "1.3.0"},
			calls: []httpxtest.Call{
				{URL: "https://pypi.org/pypi/fastcluster/1.3.0/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"fastcluster","version":"1.3.0","home_page":"https://danifold.net","description":"See https://github.com/scipy/scipy/commit/3b22d1d and [my GitHub repository](https://github.com/dmuellner/fastcluster/)."}}`)}},
				{URL: "https://pypi.org/pypi/fastcluster/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"fastcluster","project_urls":{"Homepage":"https://danifold.net"}}}`)}},
			},
			want: "https://github.com/dmuellner/fastcluster",
		},
		{
			name:   "FallsBackToFirstMention",
			target: rebuild.Target{Ecosystem: rebuild.PyPI, Package: "my-pkg", Version: "1.0.0"},
			calls: []httpxtest.Call{
				{URL: "https://pypi.org/pypi/my-pkg/1.0.0/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"my-pkg","version":"1.0.0","description":"Hosted at https://github.com/org/first-repo and https://github.com/org/second-repo"}}`)}},
				{URL: "https://pypi.org/pypi/my-pkg/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"my-pkg"}}`)}},
			},
			want: "https://github.com/org/first-repo",
		},
		{
			name:   "NameIsNormalized",
			target: rebuild.Target{Ecosystem: rebuild.PyPI, Package: "fast-cluster", Version: "1.0.0"},
			calls: []httpxtest.Call{
				{URL: "https://pypi.org/pypi/fast-cluster/1.0.0/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"fast-cluster","version":"1.0.0","description":"See https://github.com/other/project and https://github.com/owner/Fast_Cluster"}}`)}},
				{URL: "https://pypi.org/pypi/fast-cluster/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"fast-cluster"}}`)}},
			},
			want: "https://github.com/owner/fast_cluster",
		},
		{
			name:   "SkipsSponsorsWithinDescription",
			target: rebuild.Target{Ecosystem: rebuild.PyPI, Package: "my-pkg", Version: "1.0.0"},
			calls: []httpxtest.Call{
				{URL: "https://pypi.org/pypi/my-pkg/1.0.0/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"my-pkg","version":"1.0.0","description":"Sponsor https://github.com/sponsors/author or visit https://github.com/author/my-pkg"}}`)}},
				{URL: "https://pypi.org/pypi/my-pkg/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"my-pkg"}}`)}},
			},
			want: "https://github.com/author/my-pkg",
		},
		{
			name:   "OtherLinksInSortedKeyOrder",
			target: rebuild.Target{Ecosystem: rebuild.PyPI, Package: "my-pkg", Version: "1.0.0"},
			calls: []httpxtest.Call{
				{URL: "https://pypi.org/pypi/my-pkg/1.0.0/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"my-pkg","version":"1.0.0","project_urls":{"Tracker":"https://github.com/org/z-repo/issues","Bug Reports":"https://github.com/org/a-repo/issues"}}}`)}},
				{URL: "https://pypi.org/pypi/my-pkg/json", Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(`{"info":{"name":"my-pkg"}}`)}},
			},
			want: "https://github.com/org/a-repo",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := rebuild.RegistryMux{PyPI: pypireg.HTTPRegistry{Client: &httpxtest.MockClient{Calls: tc.calls, URLValidator: httpxtest.NewURLValidator(t)}}}
			got, err := Rebuilder{}.InferRepo(context.Background(), tc.target, mux)
			if err != nil {
				t.Fatalf("InferRepo() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("InferRepo() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPythonTag(t *testing.T) {
	setuptools := []string{"wheel==0.36.2", "setuptools<=56.2.0"}
	for _, tc := range []struct {
		filename string
		reqs     []string
		want     string
	}{
		{"asn1crypto-1.5.1-py2.py3-none-any.whl", setuptools, "py2.py3"},
		{"google_pasta-0.2.0-py2-none-any.whl", setuptools, "py2"},
		{"dacite-1.9.2-py3-none-any.whl", setuptools, ""},
		{"six-1.16.0-py2.py3-none-any.whl", []string{"flit_core==3.9.0"}, ""},
		{"pkg-1.0.tar.gz", setuptools, ""},
	} {
		if got := pythonTag(tc.filename, tc.reqs); got != tc.want {
			t.Errorf("pythonTag(%q, %v) = %q, want %q", tc.filename, tc.reqs, got, tc.want)
		}
	}
}

func TestInferStrategyLocatedError(t *testing.T) {
	const releaseURL = "https://pypi.org/pypi/test-package/1.0.0/json"
	const wheelURL = "https://files.pythonhosted.org/test_package-1.0.0-py3-none-any.whl"
	release := func(urls string) string {
		return `{"info":{"name":"test-package","version":"1.0.0"},"urls":[` + urls + `]}`
	}
	wheel := `{"filename":"test_package-1.0.0-py3-none-any.whl","url":"` + wheelURL + `","size":9,"upload_time_iso_8601":"2023-01-01T12:00:00.000000Z"}`
	for _, tc := range []struct {
		name        string
		calls       []httpxtest.Call
		wantLocated bool // the error carries the resolved location
	}{
		{
			name: "artifact missing from the release",
			calls: []httpxtest.Call{
				{URL: releaseURL, Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(release(""))}},
			},
		},
		{
			name: "unreadable wheel",
			calls: []httpxtest.Call{
				{URL: releaseURL, Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(release(wheel))}},
				{URL: releaseURL, Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(release(wheel))}},
				{URL: wheelURL, Response: &http.Response{StatusCode: 200, Body: httpxtest.Body("not a zip")}},
			},
			wantLocated: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepoFromYAML(`commits:
  - id: initial-commit
    files:
      README.md: |
        # test-package
`, nil))
			ref := repo.Commits["initial-commit"].String()
			target := rebuild.Target{Ecosystem: rebuild.PyPI, Package: "test-package", Version: "1.0.0", Artifact: "test_package-1.0.0-py3-none-any.whl"}
			mux := rebuild.RegistryMux{PyPI: pypireg.HTTPRegistry{Client: &httpxtest.MockClient{Calls: tc.calls, URLValidator: httpxtest.NewURLValidator(t)}}}
			rcfg := &rebuild.RepoConfig{Repo: gitx.Repo{Repository: repo.Repository}, URI: "https://github.com/test-org/test-package"}
			s, err := Rebuilder{}.InferStrategy(context.Background(), target, mux, rcfg, &rebuild.LocationHint{Location: rebuild.Location{Repo: rcfg.URI, Ref: ref}})
			if err == nil {
				t.Fatalf("InferStrategy expected error, got %v", s)
			}
			var located *rebuild.InferenceError
			if errors.As(err, &located) != tc.wantLocated {
				t.Fatalf("located error = %v, want %v: %v", !tc.wantLocated, tc.wantLocated, err)
			}
			if !tc.wantLocated {
				return
			}
			want := rebuild.InferenceErrorDetail{Location: rebuild.Location{Repo: rcfg.URI, Ref: ref}, Published: time.Date(2023, time.January, 1, 12, 0, 0, 0, time.UTC)}
			if diff := cmp.Diff(want, located.Detail); diff != "" {
				t.Errorf("located detail diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestExtractPlatformTag(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{
			name:     "standard 5-part wheel",
			filename: "foo-1.0.0-cp310-cp310-manylinux_2_17_x86_64.whl",
			want:     "manylinux_2_17_x86_64",
		},
		{
			name:     "6-part wheel with build tag",
			filename: "foo-1.0.0-1-cp310-cp310-manylinux_2_17_x86_64.whl",
			want:     "manylinux_2_17_x86_64",
		},
		{
			name:     "pure wheel",
			filename: "foo-1.0.0-py3-none-any.whl",
			want:     "any",
		},
		{
			name:     "compressed platform tags",
			filename: "foo-2.4.3-cp314-cp314-manylinux_2_27_x86_64.manylinux_2_28_x86_64.whl",
			want:     "manylinux_2_27_x86_64.manylinux_2_28_x86_64",
		},
		{
			name:     "non-wheel",
			filename: "foo-1.0.0.tar.gz",
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractPlatformTag(tt.filename); got != tt.want {
				t.Errorf("extractPlatformTag(%q) = %q, want %q", tt.filename, got, tt.want)
			}
		})
	}
}

func TestFindPlatformWheel(t *testing.T) {
	artifacts := []pypireg.Artifact{
		{Filename: "foo-1.0.0.tar.gz"},
		{Filename: "foo-1.0.0-py3-none-any.whl"},
		{Filename: "foo-1.0.0-cp310-cp310-win_amd64.whl"},
		{Filename: "foo-1.0.0-cp310-cp310-macosx_11_0_arm64.whl"},
		{Filename: "foo-1.0.0-cp310-cp310-manylinux2014_x86_64.whl"},
	}
	art, err := FindPlatformWheel(artifacts)
	if err != nil {
		t.Fatalf("FindPlatformWheel() error = %v", err)
	}
	if art.Filename != "foo-1.0.0-cp310-cp310-manylinux2014_x86_64.whl" {
		t.Errorf("FindPlatformWheel() = %v, want foo-1.0.0-cp310-cp310-manylinux2014_x86_64.whl", art.Filename)
	}
}

func TestExtractWheelTags(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     WheelTags
	}{
		{
			name:     "standard 5-part wheel",
			filename: "foo-1.0.0-cp310-cp310-manylinux_2_17_x86_64.whl",
			want: WheelTags{
				Python:   "cp310",
				ABI:      "cp310",
				Platform: "manylinux_2_17_x86_64",
			},
		},
		{
			name:     "abi3 wheel",
			filename: "cryptography-41.0.0-cp37-abi3-manylinux_2_28_x86_64.whl",
			want: WheelTags{
				Python:   "cp37",
				ABI:      "abi3",
				Platform: "manylinux_2_28_x86_64",
			},
		},
		{
			name:     "6-part wheel with build tag",
			filename: "foo-1.0.0-1-cp310-cp310-manylinux_2_17_x86_64.whl",
			want: WheelTags{
				Python:   "cp310",
				ABI:      "cp310",
				Platform: "manylinux_2_17_x86_64",
			},
		},
		{
			name:     "compressed platform tag set",
			filename: "foo-2.4.3-cp310-cp310-manylinux1_x86_64.manylinux_2_28_x86_64.whl",
			want: WheelTags{
				Python:   "cp310",
				ABI:      "cp310",
				Platform: "manylinux1_x86_64.manylinux_2_28_x86_64",
			},
		},
		{
			name:     "non-wheel",
			filename: "foo-1.0.0.tar.gz",
			want:     WheelTags{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractWheelTags(tt.filename)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("extractWheelTags(%q) diff (-want +got):\n%s", tt.filename, diff)
			}
		})
	}
}

func TestInferWheelBuildEnv(t *testing.T) {
	tests := []struct {
		name       string
		repoYAML   string
		wheelFiles []string
		tags       WheelTags
		reqs       []string
		wantEnv    []string
		wantReqs   []string
	}{
		{
			name: "MypyInTreeMypyc",
			repoYAML: `commits:
  - id: initial
    files:
      setup.py: |
        import os
        if os.getenv("MYPY_USE_MYPYC", None) == "1":
            USE_MYPYC = True
      mypyc/build.py: |
        def mypycify(): pass
`,
			wheelFiles: []string{
				"mypy/api.cpython-315-x86_64-linux-gnu.so",
				"08ae81f72d5a2b5fa9e0__mypyc.cpython-315-x86_64-linux-gnu.so",
			},
			tags:     WheelTags{Python: "cp315", ABI: "cp315", Platform: "manylinux_2_28_x86_64"},
			reqs:     []string{"setuptools==84.0.0"},
			wantEnv:  []string{"MYPY_USE_MYPYC=1"},
			wantReqs: nil,
		},
		{
			name: "ExternalMypycAddsMypyRequirement",
			repoYAML: `commits:
  - id: initial
    files:
      setup.py: |
        import os
        USE_MYPYC = os.getenv("CHARSET_NORMALIZER_USE_MYPYC", "0") == "1"
`,
			wheelFiles: []string{
				"charset_normalizer/md__mypyc.cpython-312-x86_64-linux-gnu.so",
			},
			tags:     WheelTags{Python: "cp312", ABI: "cp312", Platform: "manylinux_2_28_x86_64"},
			reqs:     []string{"setuptools==82.0.1"},
			wantEnv:  []string{"CHARSET_NORMALIZER_USE_MYPYC=1"},
			wantReqs: []string{"mypy"},
		},
		{
			name: "CharsetNormalizerCythonABI3",
			repoYAML: `commits:
  - id: initial
    files:
      setup.py: |
        import os
        USE_CYTHON = os.getenv("CHARSET_NORMALIZER_USE_CYTHON") == "1"
        LIMITED_API = os.getenv("CHARSET_NORMALIZER_CYTHON_ABI3") == "1"
      _build_hook/backend.py: |
        CYTHON_SPEC = "Cython>=3.2,<3.3"
`,
			wheelFiles: []string{
				"charset_normalizer/md.abi3.so",
				"charset_normalizer/cd.abi3.so",
			},
			tags:     WheelTags{Python: "cp37", ABI: "abi3", Platform: "musllinux_1_2_x86_64"},
			reqs:     []string{"setuptools==82.0.1"},
			wantEnv:  []string{"CHARSET_NORMALIZER_USE_CYTHON=1", "CHARSET_NORMALIZER_CYTHON_ABI3=1"},
			wantReqs: []string{"Cython>=3.2,<3.3"},
		},
		{
			name: "CharsetNormalizerCythonNativeOmitsABI3Var",
			repoYAML: `commits:
  - id: initial
    files:
      setup.py: |
        import os
        USE_CYTHON = os.getenv("CHARSET_NORMALIZER_USE_CYTHON") == "1"
        LIMITED_API = os.getenv("CHARSET_NORMALIZER_CYTHON_ABI3") == "1"
      _build_hook/backend.py: |
        CYTHON_SPEC = "Cython>=3.2,<3.3"
`,
			wheelFiles: []string{
				"charset_normalizer/md.cpython-312-x86_64-linux-gnu.so",
			},
			tags:     WheelTags{Python: "cp312", ABI: "cp312", Platform: "manylinux_2_28_x86_64"},
			reqs:     []string{"setuptools==82.0.1"},
			wantEnv:  []string{"CHARSET_NORMALIZER_USE_CYTHON=1"},
			wantReqs: []string{"Cython>=3.2,<3.3"},
		},
		{
			name: "FlitCoreBackendIgnored",
			repoYAML: `commits:
  - id: initial
    files:
      setup.py: |
        import os
        if os.environ.get("TOMLI_USE_MYPYC") == "1":
            pass
`,
			wheelFiles: []string{
				"tomli/_parser__mypyc.cpython-312-x86_64-linux-gnu.so",
			},
			tags:     WheelTags{Python: "cp312", ABI: "cp312", Platform: "manylinux_2_28_x86_64"},
			reqs:     []string{"setuptools==82.0.1", "flit_core>=3.12,<4"},
			wantEnv:  nil,
			wantReqs: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepoFromYAML(tc.repoYAML, nil))
			commit, err := repo.Repository.CommitObject(repo.Commits["initial"])
			if err != nil {
				t.Fatalf("CommitObject() error = %v", err)
			}
			tree, err := commit.Tree()
			if err != nil {
				t.Fatalf("Tree() error = %v", err)
			}
			zr := testZipReader(t, tc.wheelFiles)
			gotEnv, gotReqs := inferWheelBuildEnv(tree, "", zr, tc.tags, tc.reqs)
			if diff := cmp.Diff(tc.wantEnv, gotEnv); diff != "" {
				t.Errorf("inferWheelBuildEnv() env diff (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantReqs, gotReqs); diff != "" {
				t.Errorf("inferWheelBuildEnv() reqs diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInferStrategyDynamicBuildRequirements(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pkg       string
		version   string
		artifact  string
		generator string
		repoYAML  string
		wantReqs  []string
	}{
		{
			name:      "InTreeBackendWithCibuildwheelConstraint",
			pkg:       "frozenlist",
			version:   "1.8.0",
			artifact:  "frozenlist-1.8.0-cp312-cp312-musllinux_1_2_x86_64.whl",
			generator: "setuptools (80.9.0)",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["expandvars", "setuptools >= 47"]
        backend-path = ["packaging"]
        build-backend = "pep517_backend.hooks"
        [tool.cibuildwheel.environment]
        PIP_CONSTRAINT = "requirements/cython.txt"
      requirements/cython.txt: |
        cython==3.1.4
`,
			wantReqs: []string{"setuptools==80.9.0", "expandvars", "cython==3.1.4"},
		},
		{
			name:      "HatchCustomHookWithBuildDependencyGroup",
			pkg:       "dank-mids",
			version:   "4.20.215",
			artifact:  "dank_mids-4.20.215-cp312-cp312-manylinux_2_28_x86_64.whl",
			generator: "hatchling 1.31.0",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["hatchling>=1.27", "tomli>=2; python_version < '3.11'"]
        build-backend = "hatchling.build"
        [dependency-groups]
        build = [
            "mypy[mypyc]==2.2.0",
            "setuptools",
            "tomli>=2; python_version < '3.11'",
        ]
        [tool.hatch.build.targets.wheel.hooks.custom]
        path = "hatch_build.py"
`,
			wantReqs: []string{"hatchling==1.31.0", "tomli>=2; python_version < '3.11'", "mypy[mypyc]==2.2.0", "setuptools"},
		},
		{
			name:      "PDMRunSetuptoolsOnCPython312",
			pkg:       "editdistance",
			version:   "0.8.1",
			artifact:  "editdistance-0.8.1-cp312-cp312-musllinux_1_1_x86_64.whl",
			generator: "pdm-backend (2.1.8)",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["pdm-backend", "cython"]
        build-backend = "pdm.backend"
        [tool.pdm.build]
        run-setuptools = true
`,
			wantReqs: []string{"pdm-backend==2.1.8", "cython", "setuptools"},
		},
		{
			name:      "PDMRunSetuptoolsOnCPython311Unchanged",
			pkg:       "editdistance",
			version:   "0.8.1",
			artifact:  "editdistance-0.8.1-cp311-cp311-musllinux_1_1_x86_64.whl",
			generator: "pdm-backend (2.1.8)",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["pdm-backend", "cython"]
        build-backend = "pdm.backend"
        [tool.pdm.build]
        run-setuptools = true
`,
			wantReqs: []string{"pdm-backend==2.1.8", "cython"},
		},
		{
			name:      "ScikitBuildCoreAddsNinja",
			pkg:       "coincurve",
			version:   "21.0.0",
			artifact:  "coincurve-21.0.0-cp312-cp312-musllinux_1_2_x86_64.whl",
			generator: "hatchling 1.27.0",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        build-backend = "hatchling.build"
        requires = ["hatchling>=1.24.2", "cffi", "setuptools", "scikit-build-core>=0.9.0"]
`,
			wantReqs: []string{"hatchling==1.27.0", "cffi", "setuptools", "scikit-build-core>=0.9.0", "ninja"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepoFromYAML(tc.repoYAML, nil))
			ref := repo.Commits["initial-commit"].String()
			dir := expectedDistInfoDir(tc.pkg, tc.version)
			wheelBuf := must(archivetest.ZipFile([]archive.ZipEntry{
				{FileHeader: &zip.FileHeader{Name: dir + "/WHEEL"}, Body: []byte("Wheel-Version: 1.0\nGenerator: " + tc.generator + "\n")},
				{FileHeader: &zip.FileHeader{Name: dir + "/METADATA"}, Body: []byte("Metadata-Version: 2.1\nName: " + tc.pkg + "\n")},
			}))
			releaseURL := "https://pypi.org/pypi/" + tc.pkg + "/" + tc.version + "/json"
			wheelURL := "https://files.pythonhosted.org/" + tc.artifact
			releaseJSON := `{"info":{"name":"` + tc.pkg + `","version":"` + tc.version + `"},"urls":[{"filename":"` + tc.artifact + `","url":"` + wheelURL + `","size":` + itoa(wheelBuf.Len()) + `,"upload_time_iso_8601":"2025-01-01T00:00:00Z"}]}`
			mux := rebuild.RegistryMux{
				PyPI: pypireg.HTTPRegistry{
					Client: &httpxtest.MockClient{
						Calls: []httpxtest.Call{
							{
								URL:      releaseURL,
								Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(releaseJSON)},
							},
							{
								URL:      releaseURL,
								Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(releaseJSON)},
							},
							{
								URL:      wheelURL,
								Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(wheelBuf.String())},
							},
						},
						URLValidator: httpxtest.NewURLValidator(t),
					},
				},
			}
			rcfg := &rebuild.RepoConfig{Repo: gitx.Repo{Repository: repo.Repository}, URI: "https://github.com/example/" + tc.pkg}
			target := rebuild.Target{Ecosystem: rebuild.PyPI, Package: tc.pkg, Version: tc.version, Artifact: tc.artifact}
			s, err := Rebuilder{}.InferStrategy(context.Background(), target, mux, rcfg, &rebuild.LocationHint{Location: rebuild.Location{Repo: rcfg.URI, Ref: ref}})
			if err != nil {
				t.Fatalf("InferStrategy() error = %v", err)
			}
			pwb, ok := s.(*PlatformWheelBuild)
			if !ok {
				t.Fatalf("InferStrategy() = %T, want *PlatformWheelBuild", s)
			}
			if diff := cmp.Diff(tc.wantReqs, pwb.Requirements); diff != "" {
				t.Errorf("PlatformWheelBuild.Requirements diff (-want +got):\n%s", diff)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestInferLegacyPythonBaseImage(t *testing.T) {
	upload2026 := time.Date(2026, time.February, 8, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		tags          WheelTags
		baseImageRepo string
		cibwVersion   string
		uploadTime    time.Time
		want          string
	}{
		{
			name:          "ModernCP310LeavesBaseImageEmpty",
			tags:          WheelTags{Python: "cp310", ABI: "cp310", Platform: "manylinux_2_28_x86_64"},
			baseImageRepo: "quay.io/pypa/manylinux_2_28_x86_64",
			uploadTime:    upload2026,
			want:          "",
		},
		{
			name:          "ABI3CP37LeavesBaseImageEmpty",
			tags:          WheelTags{Python: "cp37", ABI: "abi3", Platform: "manylinux_2_28_x86_64"},
			baseImageRepo: "quay.io/pypa/manylinux_2_28_x86_64",
			uploadTime:    upload2026,
			want:          "",
		},
		{
			name:          "Musllinux1_1CP38LeavesBaseImageEmpty",
			tags:          WheelTags{Python: "cp38", ABI: "cp38", Platform: "musllinux_1_1_x86_64"},
			baseImageRepo: "quay.io/pypa/musllinux_1_1_x86_64",
			uploadTime:    upload2026,
			want:          "",
		},
		{
			name:          "CP27SelectsPinnedManylinux2010",
			tags:          WheelTags{Python: "cp27", ABI: "cp27mu", Platform: "manylinux1_x86_64"},
			baseImageRepo: "quay.io/pypa/manylinux2014_x86_64",
			uploadTime:    upload2026,
			want:          "quay.io/pypa/manylinux2010_x86_64:2021-02-06-3d322a5",
		},
		{
			name:          "CP37Manylinux2010SelectsFinalManylinux2010",
			tags:          WheelTags{Python: "cp37", ABI: "cp37m", Platform: "manylinux_2_5_x86_64.manylinux1_x86_64.manylinux_2_12_x86_64.manylinux2010_x86_64"},
			baseImageRepo: "quay.io/pypa/manylinux2014_x86_64",
			uploadTime:    upload2026,
			want:          "quay.io/pypa/manylinux2010_x86_64:2022-08-05-4535177",
		},
		{
			name:          "CP38UsesWorkflowPin",
			tags:          WheelTags{Python: "cp38", ABI: "cp38", Platform: "manylinux_2_28_x86_64"},
			baseImageRepo: "quay.io/pypa/manylinux_2_28_x86_64",
			cibwVersion:   "3.1.4",
			uploadTime:    upload2026,
			want:          "quay.io/pypa/manylinux_2_28_x86_64:2025.08.15-1@sha256:6f42f4382bc73e9584206e6722e001922c0d4846c932fccaa04c0e35903b717d",
		},
		{
			name:          "CP36FallsBackTo2_23_3WhenWorkflowHasNo2xPin",
			tags:          WheelTags{Python: "cp36", ABI: "cp36m", Platform: "musllinux_1_2_x86_64"},
			baseImageRepo: "quay.io/pypa/musllinux_1_2_x86_64",
			uploadTime:    upload2026,
			want:          "quay.io/pypa/musllinux_1_2_x86_64:2025.04.19-1@sha256:37645a6deff8f81eabf7e709c719f4ef2d11182e10997593d2f9f7d2dd9864cd",
		},
		{
			name:          "CP37OnManylinux2014ClampsOldWorkflowPinTo2_20_0",
			tags:          WheelTags{Python: "cp37", ABI: "cp37m", Platform: "manylinux_2_17_x86_64.manylinux2014_x86_64"},
			baseImageRepo: "quay.io/pypa/manylinux2014_x86_64",
			cibwVersion:   "2.10.2",
			uploadTime:    time.Date(2023, time.May, 3, 0, 0, 0, 0, time.UTC),
			want:          "quay.io/pypa/manylinux2014_x86_64:2024.08.03-1@sha256:e620e1be73a5cc9aa8fbefa17faf545b61aa9417887030dae568db2137cb19b4",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			if needsLegacyPythonBaseImage(tc.tags, tc.baseImageRepo) {
				got = inferLegacyPythonBaseImage(tc.tags, tc.baseImageRepo, tc.cibwVersion, tc.uploadTime)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInferStrategySanitizeSetupCfg(t *testing.T) {
	const releaseURL = "https://pypi.org/pypi/SQLAlchemy/2.0.52/json"
	const wheelURL = "https://files.pythonhosted.org/sqlalchemy-2.0.52-cp312-cp312-musllinux_1_2_x86_64.whl"
	uploadTime := time.Date(2026, time.August, 11, 21, 16, 59, 0, time.UTC)
	whlBytes := wheelZip(t, []archive.ZipEntry{
		zipEntry("sqlalchemy-2.0.52.dist-info/WHEEL", "Wheel-Version: 1.0\nGenerator: setuptools (84.0.0)\nRoot-Is-Purelib: false\nTag: cp312-cp312-musllinux_1_2_x86_64\n"),
		zipEntry("sqlalchemy-2.0.52.dist-info/METADATA", "Metadata-Version: 2.1\nName: SQLAlchemy\nVersion: 2.0.52\nLicense-File: LICENSE\n"),
	})
	releaseJSON := fmt.Sprintf(`{"info":{"name":"SQLAlchemy","version":"2.0.52"},"urls":[{"filename":"sqlalchemy-2.0.52-cp312-cp312-musllinux_1_2_x86_64.whl","url":%q,"size":%d,"upload_time_iso_8601":"2026-08-11T21:16:59Z"}]}`, wheelURL, len(whlBytes))
	tests := []struct {
		name         string
		setupCfg     string
		wantSanitize bool
	}{
		{
			name: "ConflictingTagBuildSanitized",
			setupCfg: `[metadata]
name = SQLAlchemy
version = attr: sqlalchemy.__version__

[egg_info]
tag_build = dev
`,
			wantSanitize: true,
		},
		{
			name: "EmptyTagBuildNotSanitized",
			setupCfg: `[metadata]
name = SQLAlchemy
version = attr: sqlalchemy.__version__

[egg_info]
tag_build =
tag_date = 0
`,
			wantSanitize: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepo([]gitxtest.Commit{
				{
					ID:  "c1",
					Tag: "v2.0.52",
					Files: gitxtest.FileContent{
						"pyproject.toml": "[build-system]\nrequires = [\"setuptools>=61.0\"]\n",
						"setup.cfg":      tc.setupCfg,
					},
				},
			}, nil))
			ref := repo.Commits["c1"].String()
			calls := []httpxtest.Call{
				{URL: releaseURL, Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(releaseJSON)}},
				{URL: releaseURL, Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(releaseJSON)}},
				{URL: wheelURL, Response: &http.Response{StatusCode: 200, Body: httpxtest.Body(string(whlBytes))}},
			}
			mux := rebuild.RegistryMux{PyPI: pypireg.HTTPRegistry{Client: &httpxtest.MockClient{Calls: calls, URLValidator: httpxtest.NewURLValidator(t)}}}
			rcfg := &rebuild.RepoConfig{Repo: gitx.Repo{Repository: repo.Repository}, URI: "https://github.com/sqlalchemy/sqlalchemy"}
			target := rebuild.Target{
				Ecosystem: rebuild.PyPI,
				Package:   "SQLAlchemy",
				Version:   "2.0.52",
				Artifact:  "sqlalchemy-2.0.52-cp312-cp312-musllinux_1_2_x86_64.whl",
			}
			got, err := Rebuilder{}.InferStrategy(context.Background(), target, mux, rcfg, nil)
			if err != nil {
				t.Fatalf("InferStrategy() error = %v", err)
			}
			want := &PlatformWheelBuild{
				Location: rebuild.Location{
					Repo: "https://github.com/sqlalchemy/sqlalchemy",
					Ref:  ref,
				},
				PythonTag:        "cp312",
				ABITag:           "cp312",
				PlatformTag:      "musllinux_1_2_x86_64",
				Requirements:     []string{"setuptools==84.0.0"},
				RegistryTime:     uploadTime,
				SanitizeSetupCfg: tc.wantSanitize,
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("InferStrategy() diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCapSetuptoolsForCP36(t *testing.T) {
	tests := []struct {
		name string
		reqs []string
		want []string
	}{
		{
			name: "caps default ceiling",
			reqs: []string{"wheel==0.40.0", "setuptools<=67.7.2"},
			want: []string{"wheel==0.40.0", "setuptools<=59.6.0"},
		},
		{
			name: "preserves lower ceiling",
			reqs: []string{"wheel==0.37.1", "setuptools<=56.2.0"},
			want: []string{"wheel==0.37.1", "setuptools<=56.2.0"},
		},
		{
			name: "preserves exact pin",
			reqs: []string{"setuptools==58.0.0"},
			want: []string{"setuptools==58.0.0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := capSetuptoolsForCP36(tt.reqs)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("capSetuptoolsForCP36(%v) diff (-want +got):\n%s", tt.reqs, diff)
			}
		})
	}
}
