// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/oss-rebuild/internal/textwrap"
	"github.com/google/oss-rebuild/pkg/rebuild/flow"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/platform"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/sysdeps"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	"github.com/pkg/errors"
)

// PureWheelBuild aggregates the options controlling a wheel build.
type PureWheelBuild struct {
	rebuild.Location
	PythonVersion string    `json:"python_version" yaml:"python_version"`
	PythonTag     string    `json:"python_tag,omitempty" yaml:"python_tag,omitempty"`
	Requirements  []string  `json:"requirements" yaml:"requirements"`
	RegistryTime  time.Time `json:"registry_time" yaml:"registry_time,omitempty"`
}

var _ rebuild.Strategy = &PureWheelBuild{}

func (b *PureWheelBuild) ToWorkflow() *rebuild.WorkflowStrategy {
	var registryTime string
	if !b.RegistryTime.IsZero() {
		registryTime = b.RegistryTime.Format(time.RFC3339)
	}
	return &rebuild.WorkflowStrategy{
		Location: b.Location,
		Source: []flow.Step{{
			Uses: "git-checkout",
		}},
		Deps: []flow.Step{{
			Uses: "pypi/deps/basic",
			With: map[string]string{
				"registryTime":  registryTime,
				"requirements":  flow.MustToJSON(b.Requirements),
				"pythonVersion": b.PythonVersion,
				"venv":          "/deps",
			},
		}},
		Build: []flow.Step{{
			Uses: "pypi/build/wheel",
			With: map[string]string{
				"dir":        b.Location.Dir,
				"locator":    "/deps/bin/",
				"venvOnPath": needsVenvOnPath(b.Requirements),
				"pythonTag":  b.PythonTag,
			},
		}},
		OutputDir: func() string {
			if b.Location.Dir != "" {
				return b.Location.Dir + "/dist"
			}
			return "dist"
		}(),
	}
}

// GenerateFor generates the instructions for a PureWheelBuild.
func (b *PureWheelBuild) GenerateFor(t rebuild.Target, be rebuild.BuildEnv) (rebuild.Instructions, error) {
	return b.ToWorkflow().GenerateFor(t, be)
}

// needsVenvOnPath flags backends that find their binary or build tools on PATH
// rather than beside the interpreter, such as uv-build, meson-python, and
// scikit-build-core.
func needsVenvOnPath(reqs []string) string {
	if hasRequirement(reqs, "uv-build", "meson-python", "scikit-build-core") {
		return "1"
	}
	return ""
}

// SdistBuild includes elements for building an sdist.
type SdistBuild struct {
	rebuild.Location
	PythonVersion string    `json:"python_version" yaml:"python_version"`
	Requirements  []string  `json:"requirements" yaml:"requirements"`
	RegistryTime  time.Time `json:"registry_time" yaml:"registry_time,omitempty"`
}

var _ rebuild.Strategy = &SdistBuild{}

func (b *SdistBuild) ToWorkflow() *rebuild.WorkflowStrategy {
	var registryTime string
	if !b.RegistryTime.IsZero() {
		registryTime = b.RegistryTime.Format(time.RFC3339)
	}
	return &rebuild.WorkflowStrategy{
		Location: b.Location,
		Source: []flow.Step{{
			Uses: "git-checkout",
		}},
		Deps: []flow.Step{{
			Uses: "pypi/deps/basic",
			With: map[string]string{
				"registryTime":  registryTime,
				"requirements":  flow.MustToJSON(b.Requirements),
				"pythonVersion": b.PythonVersion,
				"venv":          "/deps",
			},
		}},
		Build: []flow.Step{{
			Uses: "pypi/build/sdist",
			With: map[string]string{
				"dir":        b.Location.Dir,
				"locator":    "/deps/bin/",
				"venvOnPath": needsVenvOnPath(b.Requirements),
			},
		}},
		OutputDir: func() string {
			if b.Location.Dir != "" {
				return b.Location.Dir + "/dist"
			}
			return "dist"
		}(),
	}
}

// GenerateFor generates the instructions for a SourceDistBuild.
func (b *SdistBuild) GenerateFor(t rebuild.Target, be rebuild.BuildEnv) (rebuild.Instructions, error) {
	return b.ToWorkflow().GenerateFor(t, be)
}

