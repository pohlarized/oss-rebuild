// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"debug/elf"
	"io"
	"maps"
	"path"
	re "regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/oss-rebuild/internal/semver"
	pypiresolver "github.com/google/oss-rebuild/pkg/rebuild/pypi/parsing"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/platform"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/sysdeps"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	"github.com/google/oss-rebuild/pkg/registry/cratesio"
	pypireg "github.com/google/oss-rebuild/pkg/registry/pypi"
	"github.com/pelletier/go-toml/v2"
	"github.com/pkg/errors"
)

// MaturinBuild configures a maturin wheel build.
type MaturinBuild struct {
	Policy   string   `json:"policy,omitempty" yaml:"policy,omitempty"`
	Features []string `json:"features,omitempty" yaml:"features,omitempty"`
}

var (
	rustcCommentPat     = re.MustCompile(`rustc version (\d+\.\d+\.\d+)`)
	rustToolchainPinPat = re.MustCompile(`^\d+\.\d+(?:\.\d+)?$`)
	exactVersionPinPat  = re.MustCompile(`==\s*([^\s;,]+)`)
)

// usesRust reports whether the build requirements include a Rust build backend or extension builder.
func usesRust(reqs []string) bool {
	return hasRequirement(reqs, "maturin", "setuptools-rust")
}

func isSharedObject(name string) bool {
	base := path.Base(name)
	return strings.HasSuffix(base, ".so") || strings.Contains(base, ".so.")
}

func isWheelLibsDir(name string) bool {
	for _, seg := range strings.Split(name, "/") {
		if strings.HasSuffix(seg, ".libs") || seg == ".libs" {
			return true
		}
	}
	return false
}

// inspectWheelRustELF scans ELF .comment sections of shared objects in zr (excluding
// auditwheel-bundled libraries under *.libs/) for rustc version strings and system LLD usage.
func inspectWheelRustELF(zr *zip.Reader) (string, bool) {
	if zr == nil {
		return "", false
	}
	var highestVer string
	var needsSystemLLD bool
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !isSharedObject(f.Name) || isWheelLibsDir(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		ef, err := elf.NewFile(bytes.NewReader(data))
		if err != nil {
			continue
		}
		sec := ef.Section(".comment")
		if sec == nil {
			ef.Close()
			continue
		}
		commentData, err := sec.Data()
		ef.Close()
		if err != nil {
			continue
		}
		for _, entry := range bytes.Split(commentData, []byte{0}) {
			s := string(entry)
			if m := rustcCommentPat.FindStringSubmatch(s); m != nil {
				if highestVer == "" || semver.Cmp(highestVer, m[1]) < 0 {
					highestVer = m[1]
				}
			}
			// Rust toolchains that invoke the bundled rust-lld stamp a path containing
			// /checkout/src/llvm-project in the LLD comment. A plain "Linker: LLD <ver>"
			// entry indicates that the build linked with a system LLD binary.
			if strings.HasPrefix(s, "Linker: LLD ") && !strings.Contains(s, "/checkout/src/llvm-project") {
				needsSystemLLD = true
			}
		}
	}
	return highestVer, needsSystemLLD
}

type rustToolchainTOML struct {
	Toolchain struct {
		Channel string `toml:"channel"`
	} `toml:"toolchain"`
}

// readRustToolchainFile reads a pinned numeric Rust version from rust-toolchain.toml or
// rust-toolchain in dir or the repository root.
func readRustToolchainFile(tree *object.Tree, dir string) string {
	if tree == nil {
		return ""
	}
	var dirs []string
	if dir != "" && dir != "." {
		dirs = append(dirs, dir)
	}
	dirs = append(dirs, ".")
	for _, d := range dirs {
		for _, name := range []string{"rust-toolchain.toml", "rust-toolchain"} {
			p := name
			if d != "." {
				p = path.Join(d, name)
			}
			f, err := tree.File(p)
			if err != nil {
				continue
			}
			contents, err := f.Contents()
			if err != nil {
				continue
			}
			if strings.HasSuffix(name, ".toml") {
				var cfg rustToolchainTOML
				if err := toml.Unmarshal([]byte(contents), &cfg); err == nil {
					ch := strings.TrimSpace(cfg.Toolchain.Channel)
					if rustToolchainPinPat.MatchString(ch) {
						return ch
					}
				}
				continue
			}
			ch := strings.TrimSpace(contents)
			if rustToolchainPinPat.MatchString(ch) {
				return ch
			}
		}
	}
	return ""
}

