// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package parsing

import (
	"context"
	"log"
	"path/filepath"
	re "regexp"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pelletier/go-toml/v2"
	"github.com/pkg/errors"
)

// ProjectMetadata represents the [project] or [tool.poetry] table in pyproject.toml.
type ProjectMetadata struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
}

// BuildSystem represents the [build-system] table in pyproject.toml.
type BuildSystem struct {
	Requires     []string `toml:"requires"`
	BuildBackend string   `toml:"build-backend"`
	BackendPath  any      `toml:"backend-path"`
}

// CibuildwheelHooks represents build hooks in [tool.cibuildwheel] or platform subtables.
type CibuildwheelHooks struct {
	BeforeAll   any `toml:"before-all"`
	BeforeBuild any `toml:"before-build"`
	Environment any `toml:"environment"`
}

// CibuildwheelOverride represents an entry in [[tool.cibuildwheel.overrides]].
type CibuildwheelOverride struct {
	Select      any `toml:"select"`
	BeforeAll   any `toml:"before-all"`
	BeforeBuild any `toml:"before-build"`
	Environment any `toml:"environment"`
}

// CibuildwheelConfig represents the [tool.cibuildwheel] table in pyproject.toml.
type CibuildwheelConfig struct {
	BeforeAll   any                    `toml:"before-all"`
	BeforeBuild any                    `toml:"before-build"`
	Environment any                    `toml:"environment"`
	Linux       CibuildwheelHooks      `toml:"linux"`
	Overrides   []CibuildwheelOverride `toml:"overrides"`
}

// HatchTargetConfig represents a target entry under [tool.hatch.build.targets.<name>].
type HatchTargetConfig struct {
	Hooks map[string]any `toml:"hooks"`
}

// HatchBuildConfig represents the [tool.hatch.build] table in pyproject.toml.
type HatchBuildConfig struct {
	Hooks   map[string]any               `toml:"hooks"`
	Targets map[string]HatchTargetConfig `toml:"targets"`
}

// HatchConfig represents the [tool.hatch] table in pyproject.toml.
type HatchConfig struct {
	Build HatchBuildConfig `toml:"build"`
}

// PDMBuildConfig represents the [tool.pdm.build] table in pyproject.toml.
type PDMBuildConfig struct {
	RunSetuptools bool `toml:"run-setuptools"`
}

// PDMConfig represents the [tool.pdm] table in pyproject.toml.
type PDMConfig struct {
	Build PDMBuildConfig `toml:"build"`
}

// MaturinConfig represents the [tool.maturin] table in pyproject.toml.
type MaturinConfig struct {
	ManifestPath string   `toml:"manifest-path"`
	Features     []string `toml:"features"`
}

// SetuptoolsRustExtModule represents an entry in [[tool.setuptools-rust.ext-modules]].
type SetuptoolsRustExtModule struct {
	Path string `toml:"path"`
}

// SetuptoolsRustConfig represents the [tool.setuptools-rust] table in pyproject.toml.
type SetuptoolsRustConfig struct {
	ExtModules []SetuptoolsRustExtModule `toml:"ext-modules"`
}

// ToolConfig represents the [tool] table in pyproject.toml.
type ToolConfig struct {
	Poetry         ProjectMetadata      `toml:"poetry"`
	Cibuildwheel   CibuildwheelConfig   `toml:"cibuildwheel"`
	Hatch          HatchConfig          `toml:"hatch"`
	PDM            PDMConfig            `toml:"pdm"`
	Maturin        MaturinConfig        `toml:"maturin"`
	SetuptoolsRust SetuptoolsRustConfig `toml:"setuptools-rust"`
}

// PyProject represents the structure of a pyproject.toml file.
type PyProject struct {
	Project          ProjectMetadata `toml:"project"`
	BuildSystem      BuildSystem     `toml:"build-system"`
	DependencyGroups map[string]any  `toml:"dependency-groups"`
	Tool             ToolConfig      `toml:"tool"`
}

// ParsePyProject unmarshals pyproject.toml content into a PyProject struct.
func ParsePyProject(contents string) (PyProject, error) {
	var pyProject PyProject
	if err := toml.Unmarshal([]byte(contents), &pyProject); err != nil {
		return pyProject, errors.Wrap(err, "decoding pyproject.toml")
	}
	return pyProject, nil
}