// PlatformWheelBuild aggregates the options controlling a platform-specific wheel build.
type PlatformWheelBuild struct {
	rebuild.Location
	PythonTag        string                         `json:"python_tag,omitempty" yaml:"python_tag,omitempty"`
	ABITag           string                         `json:"abi_tag,omitempty" yaml:"abi_tag,omitempty"`
	Requirements     []string                       `json:"requirements" yaml:"requirements"`
	Env              []string                       `json:"env,omitempty" yaml:"env,omitempty"`
	PlatformTag      string                         `json:"platform_tag,omitempty" yaml:"platform_tag,omitempty"`
	BaseImage        string                         `json:"base_image,omitempty" yaml:"base_image,omitempty"`
	SystemDeps       []sysdeps.DependencyIdentifier `json:"system_deps,omitempty" yaml:"system_deps,omitempty"`
	RustVersion      string                         `json:"rust_version,omitempty" yaml:"rust_version,omitempty"`
	Maturin          *MaturinBuild                  `json:"maturin,omitempty" yaml:"maturin,omitempty"`
	RegistryTime     time.Time                      `json:"registry_time" yaml:"registry_time,omitempty"`
	SanitizeSetupCfg bool                           `json:"sanitize_setup_cfg,omitempty" yaml:"sanitize_setup_cfg,omitempty"`
}

var _ rebuild.Strategy = &PlatformWheelBuild{}

func (b *PlatformWheelBuild) resolveBaseImage() (string, error) {
	if b.BaseImage != "" {
		return b.BaseImage, nil
	}
	return platform.SelectBaseImage(b.PlatformTag)
}

func (b *PlatformWheelBuild) ToWorkflow() (*rebuild.WorkflowStrategy, error) {
	baseImage, err := b.resolveBaseImage()
	if err != nil {
		return nil, errors.Wrap(err, "selecting base image")
	}
	var registryTime string
	if !b.RegistryTime.IsZero() && !needsLivePyPIIndex(b.PythonTag, b.ABITag) {
		registryTime = b.RegistryTime.Format(time.RFC3339)
	}
	distDir := func() string {
		if b.Location.Dir != "" {
			return b.Location.Dir + "/dist"
		}
		return "dist"
	}()
	targetOS := rebuild.MapOS(baseImage)
	var packagesJSON, unmappableJSON, extractedJSON, envJSON string
	if len(b.SystemDeps) > 0 {
		resolved := sysdeps.DefaultMapper.Map(targetOS, b.SystemDeps)
		if len(resolved.Packages) > 0 {
			packagesJSON = flow.MustToJSON(resolved.PackageNames())
		}
		if len(resolved.Unmappable) > 0 {
			unmappableJSON = flow.MustToJSON(resolved.Unmappable)
		}
		extractedJSON = flow.MustToJSON(b.SystemDeps)
	}
	if len(b.Env) > 0 {
		envJSON = flow.MustToJSON(b.Env)
	}
	var abi3InterpretersJSON, pyLimitedAPI string
	if b.ABITag == "abi3" {
		if dirs := abi3InterpreterDirs(b.PythonTag, b.Requirements); len(dirs) > 0 {
			abi3InterpretersJSON = flow.MustToJSON(dirs)
			pyLimitedAPI = b.PythonTag
		}
	}
	var sanitizeSetupCfg string
	if b.SanitizeSetupCfg {
		sanitizeSetupCfg = "1"
	}
	depsSteps := []flow.Step{{
		Uses: "pypi/deps/platform-wheel",
		With: map[string]string{
			"registryTime":     registryTime,
			"requirements":     flow.MustToJSON(b.Requirements),
			"pythonTag":        b.PythonTag,
			"abiTag":           b.ABITag,
			"abi3Interpreters": abi3InterpretersJSON,
			"venv":             "/deps",
			"targetOS":         string(targetOS),
			"packages":         packagesJSON,
			"unmappable":       unmappableJSON,
			"extracted":        extractedJSON,
			"upgradePip":       needsUpgradePip(b.PythonTag, b.ABITag, b.PlatformTag),
			"upgradeDeps":      needsUpgradeDeps(b.PythonTag, b.ABITag),
		},
	}}
	if b.RustVersion != "" {
		rustHost, err := inferRustHost(b.PlatformTag)
		if err != nil {
			return nil, errors.Wrap(err, "inferring rust host")
		}
		depsSteps = append(depsSteps, flow.Step{
			Uses: "pypi/install-rust",
			With: map[string]string{
				"rustVersion": b.RustVersion,
				"rustHost":    rustHost,
			},
		})
	}
	var buildSteps []flow.Step
	if b.Maturin != nil {
		buildSteps = []flow.Step{{
			Uses: "pypi/build/maturin-wheel",
			With: map[string]string{
				"dir":         b.Location.Dir,
				"locator":     "/deps/bin/",
				"policy":      b.Maturin.Policy,
				"features":    strings.Join(b.Maturin.Features, ","),
				"rustVersion": b.RustVersion,
			},
		}}
	} else {
		buildWith := map[string]string{
			"dir":        b.Location.Dir,
			"distDir":    distDir,
			"locator":    "/deps/bin/",
			"env":        envJSON,
			"venvOnPath": needsVenvOnPath(b.Requirements),
			// auditwheel repair --plat requires a single policy tag rather than a compressed
			// tag set, and the highest tag matches the build container policy.
			"highestPlatformTag": platform.HighestLibcTagString(b.PlatformTag),
			"targetPlatformTag":  b.PlatformTag,
			"legacyWheel":        needsLegacyWheel(b.PythonTag, b.Requirements),
			"python2":            needsPython2(b.PythonTag),
			"pyLimitedAPI":       pyLimitedAPI,
			"sanitizeSetupCfg":   sanitizeSetupCfg,
			"normalizeWheelName": needsWheelNameNormalization(b.PlatformTag, b.PythonTag, b.Requirements),
		}
		if b.RustVersion != "" {
			buildWith["rustVersion"] = b.RustVersion
		}
		buildSteps = []flow.Step{{
			Uses: "pypi/build/platform-wheel",
			With: buildWith,
		}}
	}
	return &rebuild.WorkflowStrategy{
		Location: b.Location,
		Requires: rebuild.RequiredEnv{
			BaseImage: baseImage,
		},
		Source: []flow.Step{{
			Uses: "git-checkout",
		}},
		Deps:      depsSteps,
		Build:     buildSteps,
		OutputDir: distDir,
	}, nil
}