// inferRustVersion determines the Rust toolchain version for a Rust-backed wheel build,
// preferring the ELF .comment rustc version over rust-toolchain files and registry-time lookup.
func inferRustVersion(zr *zip.Reader, tree *object.Tree, dir string, registryTime time.Time) (string, bool) {
	ver, needsSystemLLD := inspectWheelRustELF(zr)
	if ver != "" {
		return ver, needsSystemLLD
	}
	if ver = readRustToolchainFile(tree, dir); ver != "" {
		return ver, needsSystemLLD
	}
	if !registryTime.IsZero() {
		if v, err := cratesio.RustVersionAt(registryTime); err == nil {
			return v, needsSystemLLD
		}
	}
	return "", needsSystemLLD
}

// inferRustHost returns the rustup host target triple for a PEP 425 wheel platform tag.
func inferRustHost(platformTag string) (string, error) {
	tags, err := platform.ParsePlatformTags(platformTag)
	if err != nil {
		return "", errors.Wrap(err, "parsing platform tag for rust host")
	}
	highest, err := platform.HighestLibcVersionTag(tags)
	if err != nil {
		return "", errors.Wrap(err, "selecting highest platform tag for rust host")
	}
	switch highest.Arch {
	case "x86_64", "aarch64":
		if highest.LibcImpl == platform.Musl {
			return highest.Arch + "-unknown-linux-musl", nil
		}
		return highest.Arch + "-unknown-linux-gnu", nil
	default:
		return "", errors.Errorf("unsupported arch for rust host: %s", highest.Arch)
	}
}

// readPyProjectInDir reads pyproject.toml from dir (or root) in tree if present.
func readPyProjectInDir(tree *object.Tree, dir string) (pypiresolver.PyProject, bool) {
	if tree == nil {
		return pypiresolver.PyProject{}, false
	}
	p := "pyproject.toml"
	if dir != "" && dir != "." {
		p = path.Join(dir, "pyproject.toml")
	}
	f, err := tree.File(p)
	if err != nil {
		return pypiresolver.PyProject{}, false
	}
	pp, err := pypiresolver.ReadPyProject(f)
	if err != nil {
		return pypiresolver.PyProject{}, false
	}
	return pp, true
}

// hasCargoDependency checks Cargo.lock and Cargo.toml files associated with the package
// for a reference to dep.
func hasCargoDependency(tree *object.Tree, dir string, dep string) bool {
	if tree == nil {
		return false
	}
	var candidates []string
	if dir != "" && dir != "." {
		candidates = append(candidates, path.Join(dir, "Cargo.lock"), path.Join(dir, "Cargo.toml"))
	}
	candidates = append(candidates, "Cargo.lock", "Cargo.toml")
	if pp, ok := readPyProjectInDir(tree, dir); ok {
		if mp := pp.Tool.Maturin.ManifestPath; mp != "" {
			if dir != "" && dir != "." {
				candidates = append(candidates, path.Join(dir, mp))
			} else {
				candidates = append(candidates, mp)
			}
		}
		for _, ext := range pp.Tool.SetuptoolsRust.ExtModules {
			if ext.Path != "" {
				if dir != "" && dir != "." {
					candidates = append(candidates, path.Join(dir, ext.Path))
				} else {
					candidates = append(candidates, ext.Path)
				}
			}
		}
	}
	seen := make(map[string]bool, len(candidates))
	for _, p := range candidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		f, err := tree.File(p)
		if err != nil {
			continue
		}
		contents, err := f.Contents()
		if err != nil {
			continue
		}
		if strings.Contains(contents, dep) {
			return true
		}
	}
	return false
}

// inferRustSysdeps returns additional system dependencies required by Rust wheel builds.
func inferRustSysdeps(platformTag string, needsSystemLLD bool, tree *object.Tree, dir string) []sysdeps.DependencyIdentifier {
	var deps []sysdeps.DependencyIdentifier
	if needsSystemLLD {
		deps = append(deps, sysdeps.DependencyIdentifier{
			Namespace:  sysdeps.NamespaceApk,
			Name:       "lld",
			Provenance: "wheel:comment:lld",
		})
	}
	if tags, err := platform.ParsePlatformTags(platformTag); err == nil {
		if highest, err := platform.HighestLibcVersionTag(tags); err == nil && highest.LibcImpl == platform.Musl {
			// libafl_bolts links against -lucontext, which glibc provides in libc but
			// musl requires from the external libucontext-dev package on Alpine.
			if hasCargoDependency(tree, dir, "libafl_bolts") {
				deps = append(deps, sysdeps.DependencyIdentifier{
					Namespace:  sysdeps.NamespaceApk,
					Name:       "libucontext-dev",
					Provenance: "cargo:libafl_bolts",
				})
			}
		}
	}
	return deps
}

