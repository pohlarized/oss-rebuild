// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package build

import (
	_ "embed"
	"strings"
)

//go:embed assets/manylinux2014.repo
var manylinux2014RepoConfig string

// ArtifactRegistryTokenEnvVar is the environment variable name used for artifact registry auth tokens.
const ArtifactRegistryTokenEnvVar = "ARTIFACT_REGISTRY_TOKEN"

// IsManylinux2014 returns true if the given base image is a manylinux2014 image.
func IsManylinux2014(baseImage string) bool {
	return strings.Contains(baseImage, "manylinux2014")
}

// Manylinux2014RepoSetupScript returns the shell script snippet to configure yum repositories
// on manylinux2014 images before performing package management operations.
func Manylinux2014RepoSetupScript() string {
	var sb strings.Builder
	sb.WriteString("rm -f /etc/yum.repos.d/*\n")
	sb.WriteString("if [ -f /run/secrets/artifact_registry_token ]; then\n")
	sb.WriteString("  ARTIFACT_REGISTRY_TOKEN=$(cat /run/secrets/artifact_registry_token)\n")
	sb.WriteString("fi\n")
	sb.WriteString("if [ -n \"${ARTIFACT_REGISTRY_TOKEN:-}\" ]; then\n")
	sb.WriteString("  mkdir -p /etc/yum/vars\n")
	sb.WriteString("  printf '%s' \"$ARTIFACT_REGISTRY_TOKEN\" > /etc/yum/vars/yum_token\n")
	sb.WriteString("fi\n")
	sb.WriteString("cat <<'EOREPO' > /etc/yum.repos.d/oss_rebuild.repo\n")
	sb.WriteString(strings.TrimSpace(manylinux2014RepoConfig))
	sb.WriteString("\nEOREPO")
	return sb.String()
}