// ReadPyProject reads and unmarshals a pyproject.toml git object file.
func ReadPyProject(f *object.File) (PyProject, error) {
	contents, err := f.Contents()
	if err != nil {
		return PyProject{}, errors.Wrap(err, "reading pyproject.toml")
	}
	return ParsePyProject(contents)
}

func verifyPyProjectFile(ctx context.Context, f *object.File, name, version string) (fileVerification, error) {
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
		editDist := minEditDistance(normalizeName(name), normalizeName(foundName))
		verificationResult.levDistance = editDist

		if editDist == 0 {
			verificationResult.nameMatch = true
		}

		verificationResult.foundVersion = foundVersion
		if foundVersion != "" && version == foundVersion {
			verificationResult.versionMatch = true
		}
	}

	return verificationResult, nil
}

func extractPyProjectRequirements(ctx context.Context, f *object.File) ([]string, error) {
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
	if hasHatchCustomHook(pyProject) {
		reqs = append(reqs, resolveDependencyGroup(pyProject.DependencyGroups, "build", make(map[string]bool))...)
	}
	if hasRequirementName(reqs, "scikit-build-core") && !hasRequirementName(reqs, "ninja") {
		reqs = append(reqs, "ninja")
	}
	log.Println("Added these reqs from pyproject.toml: " + strings.Join(reqs, ", "))
	return reqs, nil
}

// ExtractDynamicBuildRequirements infers PEP 517 build requirements requested
// dynamically at build time from pyproject.toml and referenced repo files.
func ExtractDynamicBuildRequirements(ctx context.Context, tree *object.Tree, searchDir, pythonTag string) ([]string, error) {
	if tree == nil {
		return nil, nil
	}
	if searchDir == "" {
		searchDir = "."
	}
	pyprojPath := filepath.Clean(filepath.Join(searchDir, "pyproject.toml"))
	f, err := tree.File(pyprojPath)
	if err == object.ErrFileNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrapf(err, "finding %s", pyprojPath)
	}
	pyProject, err := ReadPyProject(f)
	if err != nil {
		return nil, errors.Wrapf(err, "reading %s", pyprojPath)
	}
	var reqs []string
	backendPaths := stringSliceFromAny(pyProject.BuildSystem.BackendPath)
	if len(backendPaths) > 0 {
		reqs = append(reqs, extractCibuildwheelConstraints(tree, searchDir, pyProject.Tool.Cibuildwheel)...)
	}
	if hasHatchCustomHook(pyProject) {
		reqs = append(reqs, resolveDependencyGroup(pyProject.DependencyGroups, "build", make(map[string]bool))...)
	}
	if pyProject.Tool.PDM.Build.RunSetuptools && (pyProject.BuildSystem.BuildBackend == "pdm.backend" || hasRequirementName(pyProject.BuildSystem.Requires, "pdm-backend")) && isCPython312OrLater(pythonTag) {
		reqs = append(reqs, "setuptools")
	}
	if hasRequirementName(pyProject.BuildSystem.Requires, "scikit-build-core") {
		reqs = append(reqs, "ninja")
	}
	return reqs, nil
}

func requirementPkgName(req string) string {
	fields := strings.FieldsFunc(req, func(r rune) bool { return strings.ContainsRune("=<>~!;[(@ \t", r) })
	if len(fields) == 0 {
		return ""
	}
	return normalizeName(fields[0])
}

func hasRequirementName(reqs []string, names ...string) bool {
	return slices.ContainsFunc(reqs, func(r string) bool { return slices.Contains(names, requirementPkgName(r)) })
}

func hasHatchCustomHook(p PyProject) bool {
	if _, ok := p.Tool.Hatch.Build.Hooks["custom"]; ok {
		return true
	}
	if wheelTarget, ok := p.Tool.Hatch.Build.Targets["wheel"]; ok {
		if _, ok := wheelTarget.Hooks["custom"]; ok {
			return true
		}
	}
	return false
}

func resolveDependencyGroup(groups map[string]any, name string, visited map[string]bool) []string {
	if visited[name] {
		return nil
	}
	visited[name] = true
	rawGroup, ok := groups[name]
	if !ok {
		return nil
	}
	var reqs []string
	switch items := rawGroup.(type) {
	case []any:
		for _, item := range items {
			switch v := item.(type) {
			case string:
				if s := strings.TrimSpace(v); s != "" {
					reqs = append(reqs, s)
				}
			case map[string]any:
				if inc, ok := v["include-group"].(string); ok && inc != "" {
					reqs = append(reqs, resolveDependencyGroup(groups, inc, visited)...)
				}
			}
		}
	case []string:
		for _, item := range items {
			if s := strings.TrimSpace(item); s != "" {
				reqs = append(reqs, s)
			}
		}
	}
	return reqs
}