// setuptools releases that dropped support for older CPython minors:
// - setuptools 59.7.0 requires Python >= 3.7
// - setuptools 68.1.0 requires Python >= 3.8
// - setuptools 76.0.0 requires Python >= 3.9
const (
	setuptoolsDropPy36Version = "59.7.0"
	setuptoolsDropPy37Version = "68.1.0"
	setuptoolsDropPy38Version = "76.0.0"
	maxABI3CPythonMinor       = 14
)

// abi3InterpreterDirs returns ordered /opt/python directory names for legacy
// abi3 python tags (cp36, cp37, cp38) that are absent from modern PyPA build
// images, starting at the lowest CPython minor satisfying both pythonTag and
// any pinned setuptools floor.
func abi3InterpreterDirs(pythonTag string, reqs []string) []string {
	var minMinor int
	switch pythonTag {
	case "cp36":
		minMinor = 6
	case "cp37":
		minMinor = 7
	case "cp38":
		minMinor = 8
	default:
		return nil
	}
	if hasFloorAtLeast(reqs, "setuptools", setuptoolsDropPy38Version) {
		minMinor = max(minMinor, 9)
	} else if hasFloorAtLeast(reqs, "setuptools", setuptoolsDropPy37Version) {
		minMinor = max(minMinor, 8)
	} else if hasFloorAtLeast(reqs, "setuptools", setuptoolsDropPy36Version) {
		minMinor = max(minMinor, 7)
	}
	var dirs []string
	for m := minMinor; m <= maxABI3CPythonMinor; m++ {
		if m <= 7 {
			dirs = append(dirs, fmt.Sprintf("cp3%d-cp3%dm", m, m))
		} else {
			dirs = append(dirs, fmt.Sprintf("cp3%d-cp3%d", m, m))
		}
	}
	return dirs
}

// wheel 0.38.0 added the `wheel tags` CLI subcommand.
const wheelTagsMinVersion = "0.38.0"

// needsLegacyWheel flags builds that resolve a wheel version predating `wheel tags`.
func needsLegacyWheel(pythonTag string, reqs []string) string {
	if pythonTag == "cp27" || hasCeilingBelow(reqs, "wheel", wheelTagsMinVersion) {
		return "1"
	}
	return ""
}