// inferMaturinBuild constructs the MaturinBuild configuration for a maturin wheel.
func inferMaturinBuild(tree *object.Tree, dir string, tags WheelTags) *MaturinBuild {
	policy := platform.HighestPolicy(tags.Platform)
	pp, _ := readPyProjectInDir(tree, dir)
	features := slices.Clone(pp.Tool.Maturin.Features)
	manifestPath := pp.Tool.Maturin.ManifestPath
	if manifestPath == "" {
		manifestPath = "Cargo.toml"
	}
	if dir != "" && dir != "." {
		manifestPath = path.Join(dir, manifestPath)
	}
	if extraFeature := inferMaturinABI3Feature(tree, manifestPath, tags, features); extraFeature != "" && !slices.Contains(features, extraFeature) {
		features = append(features, extraFeature)
	}
	return &MaturinBuild{
		Policy:   policy,
		Features: features,
	}
}

// inferMaturinABI3Feature determines the --features flag entry needed for an abi3 or abi3t
// maturin wheel when Cargo.toml does not enable the corresponding pyo3 feature by default.
func inferMaturinABI3Feature(tree *object.Tree, manifestPath string, tags WheelTags, existingFeatures []string) string {
	if !strings.HasPrefix(tags.Python, "cp3") {
		return ""
	}
	pyVer := "py" + strings.TrimPrefix(tags.Python, "cp")
	abiParts := strings.Split(tags.ABI, ".")
	var wantedPyo3Feature, preferredCrateFeature string
	if slices.Contains(abiParts, "abi3t") {
		wantedPyo3Feature = "abi3t-" + pyVer
		preferredCrateFeature = "abi3t"
	} else if slices.Contains(abiParts, "abi3") {
		wantedPyo3Feature = "abi3-" + pyVer
		preferredCrateFeature = "abi3"
	} else {
		return ""
	}
	pyo3Target := "pyo3/" + wantedPyo3Feature
	if tree == nil {
		return pyo3Target
	}
	f, err := tree.File(manifestPath)
	if err != nil {
		return pyo3Target
	}
	contents, err := f.Contents()
	if err != nil {
		return pyo3Target
	}
	var cargo cratesio.CargoTOML
	if err := toml.Unmarshal([]byte(contents), &cargo); err != nil {
		return pyo3Target
	}
	if slices.Contains(cargo.DependencyFeatures("pyo3"), wantedPyo3Feature) {
		return ""
	}
	for _, feat := range append(slices.Clone(cargo.Features["default"]), existingFeatures...) {
		if feat == wantedPyo3Feature || feat == pyo3Target || slices.Contains(cargo.Features[feat], pyo3Target) {
			return ""
		}
	}
	if slices.Contains(cargo.Features[preferredCrateFeature], pyo3Target) {
		return preferredCrateFeature
	}
	for _, k := range slices.Sorted(maps.Keys(cargo.Features)) {
		if slices.Contains(cargo.Features[k], pyo3Target) {
			return k
		}
	}
	return pyo3Target
}

// advanceCoReleaseRegistryTime advances registryTime when build requirements pin a sibling
// package to the exact version being built (such as angr==9.3.4 requiring pyvex==9.3.4) and
// artifacts in the lockstep release were uploaded milliseconds after the target wheel.
func advanceCoReleaseRegistryTime(ctx context.Context, mux rebuild.RegistryMux, pkg, version string, release *pypireg.Release, reqs []string, registryTime time.Time) time.Time {
	normPkg := normalizeName(pkg)
	var pinnedSiblings []string
	for _, req := range reqs {
		reqPkg := requirementName(req)
		if reqPkg == "" || reqPkg == normPkg {
			continue
		}
		for _, m := range exactVersionPinPat.FindAllStringSubmatch(req, -1) {
			if m[1] == version {
				pinnedSiblings = append(pinnedSiblings, reqPkg)
			}
		}
	}
	if len(pinnedSiblings) == 0 {
		return registryTime
	}
	var latest time.Time
	if mux.PyPI != nil {
		for _, sib := range pinnedSiblings {
			if depRel, err := mux.PyPI.Release(ctx, sib, version); err == nil && depRel != nil {
				for _, art := range depRel.Artifacts {
					if art.UploadTime.After(latest) {
						latest = art.UploadTime
					}
				}
			}
		}
	}
	if release != nil {
		for _, art := range release.Artifacts {
			if art.UploadTime.After(latest) {
				latest = art.UploadTime
			}
		}
	}
	if latest.IsZero() {
		return registryTime
	}
	// Format(time.RFC3339) truncates sub-second precision, so round up to the next whole
	// second to ensure all artifacts uploaded within that second are visible in timewarp.
	rounded := latest.Truncate(time.Second).Add(time.Second)
	if rounded.After(registryTime) {
		return rounded
	}
	return registryTime
}
