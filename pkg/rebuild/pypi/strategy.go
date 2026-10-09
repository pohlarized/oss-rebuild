// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
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

// needsVenvOnPath flags backends that find their binary on PATH rather than
// beside the interpreter, as uv_build does via shutil.which.
func needsVenvOnPath(reqs []string) string {
	if hasRequirement(reqs, "uv-build") {
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
	PythonTag    string                         `json:"python_tag,omitempty" yaml:"python_tag,omitempty"`
	ABITag       string                         `json:"abi_tag,omitempty" yaml:"abi_tag,omitempty"`
	Requirements []string                       `json:"requirements" yaml:"requirements"`
	PlatformTag  string                         `json:"platform_tag,omitempty" yaml:"platform_tag,omitempty"`
	SystemDeps   []sysdeps.DependencyIdentifier `json:"system_deps,omitempty" yaml:"system_deps,omitempty"`
	StripModes   map[string]string              `json:"strip_modes,omitempty" yaml:"strip_modes,omitempty"`
	RegistryTime time.Time                      `json:"registry_time" yaml:"registry_time,omitempty"`
}

var _ rebuild.Strategy = &PlatformWheelBuild{}

func (b *PlatformWheelBuild) BaseImage() (string, error) {
	return platform.SelectBaseImage(b.PlatformTag)
}

func (b *PlatformWheelBuild) ToWorkflow() (*rebuild.WorkflowStrategy, error) {
	baseImage, err := b.BaseImage()
	if err != nil {
		return nil, errors.Wrap(err, "selecting base image")
	}
	var registryTime string
	if !b.RegistryTime.IsZero() {
		registryTime = b.RegistryTime.Format(time.RFC3339)
	}
	distDir := func() string {
		if b.Location.Dir != "" {
			return b.Location.Dir + "/dist"
		}
		return "dist"
	}()
	targetOS := rebuild.MapOS(baseImage)
	var packagesJSON, unmappableJSON, extractedJSON, stripModesJSON string
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
	if len(b.StripModes) > 0 {
		stripModesJSON = flow.MustToJSON(b.StripModes)
	}
	return &rebuild.WorkflowStrategy{
		Location: b.Location,
		Requires: rebuild.RequiredEnv{
			BaseImage: baseImage,
		},
		Source: []flow.Step{{
			Uses: "git-checkout",
		}},
		Deps: []flow.Step{{
			Uses: "pypi/deps/platform-wheel",
			With: map[string]string{
				"registryTime": registryTime,
				"requirements": flow.MustToJSON(b.Requirements),
				"pythonTag":    b.PythonTag,
				"abiTag":       b.ABITag,
				"venv":         "/deps",
				"targetOS":     string(targetOS),
				"packages":     packagesJSON,
				"unmappable":   unmappableJSON,
				"extracted":    extractedJSON,
			},
		}},
		Build: []flow.Step{{
			Uses: "pypi/build/platform-wheel",
			With: map[string]string{
				"dir":     b.Location.Dir,
				"distDir": distDir,
				"locator": "/deps/bin/",
				// auditwheel repair --plat requires a single policy tag rather than a compressed
				// tag set, and the highest tag matches the build container policy.
				"highestPlatformTag": platform.HighestLibcTagString(b.PlatformTag),
				"targetPlatformTag":  b.PlatformTag,
				"legacyWheel":        needsLegacyWheel(b.Requirements),
				"stripModes":         stripModesJSON,
			},
		}},
		OutputDir: distDir,
	}, nil
}


// wheel 0.38.0 added the `wheel tags` CLI subcommand.
const wheelTagsMinVersion = "0.38.0"

// needsLegacyWheel flags builds that resolve a wheel version predating `wheel tags`.
func needsLegacyWheel(reqs []string) string {
	if hasCeilingBelow(reqs, "wheel", wheelTagsMinVersion) {
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
			// TODO: Support Python 2.7 (requires virtualenv instead of standard library venv).
			Runs: textwrap.Dedent(`
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
				$INTERPRETER -m venv {{.With.path}}`)[1:],
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
				{{end}}{{$.With.locator}}pip install '{{regexReplace $req "'" "'\\''"}}'{{end}}`)[1:],
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
					"path":      "{{.With.venv}}",
					"pythonTag": "{{.With.pythonTag}}",
					"abiTag":    "{{.With.abiTag}}",
				},
			},
			{
				Runs: "{{.With.venv}}/bin/pip install build wheel auditwheel",
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
		Name: "pypi/build/platform-wheel",
		Steps: []flow.Step{{
			Runs: textwrap.Dedent(`
				{{.With.locator}}python3 -m build --wheel -n{{if and (ne .With.dir ".") (ne .With.dir "")}} {{.With.dir}}{{end}}
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
				  {{.With.locator}}python3 -m wheel unpack {{.With.distDir}}/*.whl -d {{.With.distDir}}/unpacked
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
				  {{.With.locator}}python3 -m wheel pack {{.With.distDir}}/unpacked/* -d {{.With.distDir}}
				  rm -rf {{.With.distDir}}/unpacked
				fi
				{{- else -}}
				{{.With.locator}}python3 -m wheel tags --remove --platform-tag {{.With.targetPlatformTag}} {{.With.distDir}}/*.whl
				{{- end}}
				{{- end}}
				{{- if .With.stripModes}}
				cat << 'EOF' > /tmp/strip_wheels.py
				import base64
				import hashlib
				import json
				import os
				from pathlib import Path
				import re
				import subprocess
				import sys
				import tempfile
				import zipfile

				dist_dir = Path(sys.argv[1])
				strip_modes = json.loads(sys.argv[2])

				for whl in sorted(dist_dir.glob("*.whl")):
				    tmp_whl = whl.with_suffix(".tmp.whl")
				    modified = False
				    with zipfile.ZipFile(whl, "r") as zin:
				        items = zin.infolist()
				        for item in items:
				            base = Path(item.filename).name
				            unhashed = re.sub(r'-[0-9a-fA-F]{8,}\.so', '.so', base)
				            if strip_modes.get(item.filename) or strip_modes.get(base) or strip_modes.get(unhashed):
				                modified = True
				                break
				        if not modified:
				            continue
				        with zipfile.ZipFile(tmp_whl, "w") as zout:
				            record_item = None
				            records = {}
				            for item in items:
				                content = zin.read(item.filename)
				                if item.filename.endswith(".dist-info/RECORD"):
				                    record_item = item
				                    continue
				                base = Path(item.filename).name
				                unhashed = re.sub(r'-[0-9a-fA-F]{8,}\.so', '.so', base)
				                mode = strip_modes.get(item.filename) or strip_modes.get(base) or strip_modes.get(unhashed)
				                if mode:
				                    with tempfile.NamedTemporaryFile(delete=False) as tf:
				                        tf.write(content)
				                        t_name = tf.name
				                    try:
				                        if mode == "all":
				                            subprocess.run(["strip", "-s", t_name], check=True)
				                        elif mode == "debug":
				                            subprocess.run(["objcopy", "-R", ".debug_*", "-R", ".zdebug_*", "-R", ".gdb_index", t_name], check=True)
				                        with open(t_name, "rb") as tf:
				                            content = tf.read()
				                    finally:
				                        if os.path.exists(t_name):
				                            os.remove(t_name)
				                zout.writestr(item, content)
				                digest = base64.urlsafe_b64encode(hashlib.sha256(content).digest()).decode("latin1").rstrip("=")
				                records[item.filename] = f"sha256={digest},{len(content)}"
				            if record_item:
				                rec_lines = [f"{fn},{records[fn]}" for fn in sorted(records.keys())]
				                rec_lines.append(f"{record_item.filename},,")
				                zout.writestr(record_item, "\n".join(rec_lines) + "\n")
				    if modified:
				        tmp_whl.replace(whl)
				EOF
				{{.With.locator}}python3 /tmp/strip_wheels.py {{.With.distDir}} '{{.With.stripModes}}'
				rm -f /tmp/strip_wheels.py
				{{- end}}`)[1:],
		}},
	},
}