// needsPython2 flags Python 2.7 builds that lack PEP 517 build and venv support.
func needsPython2(pythonTag string) string {
	if pythonTag == "cp27" {
		return "1"
	}
	return ""
}

// needsLivePyPIIndex flags legacy Python 2.7 and 3.6 builds whose bundled pip
// predates PEP 691 JSON Simple API support and cannot upgrade to pip>=22.2.
func needsLivePyPIIndex(pythonTag, abiTag string) bool {
	return pythonTag == "cp27" || (pythonTag == "cp36" && abiTag != "abi3")
}

// needsUpgradePip flags Python 3.7 manylinux builds whose container images ship
// pip<22.2 (which sends HTML Accept headers rejected by unpatched timewarp) but
// can upgrade to pip 24.0 before enabling the timewarp index.
func needsUpgradePip(pythonTag, abiTag, platformTag string) string {
	if pythonTag == "cp37" && abiTag != "abi3" && strings.Contains(platformTag, "manylinux") {
		return "1"
	}
	return ""
}

// needsUpgradeDeps flags Python 3.6 builds where `python -m venv` seeds
// setuptools 40.6.2 and pip 18.1 skips installing a <=X ceiling unless
// --upgrade is passed.
func needsUpgradeDeps(pythonTag, abiTag string) string {
	if pythonTag == "cp36" && abiTag != "abi3" {
		return "1"
	}
	return ""
}

// needsWheelNameNormalization flags builds where wheel tags, auditwheel, or
// legacy bdist_wheel may emit a filename that differs from Target.Artifact due
// to canonicalized platform tag order or unnormalized distribution name casing.
func needsWheelNameNormalization(platformTag, pythonTag string, reqs []string) string {
	if !slices.IsSorted(strings.Split(platformTag, ".")) {
		return "1"
	}
	if needsLegacyWheel(pythonTag, reqs) == "1" {
		return "1"
	}
	switch pythonTag {
	case "cp27", "cp36", "cp37":
		return "1"
	}
	return ""
}

// GenerateFor generates the instructions for a PlatformWheelBuild.
func (b *PlatformWheelBuild) GenerateFor(t rebuild.Target, be rebuild.BuildEnv) (rebuild.Instructions, error) {
	wf, err := b.ToWorkflow()
	if err != nil {
		return rebuild.Instructions{}, err
	}
	return wf.GenerateFor(t, be)
}

func init() {
	for _, t := range toolkit {
		flow.Tools.MustRegister(t)
	}
}

