// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"strings"
	"testing"
)

func TestCentOSRepoSetupScript(t *testing.T) {
	script := CentOSRepoSetupScript()
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
