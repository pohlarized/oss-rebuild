// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"strings"
	"testing"
)

func TestManylinux2014RepoSetupScript(t *testing.T) {
	script := Manylinux2014RepoSetupScript()
	if !strings.Contains(script, "rm -f /etc/yum.repos.d/*") {
		t.Errorf("script missing repo cleanup command")
	}
	if !strings.Contains(script, "/etc/yum/vars/yum_token") {
		t.Errorf("script missing /etc/yum/vars/yum_token write")
	}
	if !strings.Contains(script, "/etc/yum.repos.d/oss_rebuild.repo") {
		t.Errorf("script missing destination repo write")
	}
	if !strings.Contains(script, "$yum_token") {
		t.Errorf("script missing $yum_token placeholder in repo config")
	}
	if !strings.Contains(script, "ARTIFACT_REGISTRY_TOKEN") {
		t.Errorf("script missing ARTIFACT_REGISTRY_TOKEN reference")
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
