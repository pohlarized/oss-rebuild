// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package parsing

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/oss-rebuild/internal/gitx/gitxtest"
	"github.com/msuozzo/go-git2"
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
			commit := must(repo.Repository.CommitObject(git2.Hash(repo.Commits["initial-commit"])))
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

func TestInTreeBackendSourcePaths(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dir      string
		repoYAML string
		want     []string
	}{
		{
			name: "BackendPackageSubmodules",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        build-backend = "pep517_backend.hooks"
        backend-path = ["packaging"]
      packaging/pep517_backend/__init__.py: ""
      packaging/pep517_backend/hooks.py: |
        from ._backend import build_wheel
      packaging/pep517_backend/_backend.py: |
        from Cython.Build.Cythonize import main
      frozenlist/_frozenlist.pyx: ""
`,
			want: []string{
				"packaging/pep517_backend/__init__.py",
				"packaging/pep517_backend/_backend.py",
				"packaging/pep517_backend/hooks.py",
			},
		},
		{
			name: "BackendDirSiblingHelperModules",
			dir:  "psycopg_c",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      psycopg_c/pyproject.toml: |
        [build-system]
        build-backend = "cython_backend"
        backend-path = ["build_backend"]
      psycopg_c/build_backend/cython_backend.py: |
        from psycopg_build_ext import build_ext
      psycopg_c/build_backend/psycopg_build_ext.py: |
        from Cython.Build import cythonize
`,
			want: []string{
				"psycopg_c/build_backend/cython_backend.py",
				"psycopg_c/build_backend/psycopg_build_ext.py",
			},
		},
		{
			name: "RootBackendPathScopesToBackendPackage",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        build-backend = "pep517_backend.hooks"
        backend-path = ["."]
      pep517_backend/hooks.py: ""
      pep517_backend/_helper.py: ""
      unrelated.py: |
        import cython
      tests/test_cython.py: |
        import Cython
`,
			want: []string{
				"pep517_backend/_helper.py",
				"pep517_backend/hooks.py",
			},
		},
		{
			name: "RootBackendPathSingleModuleWithObjectSuffix",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        build-backend = "custom_backend:build_wheel"
        backend-path = ["."]
      custom_backend.py: ""
      unrelated.py: ""
`,
			want: []string{"custom_backend.py"},
		},
		{
			name: "EscapingAbsoluteAndDuplicateBackendPaths",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        build-backend = "backend"
        backend-path = ["../shared", "/abs", "sub/../hooks", "hooks"]
      shared/backend.py: ""
      hooks/backend.py: ""
`,
			want: []string{"hooks/backend.py"},
		},
		{
			name: "NoBackendPath",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        build-backend = "setuptools.build_meta"
`,
			want: nil,
		},
		{
			name: "MissingPyproject",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      README.md: ""
`,
			want: nil,
		},
		{
			name: "InvalidTOMLSkipped",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [build-system]
        build-backend = "backend
`,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepoFromYAML(tc.repoYAML, nil))
			commit := must(repo.Repository.CommitObject(git2.Hash(repo.Commits["initial-commit"])))
			tree := must(commit.Tree())
			got, err := InTreeBackendSourcePaths(tree, tc.dir)
			if err != nil {
				t.Fatalf("InTreeBackendSourcePaths() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("InTreeBackendSourcePaths() diff (-want +got):\n%s", diff)
			}
		})
	}
}
