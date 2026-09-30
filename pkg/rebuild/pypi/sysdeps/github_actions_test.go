// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package sysdeps

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/oss-rebuild/internal/gitx/gitxtest"
)

func TestExtractGitHubActionsDependencies(t *testing.T) {
	tests := []struct {
		name     string
		repoYAML string
		want     []DependencyIdentifier
	}{
		{
			name: "PygraphvizReleaseWorkflow",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      .github/workflows/release.yml: |
        name: Build Wheels and Release
        env:
          GRAPHVIZ_VERSION: "14.1.5"
        jobs:
          build_wheels:
            runs-on: ${{ matrix.os }}
            steps:
              - uses: actions/checkout@v4
              - name: Build wheels
                run: python -m cibuildwheel --output-dir wheelhouse
                env:
                  CIBW_MANYLINUX_X86_64_IMAGE: manylinux_2_28
                  CIBW_BEFORE_ALL_LINUX: >
                    dnf install -y gcc gcc-c++ bison flex expat-devel zlib-devel autoconf automake libtool gd-devel cairo-devel pango-devel &&
                    curl -L "https://gitlab.com/api/v4/projects/4207231/packages/generic/graphviz-releases/${{ env.GRAPHVIZ_VERSION }}/graphviz-${{ env.GRAPHVIZ_VERSION }}.tar.gz" -o /tmp/graphviz.tar.gz &&
                    tar xzf /tmp/graphviz.tar.gz -C /tmp
                  CIBW_BEFORE_ALL_MACOS: |
                    yum install -y should-not-be-extracted
`,
			want: []DependencyIdentifier{
				{Namespace: NamespaceDnf, Name: "gcc", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "gcc-c++", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "bison", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "flex", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "expat-devel", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "zlib-devel", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "autoconf", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "automake", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "libtool", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "gd-devel", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "cairo-devel", Provenance: ".github/workflows/release.yml"},
				{Namespace: NamespaceDnf, Name: "pango-devel", Provenance: ".github/workflows/release.yml"},
			},
		},
		{
			name: "WorkflowJobAndStepEnvScopes",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      .github/workflows/wheels.yaml: |
        name: Wheels
        env:
          CIBW_BEFORE_ALL: "yum install -y libffi-devel"
          CIBW_BEFORE_ALL_WINDOWS: "dnf install -y ignored-win"
        jobs:
          linux:
            runs-on: ubuntu-latest
            env:
              CIBW_BEFORE_ALL_LINUX: "dnf install -y openssl-devel"
              CIBW_BEFORE_BUILD_MACOS: "apk add ignored-mac"
            steps:
              - uses: pypa/cibuildwheel@v2.22.0
                env:
                  CIBW_BEFORE_BUILD: "apk add --no-cache musl-dev"
                  CIBW_BEFORE_BUILD_LINUX: "apt-get install -y libpq-dev"
`,
			want: []DependencyIdentifier{
				{Namespace: NamespaceYum, Name: "libffi-devel", Provenance: ".github/workflows/wheels.yaml"},
				{Namespace: NamespaceDnf, Name: "openssl-devel", Provenance: ".github/workflows/wheels.yaml"},
				{Namespace: NamespaceApk, Name: "musl-dev", Provenance: ".github/workflows/wheels.yaml"},
				{Namespace: NamespaceApt, Name: "libpq-dev", Provenance: ".github/workflows/wheels.yaml"},
			},
		},
		{
			name: "IgnoresNonCibwRunSteps",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      .github/workflows/test.yml: |
        name: Test
        jobs:
          ubuntu:
            runs-on: ubuntu-latest
            steps:
              - name: Install graphviz
                run: sudo apt-get install graphviz graphviz-dev
`,
			want: nil,
		},
		{
			name: "MalformedWorkflowSkippedFailOpen",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      .github/workflows/broken.yml: |
        name: [invalid: yaml: {{{
      .github/workflows/valid.yml: |
        name: Valid
        env:
          CIBW_BEFORE_ALL_LINUX: "yum install -y zlib-devel"
      .github/workflows/README.md: |
        yum install -y ignored-readme
`,
			want: []DependencyIdentifier{
				{Namespace: NamespaceYum, Name: "zlib-devel", Provenance: ".github/workflows/valid.yml"},
			},
		},
		{
			name: "NoWorkflowsDirectory",
			repoYAML: `
commits:
  - id: initial-commit
    files:
      pyproject.toml: |
        [project]
        name = "example"
`,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepoFromYAML(tc.repoYAML, nil))
			commit := must(repo.CommitObject(repo.Commits["initial-commit"]))
			tree := must(commit.Tree())
			got, err := ExtractGitHubActionsDependencies(context.Background(), tree)
			if err != nil {
				t.Fatalf("ExtractGitHubActionsDependencies() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("ExtractGitHubActionsDependencies() diff (-want +got):\n%s", diff)
			}
		})
	}
}