// isCPython312OrLater reports whether pythonTag targets CPython 3.12 or newer,
// where standard library venvs no longer seed setuptools via ensurepip.
func isCPython312OrLater(pythonTag string) bool {
	for _, tag := range strings.Split(pythonTag, ".") {
		rest, ok := strings.CutPrefix(tag, "cp3")
		if !ok || rest == "" {
			continue
		}
		minor := 0
		hasDigits := false
		for i := 0; i < len(rest) && rest[i] >= '0' && rest[i] <= '9'; i++ {
			hasDigits = true
			minor = minor*10 + int(rest[i]-'0')
		}
		if hasDigits && minor >= 12 {
			return true
		}
	}
	return false
}

func stringSliceFromAny(v any) []string {
	switch val := v.(type) {
	case string:
		if s := strings.TrimSpace(val); s != "" {
			return []string{s}
		}
	case []any:
		var out []string
		for _, item := range val {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		var out []string
		for _, item := range val {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

var constraintEnvVars = []string{
	"PIP_BUILD_CONSTRAINT",
	"PIP_CONSTRAINT",
	"UV_BUILD_CONSTRAINT",
	"UV_CONSTRAINT",
}

func extractCibuildwheelConstraints(tree *object.Tree, searchDir string, cfg CibuildwheelConfig) []string {
	paths := extractCibuildwheelConstraintPaths(cfg)
	var reqs []string
	for _, relPath := range paths {
		fullPath := filepath.Clean(filepath.Join(searchDir, relPath))
		if fullPath == ".." || strings.HasPrefix(fullPath, "../") {
			continue
		}
		f, err := tree.File(fullPath)
		if err != nil {
			continue
		}
		contents, err := f.Contents()
		if err != nil {
			continue
		}
		reqs = append(reqs, parseConstraintsFile(contents)...)
	}
	return reqs
}

func extractCibuildwheelConstraintPaths(cfg CibuildwheelConfig) []string {
	env := parseCibuildwheelEnv(cfg.Environment)
	for k, v := range parseCibuildwheelEnv(cfg.Linux.Environment) {
		env[k] = v
	}
	var paths []string
	seen := make(map[string]bool)
	for _, key := range constraintEnvVars {
		val, ok := env[key]
		if !ok {
			continue
		}
		for _, token := range strings.Fields(val) {
			if strings.Contains(token, "$") || strings.Contains(token, "://") || strings.HasPrefix(token, "/") {
				continue
			}
			clean := filepath.Clean(token)
			if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || seen[clean] {
				continue
			}
			seen[clean] = true
			paths = append(paths, clean)
		}
	}
	return paths
}

func parseCibuildwheelEnv(raw any) map[string]string {
	env := make(map[string]string)
	switch v := raw.(type) {
	case map[string]any:
		for key, val := range v {
			if s, ok := val.(string); ok {
				env[key] = s
			}
		}
	case map[string]string:
		for key, val := range v {
			env[key] = val
		}
	case string:
		for _, field := range strings.Fields(v) {
			if k, val, ok := strings.Cut(field, "="); ok {
				env[k] = strings.Trim(val, `"'`)
			}
		}
	}
	return env
}

var hashOptionPat = re.MustCompile(`\s*--hash=\S+`)

func parseConstraintsFile(contents string) []string {
	normalized := strings.ReplaceAll(contents, "\r\n", "\n")
	rawLines := strings.Split(normalized, "\n")
	var joined []string
	var buf strings.Builder
	for _, line := range rawLines {
		trimmedRight := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmedRight, `\`) {
			buf.WriteString(strings.TrimSuffix(trimmedRight, `\`))
			buf.WriteByte(' ')
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString(line)
			joined = append(joined, buf.String())
			buf.Reset()
			continue
		}
		joined = append(joined, line)
	}
	if buf.Len() > 0 {
		joined = append(joined, buf.String())
	}
	var reqs []string
	for _, line := range joined {
		line = stripInlineComment(line)
		line = hashOptionPat.ReplaceAllString(line, "")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		reqs = append(reqs, line)
	}
	return reqs
}

func stripInlineComment(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return ""
	}
	for i := 1; i < len(line); i++ {
		if line[i] == '#' && (line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i]
		}
	}
	return line
}
