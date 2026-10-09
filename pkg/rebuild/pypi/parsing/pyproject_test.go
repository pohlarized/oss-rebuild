// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package parsing

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/oss-rebuild/internal/gitx/gitxtest"
)

func TestExtractPyProjectRequirements(t *testing.T) {
	for _, tc := range []struct {
		name     string
		repoYAML string
		want     []string
		wantErr  bool
	}{
		{
			name: "SurroundingWhitespaceTrimmed",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["  foo  ", "bar >= 1.0  "]
`,
			want: []string{"foo", "bar >= 1.0"},
		},
		{
			// NOTE: Collapsing all whitespace fuses marker keywords like "and" with their operands.
			name: "MarkerWhitespacePreserved",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = [
            "foo ~= 1.0; python_version < '3.14' and platform_python_implementation != 'PyPy'",
            "bar ; sys_platform == 'win32'",
        ]
`,
			want: []string{
				"foo ~= 1.0; python_version < '3.14' and platform_python_implementation != 'PyPy'",
				"bar ; sys_platform == 'win32'",
			},
		},
		{
			name: "EmptyRequires",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = []
`,
			want: nil,
		},
		{
			name: "MissingRequires",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        build-backend = "flit_core.buildapi"
`,
			want: nil,
		},
		{
			name: "MissingBuildSystem",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [project]
        name = "foo"
`,
			want: nil,
		},
		{
			name: "HatchCustomHookMergesBuildDependencyGroup",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["hatchling>=1.27"]
        build-backend = "hatchling.build"
        [dependency-groups]
        base-types = ["types-requests"]
        build = [
            "mypy[mypyc]==2.2.0",
            "setuptools",
            { include-group = "base-types" },
        ]
        [tool.hatch.build.targets.wheel.hooks.custom]
        path = "hatch_build.py"
`,
			want: []string{
				"hatchling>=1.27",
				"mypy[mypyc]==2.2.0",
				"setuptools",
				"types-requests",
			},
		},
		{
			name: "DependencyGroupIgnoredWithoutHatchCustomHook",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["hatchling>=1.27"]
        [dependency-groups]
        build = ["mypy[mypyc]==2.2.0"]
`,
			want: []string{"hatchling>=1.27"},
		},
		{
			name: "ScikitBuildCoreAddsNinja",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["hatchling>=1.24.2", "scikit-build-core>=0.9.0"]
`,
			want: []string{"hatchling>=1.24.2", "scikit-build-core>=0.9.0", "ninja"},
		},
		{
			name: "InvalidTOML",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["foo"
`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepoFromYAML(tc.repoYAML, nil))
			commit := must(repo.CommitObject(repo.Commits["initial-commit"]))
			tree := must(commit.Tree())
			f := must(tree.File("pyproject.toml"))
			got, err := extractPyProjectRequirements(context.Background(), f)
			if (err != nil) != tc.wantErr {
				t.Fatalf("extractPyProjectRequirements() error = %v, wantErr %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("extractPyProjectRequirements() diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestExtractDynamicBuildRequirements(t *testing.T) {
	for _, tc := range []struct {
		name      string
		repoYAML  string
		searchDir string
		pythonTag string
		want      []string
		wantErr   bool
	}{
		{
			name: "BackendPathWithCibuildwheelConstraint",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["setuptools >= 47", "expandvars"]
        backend-path = ["packaging"]
        build-backend = "pep517_backend.hooks"
        [tool.cibuildwheel.environment]
        PIP_CONSTRAINT = "requirements/cython.txt"
      requirements/cython.txt: |
        # Pinned for wheel builds
        cython==3.1.4 --hash=sha256:deadbeef
`,
			pythonTag: "cp312",
			want:      []string{"cython==3.1.4"},
		},
		{
			name: "BackendPathLinuxEnvironmentOverridesTopLevel",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["setuptools"]
        backend-path = ["packaging"]
        [tool.cibuildwheel]
        environment = "PIP_CONSTRAINT=requirements/base.txt"
        [tool.cibuildwheel.linux]
        environment = "PIP_CONSTRAINT=requirements/linux.txt"
      requirements/base.txt: |
        cython==3.0.0
      requirements/linux.txt: |
        cython==3.2.8 \
          --hash=sha256:abcdef
`,
			pythonTag: "cp312",
			want:      []string{"cython==3.2.8"},
		},
		{
			name: "BackendPathWithoutConstraintFilesIgnored",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        requires = ["setuptools>=68"]
        backend-path = ["_build_hook"]
        build-backend = "backend"
`,
			pythonTag: "cp312",
			want:      nil,
		},
		{
			name: "PDMRunSetuptoolsOnCPython312",
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
			pythonTag: "cp312",
			want:      []string{"setuptools"},
		},
		{
			name: "PDMRunSetuptoolsOnCPython311Skipped",
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
			pythonTag: "cp311",
			want:      nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepoFromYAML(tc.repoYAML, nil))
			commit := must(repo.CommitObject(repo.Commits["initial-commit"]))
			tree := must(commit.Tree())
			got, err := ExtractDynamicBuildRequirements(context.Background(), tree, tc.searchDir, tc.pythonTag)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ExtractDynamicBuildRequirements() error = %v, wantErr %v", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ExtractDynamicBuildRequirements() diff (-want +got):\n%s", diff)
			}
		})
	}
}
