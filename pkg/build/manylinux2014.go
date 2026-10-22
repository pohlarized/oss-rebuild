// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"context"
	_ "embed"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

//go:embed assets/manylinux2014.repo
var manylinux2014RepoConfig string

// ArtifactRegistryTokenEnvVar names the variable through which the access
// token for the private Artifact Registry yum mirror reaches the docker CLI
// and the build container.
const ArtifactRegistryTokenEnvVar = "ARTIFACT_REGISTRY_TOKEN"

// ArtifactRegistryTokenSecretID is the docker build secret that carries the
// token into Dockerfile RUN steps.
const ArtifactRegistryTokenSecretID = "artifact_registry_token"

// ArtifactRegistryTokenSource returns an access token for the private
// Artifact Registry yum mirror that is valid for the build about to start.
type ArtifactRegistryTokenSource func(ctx context.Context) (string, error)

// artifactRegistryTokenRefreshMargin bounds how close to expiry a token may
// be when it is handed to a build.
//
// NOTE: The token is passed into the container once, at build start, and
// must stay valid for every yum invocation of that build. Access tokens live
// for one hour. Refreshing whenever less than this margin remains guarantees
// each build at least this much validity, which covers the setup and deps
// phases where yum runs.
const artifactRegistryTokenRefreshMargin = 20 * time.Minute

// cloudPlatformScope grants Artifact Registry read access.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// ADCArtifactRegistryTokenSource yields tokens from Application Default
// Credentials. The credentials are loaded on first use so that executors
// which never build on manylinux2014 need none.
func ADCArtifactRegistryTokenSource() ArtifactRegistryTokenSource {
	var once sync.Once
	var source oauth2.TokenSource
	var initErr error
	return func(ctx context.Context) (string, error) {
		once.Do(func() {
			// NOTE: The token source keeps the context for the lifetime of the
			// refresh client, so it must not be tied to any single build.
			base, err := google.DefaultTokenSource(context.Background(), cloudPlatformScope)
			if err != nil {
				initErr = errors.Wrap(err, "loading application default credentials")
				return
			}
			source = oauth2.ReuseTokenSourceWithExpiry(nil, base, artifactRegistryTokenRefreshMargin)
		})
		if initErr != nil {
			return "", initErr
		}
		token, err := source.Token()
		if err != nil {
			return "", errors.Wrap(err, "fetching artifact registry token")
		}
		return token.AccessToken, nil
	}
}

// IsManylinux2014 returns true if the given base image is a manylinux2014 image.
func IsManylinux2014(baseImage string) bool {
	return strings.Contains(baseImage, "manylinux2014")
}

// Manylinux2014RepoSetupScript returns the shell script snippet to configure yum repositories
// on manylinux2014 images before performing package management operations.
//
// The token arrives either as a docker build secret or as an environment
// variable. Both paths copy it without expanding its value on the command
// line, so that the build log produced under `set -x` never contains it. A
// missing or empty token aborts the build here rather than surfacing later
// as an opaque yum 401.
func Manylinux2014RepoSetupScript() string {
	var sb strings.Builder
	sb.WriteString("rm -f /etc/yum.repos.d/*\n")
	sb.WriteString("mkdir -p /etc/yum/vars\n")
	sb.WriteString("if [ -f /run/secrets/" + ArtifactRegistryTokenSecretID + " ]; then\n")
	sb.WriteString("  cp /run/secrets/" + ArtifactRegistryTokenSecretID + " /etc/yum/vars/yum_token\n")
	sb.WriteString("else\n")
	sb.WriteString("  printenv " + ArtifactRegistryTokenEnvVar + " > /etc/yum/vars/yum_token\n")
	sb.WriteString("fi\n")
	sb.WriteString("test -s /etc/yum/vars/yum_token\n")
	sb.WriteString("cat <<'EOREPO' > /etc/yum.repos.d/oss_rebuild.repo\n")
	sb.WriteString(strings.TrimSpace(manylinux2014RepoConfig))
	sb.WriteString("\nEOREPO")
	return sb.String()
}