// Base tools for individual operations
var toolkit = []*flow.Tool{
	{
		Name: "pypi/setup-venv",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{if .With.pythonVersion -}}
				{{.With.locator}}uvx uv venv {{.With.path}} --seed --python {{.With.pythonVersion}}
				{{- else -}}
				{{.With.locator}}python3 -m venv {{.With.path}}
				{{- end -}}`)[1:],
			Needs: []string{"python3", "uv"},
		}},
	},
	{
		Name: "pypi/setup-venv/manylinux",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{- if eq .With.abiTag "cp313t" -}}
				if [ -d "/opt/python/{{.With.pythonTag}}-{{.With.abiTag}}" ]; then
				  /opt/python/{{.With.pythonTag}}-{{.With.abiTag}}/bin/python -m venv {{.With.path}}
				else
				  uv venv {{.With.path}} --seed --python 3.13t
				fi
				{{- else if .With.abi3Interpreters -}}
				INTERPRETER=""
				for dir in{{range $dir := .With.abi3Interpreters | fromJSON}} {{$dir}}{{end}}; do
				  if [ -d "/opt/python/$dir" ]; then
				    INTERPRETER="/opt/python/$dir/bin/python"
				    break
				  fi
				done
				if [ -z "$INTERPRETER" ]; then
				  echo "Error: Requested Python tag '{{.With.pythonTag}}' not found in /opt/python" >&2
				  exit 1
				fi
				$INTERPRETER -m venv {{.With.path}}
				{{- else -}}
				INTERPRETER=""
				{{- if .With.pythonTag}}
				{{- if .With.abiTag}}
				if [ -d "/opt/python/{{.With.pythonTag}}-{{.With.abiTag}}" ]; then
				  INTERPRETER="/opt/python/{{.With.pythonTag}}-{{.With.abiTag}}/bin/python"
				else
				  for dir in /opt/python/{{.With.pythonTag}}*; do
				    if [ -d "$dir" ]; then
				      INTERPRETER="$dir/bin/python"
				      break
				    fi
				  done
				fi
				{{- else}}
				for dir in /opt/python/{{.With.pythonTag}}*; do
				  if [ -d "$dir" ]; then
				    INTERPRETER="$dir/bin/python"
				    break
				  fi
				done
				{{- end}}
				if [ -z "$INTERPRETER" ]; then
				  echo "Error: Requested Python tag '{{.With.pythonTag}}' not found in /opt/python" >&2
				  exit 1
				fi
				{{- else}}
				if [ -d "/opt/python/cp310-cp310" ]; then
				  INTERPRETER="/opt/python/cp310-cp310/bin/python"
				else
				  for dir in /opt/python/*; do
				    if [ -d "$dir" ]; then
				      INTERPRETER="$dir/bin/python"
				    fi
				  done
				fi
				{{- end}}
				{{- if eq .With.pythonTag "cp27"}}
				ln -s "$(dirname "$(dirname "$INTERPRETER")")" {{.With.path}}
				{{- else}}
				$INTERPRETER -m venv {{.With.path}}
				{{- end}}
				{{- end -}}`)[1:],
		}},
	},
	{
		Name: "pypi/setup-registry",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{if ne .With.registryTime "" -}}
				export PIP_INDEX_URL={{.BuildEnv.TimewarpURLFromString "pypi" .With.registryTime}}/simple
				{{- end -}}`)[1:],
			Needs: []string{},
		}},
	},
	{
		Name: "pypi/install-deps",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{range $i, $req := .With.requirements | fromJSON}}{{if $i}}
				{{end}}{{$.With.locator}}pip install {{if $.With.upgrade}}--upgrade {{end}}'{{regexReplace $req "'" "'\\''"}}'{{end}}`)[1:],
		}},
	},

	// Composite tools for common workflow steps
	{
		Name: "pypi/deps/basic",
		Steps: []flow.Step{
			{
				Uses: "pypi/setup-venv",
				With: map[string]string{
					"locator":       "/usr/bin/",
					"path":          "{{.With.venv}}",
					"pythonVersion": "{{.With.pythonVersion}}",
				},
			},
			{
				// Fetch the PEP 517 frontend from the real index, before timewarp.
				// The frontend doesn't impact the output and contemporary
				// versions lacked CLI flags and features we use.
				Runs: "{{.With.venv}}/bin/pip install build",
			},
			{
				Uses: "pypi/setup-registry",
				With: map[string]string{
					"registryTime": "{{.With.registryTime}}",
				},
			},
			{
				Uses: "pypi/install-deps",
				With: map[string]string{
					"requirements": "{{.With.requirements}}",
					"locator":      "{{.With.venv}}/bin/",
				},
			},
		},
	},
	{
		Name: "pypi/install-sysdeps",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{- if or .With.packages .With.unmappable .With.extracted -}}
				echo "[sysdeps] Target OS: {{.With.targetOS}}"
				{{- if .With.extracted}}
				echo "[sysdeps] Extracted dependency identifiers:"
				{{range $id := .With.extracted | fromJSON -}}
				echo "[sysdeps]   - {{$id.namespace}}:{{$id.name}}{{if $id.provenance}} (from {{$id.provenance}}){{end}}"
				{{end -}}
				{{end -}}
				{{if .With.unmappable -}}
				echo "[sysdeps] WARNING: The following dependency identifiers could not be mapped to {{.With.targetOS}}:"
				{{range $id := .With.unmappable | fromJSON -}}
				echo "[sysdeps]   - {{$id.namespace}}:{{$id.name}}{{if $id.provenance}} (from {{$id.provenance}}){{end}}"
				{{end -}}
				{{end -}}
				{{if .With.packages -}}
				echo "[sysdeps] Installing candidate system package(s) (fail-open)..."
				{{range $pkg := .With.packages | fromJSON -}}
				if {{if eq $.With.targetOS "alpine"}}apk add '{{$pkg}}'{{else if eq $.With.targetOS "almalinux"}}dnf install -y '{{$pkg}}'{{else}}yum install -y '{{$pkg}}'{{end}}; then
				  echo "[sysdeps]   + OK: {{$pkg}}"
				else
				  echo "[sysdeps]   ! INSTALL_FAILED: {{$pkg}}" >&2
				fi
				{{end -}}
				{{end -}}
				{{end -}}`)[1:],
		}},
	},
	{
		Name: "pypi/deps/platform-wheel",
		Steps: []flow.Step{
			{
				Uses: "pypi/install-sysdeps",
				With: map[string]string{
					"targetOS":   "{{.With.targetOS}}",
					"packages":   "{{.With.packages}}",
					"unmappable": "{{.With.unmappable}}",
					"extracted":  "{{.With.extracted}}",
				},
			},
			{
				Uses: "pypi/setup-venv/manylinux",
				With: map[string]string{
					"path":             "{{.With.venv}}",
					"pythonTag":        "{{.With.pythonTag}}",
					"abiTag":           "{{.With.abiTag}}",
					"abi3Interpreters": "{{.With.abi3Interpreters}}",
				},
			},
			{
				Runs: "{{if ne .With.pythonTag \"cp27\"}}{{.With.venv}}/bin/pip install {{if .With.upgradePip}}-U pip {{end}}build wheel auditwheel{{end}}",
			},
			{
				Uses: "pypi/setup-registry",
				With: map[string]string{
					"registryTime": "{{.With.registryTime}}",
				},
			},
			{
				Uses: "pypi/install-deps",
				With: map[string]string{
					"requirements": "{{.With.requirements}}",
					"locator":      "{{.With.venv}}/bin/",
					"upgrade":      "{{.With.upgradeDeps}}",
				},
			},
		},
	},
	{
		Name: "pypi/build/wheel",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{- if .With.pythonTag -}}
				printf '[bdist_wheel]\npython-tag = {{.With.pythonTag}}\n' >~/.pydistutils.cfg
				{{end -}}
				{{if .With.venvOnPath}}PATH={{.With.locator}}:$PATH {{end}}{{.With.locator}}python3 -m build --wheel -n{{if and (ne .With.dir ".") (ne .With.dir "")}} {{.With.dir}}{{end}}`)[1:],
		}},
	},
	{
		Name: "pypi/build/sdist",
		Steps: []flow.Step{
			{
				Runs: textwrap.Dedent(`
				{{if .With.venvOnPath}}PATH={{.With.locator}}:$PATH {{end}}{{.With.locator}}python3 -m build --sdist -n{{if and (ne .With.dir ".") (ne .With.dir "")}} {{.With.dir}}{{end}}`)[1:],
			}},
	},
	{
		Name: "pypi/install-rust",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{- if .With.rustVersion -}}
				export CARGO_HOME=/root/.cargo RUSTUP_HOME=/root/.rustup
				curl --proto '=https' --tlsv1.2 -sSfL -o /tmp/rustup-init https://static.rust-lang.org/rustup/dist/{{.With.rustHost}}/rustup-init
				curl --proto '=https' --tlsv1.2 -sSfL https://static.rust-lang.org/rustup/dist/{{.With.rustHost}}/rustup-init.sha256 | awk '{print $1 "  /tmp/rustup-init"}' | sha256sum -c -
				chmod +x /tmp/rustup-init
				/tmp/rustup-init -y --no-modify-path --profile minimal --default-host {{.With.rustHost}} --default-toolchain {{.With.rustVersion}}
				rm -f /tmp/rustup-init
				{{- end -}}`)[1:],
		}},
	},
	{
		Name: "pypi/build/platform-wheel",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{- if .With.rustVersion -}}
				export CARGO_HOME=/root/.cargo RUSTUP_HOME=/root/.rustup RUSTUP_TOOLCHAIN={{.With.rustVersion}} PATH=/root/.cargo/bin:$PATH CIBW_BUILD=1
				{{end -}}
				{{- if .With.env -}}
				{{range $e := .With.env | fromJSON -}}
				export {{$e}}
				{{end -}}
				{{end -}}
				{{- if .With.sanitizeSetupCfg -}}
				sed -i '/^\[egg_info\]/,/^\[/ { /^tag_build/d; /^tag_date/d; }' {{if and (ne .With.dir ".") (ne .With.dir "")}}{{.With.dir}}/{{end}}setup.cfg
				{{end -}}
				{{- if .With.pyLimitedAPI -}}
				printf '[bdist_wheel]\npy_limited_api = {{.With.pyLimitedAPI}}\n' >~/.pydistutils.cfg
				{{end -}}
				{{if .With.python2 -}}
				{{if and (ne .With.dir ".") (ne .With.dir "") -}}
				(cd {{.With.dir}} && {{.With.locator}}python setup.py bdist_wheel -d dist)
				{{- else -}}
				{{.With.locator}}python setup.py bdist_wheel -d {{.With.distDir}}
				{{- end}}
				{{- else -}}
				{{if .With.venvOnPath}}PATH={{.With.locator}}:$PATH {{end}}{{.With.locator}}python3 -m build --wheel -n{{if and (ne .With.dir ".") (ne .With.dir "")}} {{.With.dir}}{{end}}
				{{- end}}
				{{if .With.highestPlatformTag -}}
				mkdir -p {{.With.distDir}}/repaired
				AUDITWHEEL="{{.With.locator}}auditwheel"
				if [ ! -x "$AUDITWHEEL" ]; then
				  AUDITWHEEL="auditwheel"
				fi
				if $AUDITWHEEL repair {{.With.distDir}}/*.whl --plat {{.With.highestPlatformTag}} -w {{.With.distDir}}/repaired/; then
				  rm -f {{.With.distDir}}/*.whl
				  mv {{.With.distDir}}/repaired/*.whl {{.With.distDir}}/
				fi
				rm -rf {{.With.distDir}}/repaired
				{{end -}}
				{{if .With.targetPlatformTag -}}
				{{if .With.legacyWheel -}}
				if [ ! -e {{.With.distDir}}/*-{{.With.targetPlatformTag}}.whl ]; then
				  {{.With.locator}}{{if .With.python2}}python{{else}}python3{{end}} -m wheel unpack {{.With.distDir}}/*.whl -d {{.With.distDir}}/unpacked
				  rm -f {{.With.distDir}}/*.whl
				  for f in {{.With.distDir}}/unpacked/*/*.dist-info/WHEEL; do
				    prefixes=$(sed -n 's/^Tag: \([^-]*-[^-]*\)-.*/\1/p' "$f" | sort -u)
				    sed -i '/^Tag: /d; /^$/d' "$f"
				    for p in $prefixes; do
				      for plat in $(echo '{{.With.targetPlatformTag}}' | tr '.' '\n' | sort -u); do
				        echo "Tag: $p-$plat" >> "$f"
				      done
				    done
				    echo "" >> "$f"
				  done
				  {{.With.locator}}{{if .With.python2}}python{{else}}python3{{end}} -m wheel pack {{.With.distDir}}/unpacked/* -d {{.With.distDir}}
				  rm -rf {{.With.distDir}}/unpacked
				fi
				{{- else -}}
				{{.With.locator}}python3 -m wheel tags --remove --platform-tag {{.With.targetPlatformTag}} {{.With.distDir}}/*.whl
				{{- end -}}
				{{- end -}}
				{{- if .With.normalizeWheelName}}
				for f in {{.With.distDir}}/*.whl; do
				  if [ -f "$f" ] && [ "$(basename "$f")" != "{{.Target.Artifact}}" ]; then
				    mv "$f" "{{.With.distDir}}/{{.Target.Artifact}}"
				  fi
				  break
				done
				{{- end -}}`)[1:],
		}},
	},
	{
		Name: "pypi/build/maturin-wheel",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{- if .With.rustVersion -}}
				export CARGO_HOME=/root/.cargo RUSTUP_HOME=/root/.rustup RUSTUP_TOOLCHAIN={{.With.rustVersion}} PATH=/root/.cargo/bin:$PATH CIBW_BUILD=1
				{{end -}}
				{{if and (ne .With.dir ".") (ne .With.dir "")}}(cd {{.With.dir}} && {{.With.locator}}maturin build --release -i {{.With.locator}}python --compatibility {{.With.policy}}{{if .With.features}} --features {{.With.features}}{{end}} --out dist){{else}}{{.With.locator}}maturin build --release -i {{.With.locator}}python --compatibility {{.With.policy}}{{if .With.features}} --features {{.With.features}}{{end}} --out dist{{end}}`)[1:],
		}},
	},
}
