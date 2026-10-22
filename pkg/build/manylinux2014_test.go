// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"strings"
	"testing"
)

func TestManylinux2014RepoSetupScript(t *testing.T) {
	script := Manylinux2014RepoSetupScript()
	for _, want := range []string{
		"rm -f /etc/yum.repos.d/*",
		"cp /run/secrets/artifact_registry_token /etc/yum/vars/yum_token",
		"printenv ARTIFACT_REGISTRY_TOKEN > /etc/yum/vars/yum_token",
		"test -s /etc/yum/vars/yum_token",
		"/etc/yum.repos.d/oss_rebuild.repo",
		"$yum_token",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// The build log is captured under `set -x`. Expanding the token on a
	// command line would print it there.
	if strings.Contains(script, "$ARTIFACT_REGISTRY_TOKEN") || strings.Contains(script, "${ARTIFACT_REGISTRY_TOKEN") {
		t.Errorf("script expands the token value on a command line:\n%s", script)
	}
}

func TestIsManylinux2014(t *testing.T) {
	tests := []struct {
		image string
		want  bool
	}{
		{"quay.io/pypa/manylinux2014_x86_64", true},
		{"quay.io/pypa/manylinux2014_aarch64", true},
		{"manylinux2014:latest", true},
		{"centos:7", false},
		{"quay.io/pypa/manylinux_2_28_x86_64", false},
		{"quay.io/pypa/manylinux_2_34_x86_64", false},
		{"alpine:latest", false},
		{"debian:stable", false},
	}
	for _, tc := range tests {
		if got := IsManylinux2014(tc.image); got != tc.want {
			t.Errorf("IsManylinux2014(%q) = %v, want %v", tc.image, got, tc.want)
		}
	}
}
