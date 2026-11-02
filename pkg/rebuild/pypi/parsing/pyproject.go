// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package parsing

import (
	"context"
	"log"
	"path"
	"path/filepath"
	"slices"
	"strings"

	pypireg "github.com/google/oss-rebuild/pkg/registry/pypi"
	"github.com/msuozzo/go-git2"
	"github.com/pelletier/go-toml/v2"
	"github.com/pkg/errors"
)

// ProjectMetadata represents the [project] or [tool.poetry] table in pyproject.toml.
type ProjectMetadata struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
}

// CibuildwheelHooks represents build hooks in [tool.cibuildwheel], platform subtables, or override entries.
type CibuildwheelHooks struct {
	BeforeAll   any `toml:"before-all"`
	BeforeBuild any `toml:"before-build"`
}

// CibuildwheelOverride represents an entry in [[tool.cibuildwheel.overrides]].
type CibuildwheelOverride struct {
	Select any `toml:"select"`
	CibuildwheelHooks
}

// CibuildwheelConfig represents the [tool.cibuildwheel] table in pyproject.toml.
type CibuildwheelConfig struct {
	CibuildwheelHooks
	Linux     CibuildwheelHooks      `toml:"linux"`
	Overrides []CibuildwheelOverride `toml:"overrides"`
}

// PyProject represents the structure of a pyproject.toml file.
type PyProject struct {
	Project     ProjectMetadata `toml:"project"`
	BuildSystem struct {
		Requires     []string `toml:"requires"`
		BuildBackend string   `toml:"build-backend"`
		BackendPath  []string `toml:"backend-path"`
	} `toml:"build-system"`
	Tool struct {
		Poetry       ProjectMetadata    `toml:"poetry"`
		Cibuildwheel CibuildwheelConfig `toml:"cibuildwheel"`
	} `toml:"tool"`
}

// ReadPyProject reads and unmarshals a pyproject.toml git object file.
func ReadPyProject(f *git2.File) (PyProject, error) {
	contents, err := f.Contents()
	if err != nil {
		return PyProject{}, errors.Wrap(err, "reading pyproject.toml")
	}
	var pyProject PyProject
	if err := toml.Unmarshal([]byte(contents), &pyProject); err != nil {
		return PyProject{}, errors.Wrap(err, "decoding pyproject.toml")
	}
	return pyProject, nil
}

func verifyPyProjectFile(ctx context.Context, f *git2.File, name, version string) (fileVerification, error) {
	var verificationResult fileVerification
	verificationResult.foundF = f
	pyProject, err := ReadPyProject(f)
	if err != nil {
		return verificationResult, err
	}
	foundName := ""
	foundVersion := ""
	if pyProject.Project.Name != "" {
		foundName = pyProject.Project.Name
		foundVersion = pyProject.Project.Version
	} else if pyProject.Tool.Poetry.Name != "" {
		foundName = pyProject.Tool.Poetry.Name
		foundVersion = pyProject.Tool.Poetry.Version
	}

	if filepath.Dir(f.Name) == "." {
		verificationResult.main = true
	}

	if foundName != "" {
		editDist := minEditDistance(pypireg.NormalizedName(name), pypireg.NormalizedName(foundName))
		verificationResult.levDistance = editDist

		if editDist == 0 {
			verificationResult.nameMatch = true
		}

		verificationResult.foundVersion = foundVersion
		if foundVersion != "" && SameVersion(foundVersion, version) {
			verificationResult.versionMatch = true
		}
	}

	return verificationResult, nil
}

func extractPyProjectRequirements(ctx context.Context, f *git2.File) ([]string, error) {
	var reqs []string
	log.Println("Looking for additional reqs in pyproject.toml")
	pyProject, err := ReadPyProject(f)
	if err != nil {
		return nil, err
	}
	for _, r := range pyProject.BuildSystem.Requires {
		// TODO: Some of these requirements are probably already in rbcfg.Requirements, should we skip
		// them? To even know which package we're looking at would require parsing the dependency spec.
		// https://packaging.python.org/en/latest/specifications/dependency-specifiers/#dependency-specifiers
		reqs = append(reqs, strings.TrimSpace(r))
	}
	log.Println("Added these reqs from pyproject.toml: " + strings.Join(reqs, ", "))
	return reqs, nil
}

// InTreeBackendSourcePaths returns existing Python source files in tree for an
// in-tree PEP 517 backend configured in dir/pyproject.toml, relative to tree root.
// Missing or unparseable pyproject.toml files return nil, nil.
// https://peps.python.org/pep-0517/#in-tree-build-backends
func InTreeBackendSourcePaths(tree *git2.Tree, dir string) ([]string, error) {
	f, err := tree.File(path.Join(dir, "pyproject.toml"))
	if errors.Is(err, git2.ErrFileNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, "finding pyproject.toml")
	}
	pyProject, err := ReadPyProject(f)
	var decodeErr *toml.DecodeError
	if errors.As(err, &decodeErr) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	mod, _, _ := strings.Cut(pyProject.BuildSystem.BuildBackend, ":")
	topMod, _, _ := strings.Cut(strings.TrimSpace(mod), ".")
	topMod = strings.TrimSpace(topMod)
	if topMod == "" || strings.Contains(topMod, "/") || len(pyProject.BuildSystem.BackendPath) == 0 {
		return nil, nil
	}
	var paths []string
	collectDir := func(subDir string) error {
		subTree, err := tree.Tree(subDir)
		if errors.Is(err, git2.ErrDirectoryNotFound) {
			return nil
		}
		if err != nil {
			return errors.Wrapf(err, "finding directory %s", subDir)
		}
		return errors.Wrapf(subTree.Files().ForEach(func(f *git2.File) error {
			if strings.HasSuffix(f.Name, ".py") {
				paths = append(paths, path.Join(subDir, f.Name))
			}
			return nil
		}), "walking %s", subDir)
	}
	for _, bp := range pyProject.BuildSystem.BackendPath {
		bp = strings.TrimSpace(bp)
		// NOTE: PEP 517 requires backend-path entries to stay within the source tree.
		cleanBP := path.Clean(bp)
		if bp == "" || path.IsAbs(bp) || cleanBP == ".." || strings.HasPrefix(cleanBP, "../") {
			continue
		}
		// NOTE: A "." entry exposes the whole project directory, including tests
		// and unrelated modules, so only the build-backend module or package is
		// scanned.
		if cleanBP != "." {
			if err := collectDir(path.Join(dir, cleanBP)); err != nil {
				return nil, err
			}
			continue
		}
		modFile := path.Join(dir, topMod+".py")
		if _, err := tree.File(modFile); err == nil {
			paths = append(paths, modFile)
		} else if !errors.Is(err, git2.ErrFileNotFound) {
			return nil, errors.Wrapf(err, "finding %s", modFile)
		}
		if err := collectDir(path.Join(dir, topMod)); err != nil {
			return nil, err
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}
