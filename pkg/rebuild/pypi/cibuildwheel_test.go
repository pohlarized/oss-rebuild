// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/oss-rebuild/internal/gitx/gitxtest"
	"github.com/google/oss-rebuild/internal/textwrap"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/platform"
)

func TestCibuildwheelSpecs(t *testing.T) {
	tests := []struct {
		name     string
		workflow string
		want     []string
		wantUses bool
	}{
		{
			name: "ActionTag",
			workflow: `
				jobs:
				  wheels:
				    steps:
				      - uses: actions/checkout@v4
				      - uses: pypa/cibuildwheel@v2.16.2`,
			want:     []string{"2.16.2"},
			wantUses: true,
		},
		{
			name: "ActionMinorTag",
			workflow: `
				jobs:
				  wheels:
				    steps:
				      - uses: pypa/cibuildwheel@v2.16`,
			want:     []string{"2.16"},
			wantUses: true,
		},
		{
			name: "ActionCommit",
			workflow: `
				jobs:
				  wheels:
				    steps:
				      - uses: pypa/cibuildwheel@` + testCommit("5") + ` # v2.16.2`,
			want:     []string{testCommit("5")},
			wantUses: true,
		},
		{
			name: "ActionBranchOrMajorOrPreReleaseTagIgnoredForSpecs",
			workflow: `
				jobs:
				  wheels:
				    steps:
				      - uses: pypa/cibuildwheel@main
				      - uses: pypa/cibuildwheel@release/2.16
				      - uses: pypa/cibuildwheel@v2
				      - uses: pypa/cibuildwheel@v2.16.0b1`,
			want:     nil,
			wantUses: true,
		},
		{
			name: "PipAndPipxRequirementsAcrossJobs",
			workflow: `
				jobs:
				  wheels:
				    steps:
				      - uses: pypa/cibuildwheel@v1.12.0
				  sdist:
				    steps:
				      - run: |
				          python -m pip install "cibuildwheel[uv] == 3.1.0"
				          pipx run cibuildwheel==2.16.2 --output-dir wheelhouse`,
			want:     []string{"3.1.0", "2.16.2", "1.12.0"},
			wantUses: true,
		},
		{
			name: "RangeAndPreReleaseIgnoredForSpecs",
			workflow: `
				jobs:
				  wheels:
				    steps:
				      - run: |
				          pip install "cibuildwheel>=2.16,<3"
				          pip install cibuildwheel==3.0.0b1`,
			want:     nil,
			wantUses: true,
		},
		{
			name: "UnpinnedCommand",
			workflow: `
				jobs:
				  wheels:
				    steps:
				      - run: python -m cibuildwheel --output-dir wheelhouse`,
			want:     nil,
			wantUses: true,
		},
		{
			name: "SimilarName",
			workflow: `
				jobs:
				  wheels:
				    steps:
				      - run: pip install cibuildwheel_fork`,
			want:     nil,
			wantUses: false,
		},
		{
			name: "CommentedOut",
			workflow: `
				jobs:
				  wheels:
				    steps:
				      # - uses: pypa/cibuildwheel@v2.16.2
				      - uses: actions/checkout@v4`,
			want:     nil,
			wantUses: false,
		},
		{
			name:     "InvalidYAML",
			workflow: `jobs: [`,
			want:     nil,
			wantUses: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotUses := cibuildwheelSpecs([]byte(textwrap.Dedent(tc.workflow)))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("cibuildwheelSpecs() specs diff (-want +got):\n%s", diff)
			}
			if gotUses != tc.wantUses {
				t.Errorf("cibuildwheelSpecs() uses = %t, want %t", gotUses, tc.wantUses)
			}
		})
	}
}

func TestExtractCibuildwheelSpecs(t *testing.T) {
	wheels := textwrap.Dedent(`
		jobs:
		  wheels:
		    steps:
		      - uses: pypa/cibuildwheel@v2.16.2`)
	tests := []struct {
		name     string
		files    gitxtest.FileContent
		want     []string
		wantUses bool
	}{
		{
			name: "AcrossWorkflows",
			files: gitxtest.FileContent{
				".github/workflows/test.yaml": textwrap.Dedent(`
					jobs:
					  test:
					    steps:
					      - run: pip install cibuildwheel==2.3.1`),
				".github/workflows/wheels.yml":         wheels,
				".github/workflows/wheels.yml.bak":     wheels,
				".github/workflows/nested/wheels.yaml": wheels,
			},
			want:     []string{"2.3.1", "2.16.2"},
			wantUses: true,
		},
		{
			name: "UnpinnedSecondWorkflow",
			files: gitxtest.FileContent{
				".github/workflows/test.yml": textwrap.Dedent(`
					jobs:
					  test:
					    steps:
					      - run: pytest`),
				".github/workflows/wheels.yml": textwrap.Dedent(`
					jobs:
					  wheels:
					    steps:
					      - run: pipx run cibuildwheel`),
			},
			want:     nil,
			wantUses: true,
		},
		{
			name:     "NoWorkflows",
			files:    gitxtest.FileContent{"README.md": "# test-package\n"},
			want:     nil,
			wantUses: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepo([]gitxtest.Commit{{ID: "initial-commit", Files: tc.files}}, nil))
			commit := must(repo.CommitObject(repo.Commits["initial-commit"]))
			got, gotUses, err := extractCibuildwheelSpecs(must(commit.Tree()))
			if err != nil {
				t.Fatalf("extractCibuildwheelSpecs() returned error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("extractCibuildwheelSpecs() specs diff (-want +got):\n%s", diff)
			}
			if gotUses != tc.wantUses {
				t.Errorf("extractCibuildwheelSpecs() uses = %t, want %t", gotUses, tc.wantUses)
			}
		})
	}
}

