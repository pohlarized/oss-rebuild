// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package sysdeps

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

// cibwLinuxEnvVars lists the cibuildwheel hook environment variables that apply to Linux builds.
// Platform-specific hooks for non-Linux targets (such as CIBW_BEFORE_ALL_MACOS and
// CIBW_BEFORE_ALL_WINDOWS) are intentionally excluded.
// https://cibuildwheel.pypa.io/en/stable/options/#before-all
var cibwLinuxEnvVars = []string{
	"CIBW_BEFORE_ALL",
	"CIBW_BEFORE_ALL_LINUX",
	"CIBW_BEFORE_BUILD",
	"CIBW_BEFORE_BUILD_LINUX",
}

type ghaWorkflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]ghaJob `yaml:"jobs"`
}

type ghaJob struct {
	Env   map[string]string `yaml:"env"`
	Steps []ghaStep         `yaml:"steps"`
}

type ghaStep struct {
	Env map[string]string `yaml:"env"`
}

// ExtractGitHubActionsDependencies extracts system dependencies from cibuildwheel environment
// variables configured in .github/workflows/*.yml and *.yaml files.
func ExtractGitHubActionsDependencies(ctx context.Context, tree *object.Tree) ([]DependencyIdentifier, error) {
	if tree == nil {
		return nil, nil
	}
	workflowTree, err := tree.Tree(".github/workflows")
	if err == object.ErrDirectoryNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, "finding .github/workflows directory")
	}
	var ids []DependencyIdentifier
	for _, entry := range workflowTree.Entries {
		if !entry.Mode.IsFile() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name))
		if ext != ".yml" && ext != ".yaml" {
			continue
		}
		f, err := workflowTree.File(entry.Name)
		if err != nil {
			return nil, errors.Wrapf(err, "getting workflow file %s", entry.Name)
		}
		contents, err := f.Contents()
		if err != nil {
			return nil, errors.Wrapf(err, "reading workflow file %s", entry.Name)
		}
		var wf ghaWorkflow
		if err := yaml.Unmarshal([]byte(contents), &wf); err != nil {
			continue
		}
		provenance := filepath.Join(".github/workflows", entry.Name)
		ids = append(ids, extractWorkflowCibwEnv(wf, provenance)...)
	}
	return DeduplicateIdentifiers(ids), nil
}

func extractWorkflowCibwEnv(wf ghaWorkflow, provenance string) []DependencyIdentifier {
	var scripts []string
	scripts = append(scripts, extractCibwEnvMap(wf.Env)...)
	for _, jobName := range slices.Sorted(maps.Keys(wf.Jobs)) {
		job := wf.Jobs[jobName]
		scripts = append(scripts, extractCibwEnvMap(job.Env)...)
		for _, step := range job.Steps {
			scripts = append(scripts, extractCibwEnvMap(step.Env)...)
		}
	}
	var ids []DependencyIdentifier
	for _, script := range scripts {
		ids = append(ids, ParsePackageManagerCommands(script, provenance)...)
	}
	return ids
}

func extractCibwEnvMap(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	var scripts []string
	for _, key := range cibwLinuxEnvVars {
		if val := strings.TrimSpace(env[key]); val != "" {
			scripts = append(scripts, val)
		}
	}
	return scripts
}