func TestExtractCibuildwheelDependencies(t *testing.T) {
	tests := []struct {
		name      string
		searchDir string
		libc      platform.LibcImpl
		repoYAML  string
		want      []string
	}{
		{
			name:      "TopLevelAndLinuxHooksGlibc",
			searchDir: "",
			libc:      platform.Glibc,
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [project]
        name = "example"
        version = "1.0.0"

        [tool.cibuildwheel]
        before-all = "yum install -y libffi-devel"
        before-build = ["dnf install -y zlib-devel"]

        [tool.cibuildwheel.linux]
        before-all = "apk add --no-cache graphviz-dev"
`,
			want: []string{"libffi-devel", "zlib-devel"},
		},
		{
			name:      "TopLevelAndLinuxHooksMusl",
			searchDir: "",
			libc:      platform.Musl,
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [project]
        name = "example"
        version = "1.0.0"

        [tool.cibuildwheel]
        before-all = "yum install -y libffi-devel"
        before-build = ["dnf install -y zlib-devel"]

        [tool.cibuildwheel.linux]
        before-all = "apk add --no-cache graphviz-dev"
`,
			want: []string{"graphviz-dev"},
		},
		{
			name:      "OverridesFilteringManylinuxTarget",
			searchDir: "",
			libc:      platform.Glibc,
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [[tool.cibuildwheel.overrides]]
        select = "*-macosx* *-manylinux*"
        before-all = "dnf install -y gd-devel cairo-devel"

        [[tool.cibuildwheel.overrides]]
        select = "*-musllinux*"
        before-all = "dnf install -y musl-only-should-be-skipped"

        [[tool.cibuildwheel.overrides]]
        select = "*-manylinux_aarch64"
        before-all = "dnf install -y aarch64-only-should-be-skipped"

        [[tool.cibuildwheel.overrides]]
        select = "*-macosx*"
        before-all = "yum install -y should-not-be-extracted"

        [[tool.cibuildwheel.overrides]]
        select = ["*-win_amd64", "*-win32"]
        before-all = "dnf install -y windows-ignored"
`,
			want: []string{"gd-devel", "cairo-devel"},
		},
		{
			name:      "OverridesFilteringMusllinuxTarget",
			searchDir: "",
			libc:      platform.Musl,
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [[tool.cibuildwheel.overrides]]
        select = "*-macosx* *-manylinux*"
        before-all = "apk add manylinux-only-should-be-skipped"

        [[tool.cibuildwheel.overrides]]
        select = "*-musllinux* *-manylinux_i686"
        before-all = "apk add gd-dev cairo-dev"

        [[tool.cibuildwheel.overrides]]
        select = "*-macosx*"
        before-all = "apk add should-not-be-extracted"
`,
			want: []string{"gd-dev", "cairo-dev"},
		},
		{
			name:      "SubdirectorySearch",
			searchDir: "python",
			libc:      platform.Glibc,
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [tool.cibuildwheel.linux]
        before-all = "yum install -y openssl-devel"
      python/pyproject.toml: |
        [project]
        name = "subpkg"
        version = "0.1.0"
        [tool.cibuildwheel]
        before-build = "yum install -y curl-devel"
`,
			want: []string{"curl-devel"},
		},
		{
			name:      "NoPyprojectToml",
			searchDir: "",
			libc:      platform.Glibc,
			repoYAML: `
commits:
  - id: initial-commit
    files:
      setup.py: |
        from setuptools import setup
        setup(name="legacy", version="0.1.0")
`,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepoFromYAML(tc.repoYAML, nil))
			commit := must(repo.CommitObject(repo.Commits["initial-commit"]))
			tree := must(commit.Tree())
			got, err := extractCibuildwheelDependencies(tree, tc.searchDir, tc.libc)
			if err != nil {
				t.Fatalf("extractCibuildwheelDependencies() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("extractCibuildwheelDependencies() diff (-want +got):\n%s", diff)
			}
		})
	}
}
