// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/oss-rebuild/internal/textwrap"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/platform"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/sysdeps"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
)

const defaultSource = `git checkout --force 'the_ref'
if [ -f .gitmodules ]; then
  git config --global url."https://github.com/".insteadOf "git@github.com:" || true
  git config --global url."https://gitlab.com/".insteadOf "git@gitlab.com:" || true
  git config --global url."https://bitbucket.org/".insteadOf "git@bitbucket.org:" || true
  git config --global url."https://codeberg.org/".insteadOf "git@codeberg.org:" || true
  git config --global url."https://".insteadOf "git://" || true
  git submodule sync --recursive || true
  GIT_TERMINAL_PROMPT=0 git submodule update --init || true
  GIT_TERMINAL_PROMPT=0 git submodule foreach --recursive 'git submodule sync || true; GIT_TERMINAL_PROMPT=0 git submodule update --init || true' || true
fi`

func TestPureWheelBuild(t *testing.T) {
	defaultLocation := rebuild.Location{
		Dir:  "the_dir", // Changed due to directory parsing logic in infer
		Ref:  "the_ref",
		Repo: "the_repo",
	}
	tests := []struct {
		name     string
		strategy rebuild.Strategy
		want     rebuild.Instructions
	}{
		{
			"WithDeps",
			&PureWheelBuild{
				Location:     defaultLocation,
				Requirements: []string{"req_1", "req_2"},
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build
/deps/bin/pip install 'req_1'
/deps/bin/pip install 'req_2'`,
				Build: "/deps/bin/python3 -m build --wheel -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithPythonTag",
			&PureWheelBuild{
				Location:     defaultLocation,
				Requirements: []string{"setuptools<=56.2.0"},
				PythonTag:    "py2.py3",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build
/deps/bin/pip install 'setuptools<=56.2.0'`,
				Build: `printf '[bdist_wheel]\npython-tag = py2.py3\n' >~/.pydistutils.cfg
/deps/bin/python3 -m build --wheel -n the_dir`,
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithUVBackend",
			&PureWheelBuild{
				Location:     defaultLocation,
				Requirements: []string{"uv-build==0.10.0"},
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build
/deps/bin/pip install 'uv-build==0.10.0'`,
				Build: "PATH=/deps/bin/:$PATH /deps/bin/python3 -m build --wheel -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"DepsEscaping",
			&PureWheelBuild{
				Location:     defaultLocation,
				Requirements: []string{"req_1<='1.2.3'"},
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build
/deps/bin/pip install 'req_1<='\''1.2.3'\'''`,
				Build: "/deps/bin/python3 -m build --wheel -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"NoDeps",
			&PureWheelBuild{
				Location: defaultLocation,
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build`,
				Build: "/deps/bin/python3 -m build --wheel -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithTimewarp",
			&PureWheelBuild{
				Location:     defaultLocation,
				RegistryTime: time.Date(2006, time.January, 2, 3, 4, 5, 0, time.UTC),
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build
export PIP_INDEX_URL=http://pypi:2006-01-02T03:04:05Z@orange/simple`,
				Build: "/deps/bin/python3 -m build --wheel -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithoutDir",
			&PureWheelBuild{
				Location: rebuild.Location{Ref: "the_ref", Repo: "the_repo"},
			},
			rebuild.Instructions{
				Location: rebuild.Location{Ref: "the_ref", Repo: "the_repo"},
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build`,
				Build: "/deps/bin/python3 -m build --wheel -n",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "dist/the_artifact",
			},
		},
		{
			"WithPythonVersion",
			&PureWheelBuild{
				Location:      defaultLocation,
				PythonVersion: "3.11",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/uvx uv venv /deps --seed --python 3.11
/deps/bin/pip install build`,
				Build: "/deps/bin/python3 -m build --wheel -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := tc.strategy.GenerateFor(rebuild.Target{Ecosystem: rebuild.PyPI, Package: "the_package", Version: "the_version", Artifact: "the_artifact"}, rebuild.BuildEnv{HasRepo: true, TimewarpHost: "orange"})
			if err != nil {
				t.Fatalf("%s: Strategy%v.GenerateFor() failed unexpectedly: %v", tc.name, tc.strategy, err)
			}
			if diff := cmp.Diff(inst, tc.want); diff != "" {
				t.Errorf("Strategy%v.GenerateFor() returned diff (-got +want):\n%s", tc.strategy, diff)
			}
		})
	}
}

func TestSourceDistBuild(t *testing.T) {
	defaultLocation := rebuild.Location{
		Dir:  "the_dir",
		Ref:  "the_ref",
		Repo: "the_repo",
	}
	tests := []struct {
		name     string
		strategy rebuild.Strategy
		want     rebuild.Instructions
	}{
		{
			"WithDeps",
			&SdistBuild{
				Location: defaultLocation,
				Requirements: []string{
					"req_1",
					"req_2",
				},
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build
/deps/bin/pip install 'req_1'
/deps/bin/pip install 'req_2'`,
				Build: "/deps/bin/python3 -m build --sdist -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"DepsEscaping",
			&SdistBuild{
				Location: defaultLocation,
				Requirements: []string{
					"req_1<='1.2.3'",
				},
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build
/deps/bin/pip install 'req_1<='\''1.2.3'\'''`,
				Build: "/deps/bin/python3 -m build --sdist -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"NoDeps",
			&SdistBuild{
				Location: defaultLocation,
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build`,
				Build: "/deps/bin/python3 -m build --sdist -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithTimewarp",
			&SdistBuild{
				Location:     defaultLocation,
				RegistryTime: time.Date(2006, time.January, 2, 3, 4, 5, 0, time.UTC),
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build
export PIP_INDEX_URL=http://pypi:2006-01-02T03:04:05Z@orange/simple`,
				Build: "/deps/bin/python3 -m build --sdist -n the_dir",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithoutDir",
			&SdistBuild{
				Location: rebuild.Location{Ref: "the_ref", Repo: "the_repo"},
			},
			rebuild.Instructions{
				Location: rebuild.Location{Ref: "the_ref", Repo: "the_repo"},
				Source:   defaultSource,
				Deps: `/usr/bin/python3 -m venv /deps
/deps/bin/pip install build`,
				Build: "/deps/bin/python3 -m build --sdist -n",
				Requires: rebuild.RequiredEnv{
					SystemDeps: []string{"git", "python3", "uv"},
				},
				OutputPath: "dist/the_artifact",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := tc.strategy.GenerateFor(rebuild.Target{Ecosystem: rebuild.PyPI, Package: "the_package", Version: "the_version", Artifact: "the_artifact"}, rebuild.BuildEnv{HasRepo: true, TimewarpHost: "orange"})
			if err != nil {
				t.Fatalf("%s: Strategy%v.GenerateFor() failed unexpectedly: %v", tc.name, tc.strategy, err)
			}
			if diff := cmp.Diff(inst, tc.want); diff != "" {
				t.Errorf("Strategy%v.GenerateFor() returned diff (-got +want):\n%s", tc.strategy, diff)
			}
		})
	}
}

func TestPlatformWheelBuild(t *testing.T) {
	defaultLocation := rebuild.Location{
		Dir:  "the_dir",
		Ref:  "the_ref",
		Repo: "the_repo",
	}
	tests := []struct {
		name     string
		strategy rebuild.Strategy
		want     rebuild.Instructions
	}{
		{
			"WithDeps",
			&PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp310",
				ABITag:       "cp310",
				Requirements: []string{"req_1", "req_2"},
				PlatformTag:  "manylinux_2_17_x86_64",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/cp310*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp310' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
/deps/bin/pip install 'req_1'
/deps/bin/pip install 'req_2'`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_17_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_17_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux2014_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"DepsEscaping",
			&PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp310",
				ABITag:       "cp310",
				Requirements: []string{"req_1<='1.2.3'"},
				PlatformTag:  "manylinux_2_17_x86_64",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/cp310*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp310' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
/deps/bin/pip install 'req_1<='\''1.2.3'\'''`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_17_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_17_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux2014_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"NoDeps",
			&PlatformWheelBuild{
				Location:    defaultLocation,
				PythonTag:   "cp310",
				ABITag:      "cp310",
				PlatformTag: "manylinux_2_17_x86_64",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/cp310*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp310' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_17_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_17_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux2014_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithTimewarp",
			&PlatformWheelBuild{
				Location:     defaultLocation,
				PlatformTag:  "manylinux_2_17_x86_64",
				RegistryTime: time.Date(2006, time.January, 2, 3, 4, 5, 0, time.UTC),
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
    fi
  done
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
export PIP_INDEX_URL=http://pypi:2006-01-02T03:04:05Z@orange/simple`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_17_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_17_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux2014_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithoutDir",
			&PlatformWheelBuild{
				Location:    rebuild.Location{Ref: "the_ref", Repo: "the_repo"},
				PlatformTag: "manylinux_2_17_x86_64",
			},
			rebuild.Instructions{
				Location: rebuild.Location{Ref: "the_ref", Repo: "the_repo"},
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
    fi
  done
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel`,
				Build: `/deps/bin/python3 -m build --wheel -n
mkdir -p dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair dist/*.whl --plat manylinux_2_17_x86_64 -w dist/repaired/; then
  rm -f dist/*.whl
  mv dist/repaired/*.whl dist/
fi
rm -rf dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_17_x86_64 dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux2014_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "dist/the_artifact",
			},
		},
		{
			"CompressedTagSet",
			&PlatformWheelBuild{
				Location:    defaultLocation,
				PythonTag:   "cp310",
				ABITag:      "cp310",
				PlatformTag: "manylinux1_x86_64.manylinux_2_28_x86_64.manylinux_2_5_x86_64",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/cp310*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp310' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux1_x86_64.manylinux_2_28_x86_64.manylinux_2_5_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux_2_28_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"SpecificPythonVersion",
			&PlatformWheelBuild{
				Location:    defaultLocation,
				PythonTag:   "cp38",
				ABITag:      "cp38",
				PlatformTag: "manylinux_2_28_x86_64",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp38-cp38" ]; then
  INTERPRETER="/opt/python/cp38-cp38/bin/python"
else
  for dir in /opt/python/cp38*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp38' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux_2_28_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"PythonTagWithoutABITag",
			&PlatformWheelBuild{
				Location:    defaultLocation,
				PythonTag:   "cp38",
				PlatformTag: "manylinux_2_28_x86_64",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
for dir in /opt/python/cp38*; do
  if [ -d "$dir" ]; then
    INTERPRETER="$dir/bin/python"
    break
  fi
done
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp38' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux_2_28_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithSystemDepsManylinux",
			&PlatformWheelBuild{
				Location:    defaultLocation,
				PythonTag:   "cp310",
				ABITag:      "cp310",
				PlatformTag: "manylinux_2_28_x86_64",
				SystemDeps: []sysdeps.DependencyIdentifier{
					{Namespace: sysdeps.NamespaceBinary, Name: "dot", Provenance: "setup.py"},
					{Namespace: sysdeps.NamespaceApt, Name: "graphviz-dev", Provenance: "test.yml"},
					{Namespace: "unknown_ns", Name: "custom-dep", Provenance: "notes.txt"},
				},
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `echo "[sysdeps] Target OS: almalinux"
echo "[sysdeps] Extracted dependency identifiers:"
echo "[sysdeps]   - bin:dot (from setup.py)"
echo "[sysdeps]   - apt:graphviz-dev (from test.yml)"
echo "[sysdeps]   - unknown_ns:custom-dep (from notes.txt)"
echo "[sysdeps] WARNING: The following dependency identifiers could not be mapped to almalinux:"
echo "[sysdeps]   - unknown_ns:custom-dep (from notes.txt)"
echo "[sysdeps] Installing candidate system package(s) (fail-open)..."
if dnf install -y '/usr/bin/dot'; then
  echo "[sysdeps]   + OK: /usr/bin/dot"
else
  echo "[sysdeps]   ! INSTALL_FAILED: /usr/bin/dot" >&2
fi
if dnf install -y 'graphviz-devel'; then
  echo "[sysdeps]   + OK: graphviz-devel"
else
  echo "[sysdeps]   ! INSTALL_FAILED: graphviz-devel" >&2
fi
INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/cp310*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp310' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux_2_28_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"WithSystemDepsAlpine",
			&PlatformWheelBuild{
				Location:    defaultLocation,
				PythonTag:   "cp310",
				ABITag:      "cp310",
				PlatformTag: "musllinux_1_2_x86_64",
				SystemDeps: []sysdeps.DependencyIdentifier{
					{Namespace: sysdeps.NamespaceBinary, Name: "dot"},
					{Namespace: sysdeps.NamespaceApt, Name: "graphviz-dev"},
				},
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `echo "[sysdeps] Target OS: alpine"
echo "[sysdeps] Extracted dependency identifiers:"
echo "[sysdeps]   - bin:dot"
echo "[sysdeps]   - apt:graphviz-dev"
echo "[sysdeps] Installing candidate system package(s) (fail-open)..."
if apk add 'cmd:dot'; then
  echo "[sysdeps]   + OK: cmd:dot"
else
  echo "[sysdeps]   ! INSTALL_FAILED: cmd:dot" >&2
fi
if apk add 'graphviz-dev'; then
  echo "[sysdeps]   + OK: graphviz-dev"
else
  echo "[sysdeps]   ! INSTALL_FAILED: graphviz-dev" >&2
fi
INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/cp310*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp310' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat musllinux_1_2_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag musllinux_1_2_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/musllinux_1_2_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"LegacyWheel",
			&PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp310",
				ABITag:       "cp310",
				Requirements: []string{"wheel==0.37.1", "setuptools<=67.7.2"},
				PlatformTag:  "manylinux_2_17_x86_64",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/cp310*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp310' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
/deps/bin/pip install 'wheel==0.37.1'
/deps/bin/pip install 'setuptools<=67.7.2'`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_17_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
if [ ! -e the_dir/dist/*-manylinux_2_17_x86_64.whl ]; then
  /deps/bin/python3 -m wheel unpack the_dir/dist/*.whl -d the_dir/dist/unpacked
  rm -f the_dir/dist/*.whl
  for f in the_dir/dist/unpacked/*/*.dist-info/WHEEL; do
    prefixes=$(sed -n 's/^Tag: \([^-]*-[^-]*\)-.*/\1/p' "$f" | sort -u)
    sed -i '/^Tag: /d; /^$/d' "$f"
    for p in $prefixes; do
      for plat in $(echo 'manylinux_2_17_x86_64' | tr '.' '\n' | sort -u); do
        echo "Tag: $p-$plat" >> "$f"
      done
    done
    echo "" >> "$f"
  done
  /deps/bin/python3 -m wheel pack the_dir/dist/unpacked/* -d the_dir/dist
  rm -rf the_dir/dist/unpacked
fi
for f in the_dir/dist/*.whl; do
  if [ -f "$f" ] && [ "$(basename "$f")" != "the_artifact" ]; then
    mv "$f" "the_dir/dist/the_artifact"
  fi
  break
done`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux2014_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"CP27Wheel",
			&PlatformWheelBuild{
				Location:     rebuild.Location{Repo: "https://github.com/simplejson/simplejson", Ref: "the_ref"},
				PythonTag:    "cp27",
				ABITag:       "cp27mu",
				Requirements: []string{"wheel==0.37.1", "setuptools<=56.2.0"},
				PlatformTag:  "manylinux1_x86_64",
				BaseImage:    "quay.io/pypa/manylinux2010_x86_64:2021-02-06-3d322a5",
				RegistryTime: time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
			},
			rebuild.Instructions{
				Location: rebuild.Location{Repo: "https://github.com/simplejson/simplejson", Ref: "the_ref"},
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp27-cp27mu" ]; then
  INTERPRETER="/opt/python/cp27-cp27mu/bin/python"
else
  for dir in /opt/python/cp27*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp27' not found in /opt/python" >&2
  exit 1
fi
ln -s "$(dirname "$(dirname "$INTERPRETER")")" /deps
/deps/bin/pip install 'wheel==0.37.1'
/deps/bin/pip install 'setuptools<=56.2.0'`,
				Build: `/deps/bin/python setup.py bdist_wheel -d dist
mkdir -p dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair dist/*.whl --plat manylinux1_x86_64 -w dist/repaired/; then
  rm -f dist/*.whl
  mv dist/repaired/*.whl dist/
fi
rm -rf dist/repaired
if [ ! -e dist/*-manylinux1_x86_64.whl ]; then
  /deps/bin/python -m wheel unpack dist/*.whl -d dist/unpacked
  rm -f dist/*.whl
  for f in dist/unpacked/*/*.dist-info/WHEEL; do
    prefixes=$(sed -n 's/^Tag: \([^-]*-[^-]*\)-.*/\1/p' "$f" | sort -u)
    sed -i '/^Tag: /d; /^$/d' "$f"
    for p in $prefixes; do
      for plat in $(echo 'manylinux1_x86_64' | tr '.' '\n' | sort -u); do
        echo "Tag: $p-$plat" >> "$f"
      done
    done
    echo "" >> "$f"
  done
  /deps/bin/python -m wheel pack dist/unpacked/* -d dist
  rm -rf dist/unpacked
fi
for f in dist/*.whl; do
  if [ -f "$f" ] && [ "$(basename "$f")" != "the_artifact" ]; then
    mv "$f" "dist/the_artifact"
  fi
  break
done`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux2010_x86_64:2021-02-06-3d322a5",
					SystemDeps: []string{"git"},
				},
				OutputPath: "dist/the_artifact",
			},
		},
		{
			"CP36Wheel",
			&PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp36",
				ABITag:       "cp36m",
				Requirements: []string{"wheel==0.40.0", "setuptools<=59.6.0"},
				PlatformTag:  "musllinux_1_1_x86_64",
				RegistryTime: time.Date(2023, time.July, 1, 0, 0, 0, 0, time.UTC),
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp36-cp36m" ]; then
  INTERPRETER="/opt/python/cp36-cp36m/bin/python"
else
  for dir in /opt/python/cp36*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp36' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
/deps/bin/pip install --upgrade 'wheel==0.40.0'
/deps/bin/pip install --upgrade 'setuptools<=59.6.0'`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat musllinux_1_1_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag musllinux_1_1_x86_64 the_dir/dist/*.whl
for f in the_dir/dist/*.whl; do
  if [ -f "$f" ] && [ "$(basename "$f")" != "the_artifact" ]; then
    mv "$f" "the_dir/dist/the_artifact"
  fi
  break
done`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/musllinux_1_1_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"CP37ManylinuxWheel",
			&PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp37",
				ABITag:       "cp37m",
				Requirements: []string{"wheel==0.40.0", "setuptools<=67.7.2"},
				PlatformTag:  "manylinux_2_17_x86_64",
				RegistryTime: time.Date(2023, time.July, 1, 0, 0, 0, 0, time.UTC),
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp37-cp37m" ]; then
  INTERPRETER="/opt/python/cp37-cp37m/bin/python"
else
  for dir in /opt/python/cp37*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp37' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install -U pip build wheel auditwheel
export PIP_INDEX_URL=http://pypi:2023-07-01T00:00:00Z@orange/simple
/deps/bin/pip install 'wheel==0.40.0'
/deps/bin/pip install 'setuptools<=67.7.2'`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_17_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_17_x86_64 the_dir/dist/*.whl
for f in the_dir/dist/*.whl; do
  if [ -f "$f" ] && [ "$(basename "$f")" != "the_artifact" ]; then
    mv "$f" "the_dir/dist/the_artifact"
  fi
  break
done`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux2014_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"LegacyPythonTag",
			&PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp37",
				ABITag:       "cp37m",
				Requirements: []string{"wheel==0.42.0", "setuptools<=67.7.2"},
				PlatformTag:  "musllinux_1_2_x86_64",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp37-cp37m" ]; then
  INTERPRETER="/opt/python/cp37-cp37m/bin/python"
else
  for dir in /opt/python/cp37*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp37' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
/deps/bin/pip install 'wheel==0.42.0'
/deps/bin/pip install 'setuptools<=67.7.2'`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat musllinux_1_2_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag musllinux_1_2_x86_64 the_dir/dist/*.whl
for f in the_dir/dist/*.whl; do
  if [ -f "$f" ] && [ "$(basename "$f")" != "the_artifact" ]; then
    mv "$f" "the_dir/dist/the_artifact"
  fi
  break
done`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/musllinux_1_2_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			"ModernWheel",
			&PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp310",
				ABITag:       "cp310",
				Requirements: []string{"wheel==0.38.0", "setuptools<=67.7.2"},
				PlatformTag:  "manylinux_2_17_x86_64",
			},
			rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp310-cp310" ]; then
  INTERPRETER="/opt/python/cp310-cp310/bin/python"
else
  for dir in /opt/python/cp310*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp310' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
/deps/bin/pip install 'wheel==0.38.0'
/deps/bin/pip install 'setuptools<=67.7.2'`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_17_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_17_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux2014_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := tc.strategy.GenerateFor(rebuild.Target{Ecosystem: rebuild.PyPI, Package: "the_package", Version: "the_version", Artifact: "the_artifact"}, rebuild.BuildEnv{HasRepo: true, TimewarpHost: "orange"})
			if err != nil {
				t.Fatalf("%s: Strategy%v.GenerateFor() failed unexpectedly: %v", tc.name, tc.strategy, err)
			}
			if diff := cmp.Diff(inst, tc.want); diff != "" {
				t.Errorf("Strategy%v.GenerateFor() returned diff (-got +want):\n%s", tc.strategy, diff)
			}
		})
	}
}

func TestPlatformWheelBuild_resolveBaseImage(t *testing.T) {
	const pinned = "quay.io/pypa/musllinux_1_2_x86_64:2026.03.20-1@sha256:5b6fe3ed82ff48748c5c528fc82e42674a5363890ed949923af65b0c5b5ef76c"
	tests := []struct {
		name        string
		platformTag string
		baseImage   string
		want        string
		wantErr     bool
	}{
		{
			name:        "manylinux2014",
			platformTag: "manylinux2014_x86_64",
			want:        "quay.io/pypa/manylinux2014_x86_64",
		},
		{
			name:        "manylinux_2_28",
			platformTag: "manylinux_2_28_x86_64",
			want:        "quay.io/pypa/manylinux_2_28_x86_64",
		},
		{
			name:        "manylinux_2_34",
			platformTag: "manylinux_2_34_x86_64",
			want:        "quay.io/pypa/manylinux_2_34_x86_64",
		},
		{
			name:        "compressed tag set with legacy baseline",
			platformTag: "manylinux1_x86_64.manylinux_2_28_x86_64",
			want:        "quay.io/pypa/manylinux_2_28_x86_64",
		},
		{
			name:        "musllinux_1_1",
			platformTag: "musllinux_1_1_x86_64",
			want:        "quay.io/pypa/musllinux_1_1_x86_64",
		},
		{
			name:        "musllinux_1_2",
			platformTag: "musllinux_1_2_x86_64",
			want:        "quay.io/pypa/musllinux_1_2_x86_64",
		},
		{
			name:        "explicit base image is used verbatim",
			platformTag: "musllinux_1_2_x86_64",
			baseImage:   pinned,
			want:        pinned,
		},
		{
			name:        "explicit base image skips platform tag selection",
			platformTag: "invalid_tag",
			baseImage:   pinned,
			want:        pinned,
		},
		{
			name:        "empty platform tag returns error",
			platformTag: "",
			wantErr:     true,
		},
		{
			name:        "invalid platform tag returns error",
			platformTag: "invalid_tag",
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &PlatformWheelBuild{PlatformTag: tt.platformTag, BaseImage: tt.baseImage}
			got, err := b.resolveBaseImage()
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveBaseImage() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("resolveBaseImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestABI3InterpreterDirs(t *testing.T) {
	tests := []struct {
		name      string
		pythonTag string
		reqs      []string
		want      []string
	}{
		{
			name:      "Cp36WithSetuptools75StartsAtCp38",
			pythonTag: "cp36",
			reqs:      []string{"setuptools==75.3.3"},
			want:      []string{"cp38-cp38", "cp39-cp39", "cp310-cp310", "cp311-cp311", "cp312-cp312", "cp313-cp313", "cp314-cp314"},
		},
		{
			name:      "Cp37WithSetuptoolsCeilingStartsAtCp37m",
			pythonTag: "cp37",
			reqs:      []string{"wheel==0.42.0", "setuptools<=67.7.2"},
			want:      []string{"cp37-cp37m", "cp38-cp38", "cp39-cp39", "cp310-cp310", "cp311-cp311", "cp312-cp312", "cp313-cp313", "cp314-cp314"},
		},
		{
			name:      "Cp37WithSetuptools82StartsAtCp39",
			pythonTag: "cp37",
			reqs:      []string{"setuptools==82.0.1"},
			want:      []string{"cp39-cp39", "cp310-cp310", "cp311-cp311", "cp312-cp312", "cp313-cp313", "cp314-cp314"},
		},
		{
			name:      "Cp38WithSetuptools80StartsAtCp39",
			pythonTag: "cp38",
			reqs:      []string{"setuptools==80.9.0", "wheel"},
			want:      []string{"cp39-cp39", "cp310-cp310", "cp311-cp311", "cp312-cp312", "cp313-cp313", "cp314-cp314"},
		},
		{
			name:      "Cp36WithNoSetuptoolsFloorStartsAtCp36m",
			pythonTag: "cp36",
			reqs:      []string{"setuptools<=56.2.0"},
			want:      []string{"cp36-cp36m", "cp37-cp37m", "cp38-cp38", "cp39-cp39", "cp310-cp310", "cp311-cp311", "cp312-cp312", "cp313-cp313", "cp314-cp314"},
		},
		{
			name:      "Cp39ReturnsNil",
			pythonTag: "cp39",
			reqs:      []string{"setuptools==80.9.0"},
			want:      nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := abi3InterpreterDirs(tc.pythonTag, tc.reqs)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("abi3InterpreterDirs(%q, %v) diff (-want +got):\n%s", tc.pythonTag, tc.reqs, diff)
			}
		})
	}
}

func TestPlatformWheelBuildABI3AndFreethreaded(t *testing.T) {
	regTime := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	loc := rebuild.Location{Repo: "https://github.com/example/pkg", Ref: "deadbeef"}
	target := rebuild.Target{Ecosystem: rebuild.PyPI, Package: "pkg", Version: "1.0.0", Artifact: "pkg-1.0.0.whl"}
	be := rebuild.BuildEnv{HasRepo: true, TimewarpHost: "localhost:8081"}
	tests := []struct {
		name     string
		strategy *PlatformWheelBuild
		wantDeps string
		wantBld  string
	}{
		{
			name: "Cp38ABI3WithSetuptools80UsesCandidateLoopAndPyLimitedAPI",
			strategy: &PlatformWheelBuild{
				Location:     loc,
				PythonTag:    "cp38",
				ABITag:       "abi3",
				Requirements: []string{"setuptools==80.9.0"},
				PlatformTag:  "manylinux_2_34_x86_64",
				RegistryTime: regTime,
			},
			wantDeps: textwrap.Dedent(`
				INTERPRETER=""
				for dir in cp39-cp39 cp310-cp310 cp311-cp311 cp312-cp312 cp313-cp313 cp314-cp314; do
				  if [ -d "/opt/python/$dir" ]; then
				    INTERPRETER="/opt/python/$dir/bin/python"
				    break
				  fi
				done
				if [ -z "$INTERPRETER" ]; then
				  echo "Error: Requested Python tag 'cp38' not found in /opt/python" >&2
				  exit 1
				fi
				$INTERPRETER -m venv /deps
				/deps/bin/pip install build wheel auditwheel
				export PIP_INDEX_URL=http://pypi:2026-01-01T00:00:00Z@localhost:8081/simple
				/deps/bin/pip install 'setuptools==80.9.0'`)[1:],
			wantBld: textwrap.Dedent(`
				printf '[bdist_wheel]\npy_limited_api = cp38\n' >~/.pydistutils.cfg
				/deps/bin/python3 -m build --wheel -n
				mkdir -p dist/repaired
				AUDITWHEEL="/deps/bin/auditwheel"
				if [ ! -x "$AUDITWHEEL" ]; then
				  AUDITWHEEL="auditwheel"
				fi
				if $AUDITWHEEL repair dist/*.whl --plat manylinux_2_34_x86_64 -w dist/repaired/; then
				  rm -f dist/*.whl
				  mv dist/repaired/*.whl dist/
				fi
				rm -rf dist/repaired
				/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_34_x86_64 dist/*.whl`)[1:],
		},
		{
			name: "Cp39ABI3UsesDefaultTagLookup",
			strategy: &PlatformWheelBuild{
				Location:     loc,
				PythonTag:    "cp39",
				ABITag:       "abi3",
				Requirements: []string{"setuptools==80.9.0"},
				PlatformTag:  "musllinux_1_2_x86_64",
				RegistryTime: regTime,
			},
			wantDeps: textwrap.Dedent(`
				INTERPRETER=""
				if [ -d "/opt/python/cp39-abi3" ]; then
				  INTERPRETER="/opt/python/cp39-abi3/bin/python"
				else
				  for dir in /opt/python/cp39*; do
				    if [ -d "$dir" ]; then
				      INTERPRETER="$dir/bin/python"
				      break
				    fi
				  done
				fi
				if [ -z "$INTERPRETER" ]; then
				  echo "Error: Requested Python tag 'cp39' not found in /opt/python" >&2
				  exit 1
				fi
				$INTERPRETER -m venv /deps
				/deps/bin/pip install build wheel auditwheel
				export PIP_INDEX_URL=http://pypi:2026-01-01T00:00:00Z@localhost:8081/simple
				/deps/bin/pip install 'setuptools==80.9.0'`)[1:],
			wantBld: textwrap.Dedent(`
				/deps/bin/python3 -m build --wheel -n
				mkdir -p dist/repaired
				AUDITWHEEL="/deps/bin/auditwheel"
				if [ ! -x "$AUDITWHEEL" ]; then
				  AUDITWHEEL="auditwheel"
				fi
				if $AUDITWHEEL repair dist/*.whl --plat musllinux_1_2_x86_64 -w dist/repaired/; then
				  rm -f dist/*.whl
				  mv dist/repaired/*.whl dist/
				fi
				rm -rf dist/repaired
				/deps/bin/python3 -m wheel tags --remove --platform-tag musllinux_1_2_x86_64 dist/*.whl`)[1:],
		},
		{
			name: "Cp313tUsesExactDirOrUVFallback",
			strategy: &PlatformWheelBuild{
				Location:     loc,
				PythonTag:    "cp313",
				ABITag:       "cp313t",
				Requirements: []string{"setuptools==80.9.0"},
				PlatformTag:  "musllinux_1_2_x86_64",
				RegistryTime: regTime,
			},
			wantDeps: textwrap.Dedent(`
				if [ -d "/opt/python/cp313-cp313t" ]; then
				  /opt/python/cp313-cp313t/bin/python -m venv /deps
				else
				  uv venv /deps --seed --python 3.13t
				fi
				/deps/bin/pip install build wheel auditwheel
				export PIP_INDEX_URL=http://pypi:2026-01-01T00:00:00Z@localhost:8081/simple
				/deps/bin/pip install 'setuptools==80.9.0'`)[1:],
			wantBld: textwrap.Dedent(`
				/deps/bin/python3 -m build --wheel -n
				mkdir -p dist/repaired
				AUDITWHEEL="/deps/bin/auditwheel"
				if [ ! -x "$AUDITWHEEL" ]; then
				  AUDITWHEEL="auditwheel"
				fi
				if $AUDITWHEEL repair dist/*.whl --plat musllinux_1_2_x86_64 -w dist/repaired/; then
				  rm -f dist/*.whl
				  mv dist/repaired/*.whl dist/
				fi
				rm -rf dist/repaired
				/deps/bin/python3 -m wheel tags --remove --platform-tag musllinux_1_2_x86_64 dist/*.whl`)[1:],
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := tc.strategy.GenerateFor(target, be)
			if err != nil {
				t.Fatalf("GenerateFor() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantDeps, inst.Deps); diff != "" {
				t.Errorf("GenerateFor() Deps diff (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantBld, inst.Build); diff != "" {
				t.Errorf("GenerateFor() Build diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPlatformWheelBuildEnv(t *testing.T) {
	defaultLocation := rebuild.Location{
		Dir:  "the_dir",
		Ref:  "the_ref",
		Repo: "the_repo",
	}
	target := rebuild.Target{
		Ecosystem: rebuild.PyPI,
		Package:   "the_package",
		Version:   "1.0.0",
		Artifact:  "the_package-1.0.0-cp312-cp312-manylinux_2_28_x86_64.whl",
	}
	buildEnv := rebuild.BuildEnv{HasRepo: true, TimewarpHost: "orange"}
	tests := []struct {
		name      string
		env       []string
		wantBuild string
	}{
		{
			name: "NoEnv",
			env:  nil,
			wantBuild: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 the_dir/dist/*.whl`,
		},
		{
			name: "SingleEnvVar",
			env:  []string{"MYPY_USE_MYPYC=1"},
			wantBuild: `export MYPY_USE_MYPYC=1
/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 the_dir/dist/*.whl`,
		},
		{
			name: "MultipleEnvVars",
			env:  []string{"CHARSET_NORMALIZER_USE_CYTHON=1", "CHARSET_NORMALIZER_CYTHON_ABI3=1"},
			wantBuild: `export CHARSET_NORMALIZER_USE_CYTHON=1
export CHARSET_NORMALIZER_CYTHON_ABI3=1
/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 the_dir/dist/*.whl`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp312",
				ABITag:       "cp312",
				PlatformTag:  "manylinux_2_28_x86_64",
				Requirements: []string{"setuptools==82.0.1"},
				Env:          tc.env,
			}
			inst, err := s.GenerateFor(target, buildEnv)
			if err != nil {
				t.Fatalf("GenerateFor() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.wantBuild, inst.Build); diff != "" {
				t.Errorf("GenerateFor() Build diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNeedsVenvOnPath(t *testing.T) {
	tests := []struct {
		name string
		reqs []string
		want string
	}{
		{"UvBuild", []string{"uv-build==0.10.0"}, "1"},
		{"MesonPython", []string{"meson-python>=0.17.1"}, "1"},
		{"ScikitBuildCore", []string{"scikit-build-core==0.12.2"}, "1"},
		{"Setuptools", []string{"setuptools>=70.1.0"}, ""},
		{"Empty", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsVenvOnPath(tc.reqs); got != tc.want {
				t.Errorf("needsVenvOnPath(%v) = %q, want %q", tc.reqs, got, tc.want)
			}
		})
	}
}

func TestPlatformWheelBuildVenvOnPath(t *testing.T) {
	defaultLocation := rebuild.Location{
		Dir:  "the_dir",
		Ref:  "the_ref",
		Repo: "the_repo",
	}
	tests := []struct {
		name     string
		strategy rebuild.Strategy
		want     rebuild.Instructions
	}{
		{
			name: "SetuptoolsOmitsVenvOnPath",
			strategy: &PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp312",
				ABITag:       "cp312",
				PlatformTag:  "manylinux_2_28_x86_64",
				Requirements: []string{"setuptools>=70.1.0"},
			},
			want: rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp312-cp312" ]; then
  INTERPRETER="/opt/python/cp312-cp312/bin/python"
else
  for dir in /opt/python/cp312*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp312' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
/deps/bin/pip install 'setuptools>=70.1.0'`,
				Build: `/deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux_2_28_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			name: "MesonPythonPutsVenvOnPath",
			strategy: &PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp312",
				ABITag:       "cp312",
				PlatformTag:  "manylinux_2_28_x86_64",
				Requirements: []string{"meson-python>=0.17.1", "ninja"},
			},
			want: rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp312-cp312" ]; then
  INTERPRETER="/opt/python/cp312-cp312/bin/python"
else
  for dir in /opt/python/cp312*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp312' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
/deps/bin/pip install 'meson-python>=0.17.1'
/deps/bin/pip install 'ninja'`,
				Build: `PATH=/deps/bin/:$PATH /deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux_2_28_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
		{
			name: "ScikitBuildCorePutsVenvOnPath",
			strategy: &PlatformWheelBuild{
				Location:     defaultLocation,
				PythonTag:    "cp312",
				ABITag:       "cp312",
				PlatformTag:  "manylinux_2_28_x86_64",
				Requirements: []string{"scikit-build-core==0.12.2", "ninja"},
			},
			want: rebuild.Instructions{
				Location: defaultLocation,
				Source:   defaultSource,
				Deps: `INTERPRETER=""
if [ -d "/opt/python/cp312-cp312" ]; then
  INTERPRETER="/opt/python/cp312-cp312/bin/python"
else
  for dir in /opt/python/cp312*; do
    if [ -d "$dir" ]; then
      INTERPRETER="$dir/bin/python"
      break
    fi
  done
fi
if [ -z "$INTERPRETER" ]; then
  echo "Error: Requested Python tag 'cp312' not found in /opt/python" >&2
  exit 1
fi
$INTERPRETER -m venv /deps
/deps/bin/pip install build wheel auditwheel
/deps/bin/pip install 'scikit-build-core==0.12.2'
/deps/bin/pip install 'ninja'`,
				Build: `PATH=/deps/bin/:$PATH /deps/bin/python3 -m build --wheel -n the_dir
mkdir -p the_dir/dist/repaired
AUDITWHEEL="/deps/bin/auditwheel"
if [ ! -x "$AUDITWHEEL" ]; then
  AUDITWHEEL="auditwheel"
fi
if $AUDITWHEEL repair the_dir/dist/*.whl --plat manylinux_2_28_x86_64 -w the_dir/dist/repaired/; then
  rm -f the_dir/dist/*.whl
  mv the_dir/dist/repaired/*.whl the_dir/dist/
fi
rm -rf the_dir/dist/repaired
/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 the_dir/dist/*.whl`,
				Requires: rebuild.RequiredEnv{
					BaseImage:  "quay.io/pypa/manylinux_2_28_x86_64",
					SystemDeps: []string{"git"},
				},
				OutputPath: "the_dir/dist/the_artifact",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := tc.strategy.GenerateFor(
				rebuild.Target{Ecosystem: rebuild.PyPI, Package: "the_package", Version: "the_version", Artifact: "the_artifact"},
				rebuild.BuildEnv{HasRepo: true, TimewarpHost: "orange"},
			)
			if err != nil {
				t.Fatalf("GenerateFor() failed unexpectedly: %v", err)
			}
			if diff := cmp.Diff(tc.want, inst); diff != "" {
				t.Errorf("GenerateFor() diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPlatformWheelBuildRustAndMaturin(t *testing.T) {
	defaultTarget := rebuild.Target{
		Ecosystem: rebuild.PyPI,
		Package:   "foo",
		Version:   "1.0.0",
		Artifact:  "foo-1.0.0-cp312-cp312-manylinux_2_28_x86_64.whl",
	}
	be := rebuild.BuildEnv{
		HasRepo:      true,
		TimewarpHost: "localhost:8080",
	}
	expectedSource := textwrap.Dedent(`
		git checkout --force 'deadbeef'
		if [ -f .gitmodules ]; then
		  git config --global url."https://github.com/".insteadOf "git@github.com:" || true
		  git config --global url."https://gitlab.com/".insteadOf "git@gitlab.com:" || true
		  git config --global url."https://bitbucket.org/".insteadOf "git@bitbucket.org:" || true
		  git config --global url."https://codeberg.org/".insteadOf "git@codeberg.org:" || true
		  git config --global url."https://".insteadOf "git://" || true
		  git submodule sync --recursive || true
		  GIT_TERMINAL_PROMPT=0 git submodule update --init || true
		  GIT_TERMINAL_PROMPT=0 git submodule foreach --recursive 'git submodule sync || true; GIT_TERMINAL_PROMPT=0 git submodule update --init || true' || true
		fi`)[1:]
	testCases := []struct {
		name     string
		strategy *PlatformWheelBuild
		want     rebuild.Instructions
	}{
		{
			name: "SetuptoolsRustWheel",
			strategy: &PlatformWheelBuild{
				Location: rebuild.Location{
					Repo: "https://github.com/example/foo",
					Ref:  "deadbeef",
				},
				PythonTag:    "cp312",
				ABITag:       "cp312",
				PlatformTag:  "manylinux_2_28_x86_64",
				Requirements: []string{"wheel==0.45.1", "setuptools==75.0.0", "setuptools-rust"},
				RustVersion:  "1.95.0",
				RegistryTime: time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC),
			},
			want: rebuild.Instructions{
				Location: rebuild.Location{
					Repo: "https://github.com/example/foo",
					Ref:  "deadbeef",
				},
				Source: expectedSource,
				Deps: textwrap.Dedent(`
					INTERPRETER=""
					if [ -d "/opt/python/cp312-cp312" ]; then
					  INTERPRETER="/opt/python/cp312-cp312/bin/python"
					else
					  for dir in /opt/python/cp312*; do
					    if [ -d "$dir" ]; then
					      INTERPRETER="$dir/bin/python"
					      break
					    fi
					  done
					fi
					if [ -z "$INTERPRETER" ]; then
					  echo "Error: Requested Python tag 'cp312' not found in /opt/python" >&2
					  exit 1
					fi
					$INTERPRETER -m venv /deps
					/deps/bin/pip install build wheel auditwheel
					export PIP_INDEX_URL=http://pypi:2026-05-01T00:00:00Z@localhost:8080/simple
					/deps/bin/pip install 'wheel==0.45.1'
					/deps/bin/pip install 'setuptools==75.0.0'
					/deps/bin/pip install 'setuptools-rust'
					export CARGO_HOME=/root/.cargo RUSTUP_HOME=/root/.rustup
					curl --proto '=https' --tlsv1.2 -sSfL -o /tmp/rustup-init https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu/rustup-init
					curl --proto '=https' --tlsv1.2 -sSfL https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu/rustup-init.sha256 | awk '{print $1 "  /tmp/rustup-init"}' | sha256sum -c -
					chmod +x /tmp/rustup-init
					/tmp/rustup-init -y --no-modify-path --profile minimal --default-host x86_64-unknown-linux-gnu --default-toolchain 1.95.0
					rm -f /tmp/rustup-init`)[1:],
				Build: textwrap.Dedent(`
					export CARGO_HOME=/root/.cargo RUSTUP_HOME=/root/.rustup RUSTUP_TOOLCHAIN=1.95.0 PATH=/root/.cargo/bin:$PATH CIBW_BUILD=1
					/deps/bin/python3 -m build --wheel -n
					mkdir -p dist/repaired
					AUDITWHEEL="/deps/bin/auditwheel"
					if [ ! -x "$AUDITWHEEL" ]; then
					  AUDITWHEEL="auditwheel"
					fi
					if $AUDITWHEEL repair dist/*.whl --plat manylinux_2_28_x86_64 -w dist/repaired/; then
					  rm -f dist/*.whl
					  mv dist/repaired/*.whl dist/
					fi
					rm -rf dist/repaired
					/deps/bin/python3 -m wheel tags --remove --platform-tag manylinux_2_28_x86_64 dist/*.whl`)[1:],
				OutputPath: "dist/foo-1.0.0-cp312-cp312-manylinux_2_28_x86_64.whl",
				Requires: rebuild.RequiredEnv{
					BaseImage:  platform.ImageManylinux2_28X86_64,
					SystemDeps: []string{"git"},
				},
			},
		},
		{
			name: "MaturinWheelWithSubdirAndFeatures",
			strategy: &PlatformWheelBuild{
				Location: rebuild.Location{
					Repo: "https://github.com/example/hypothesis",
					Dir:  "hypothesis",
					Ref:  "deadbeef",
				},
				PythonTag:    "cp310",
				ABITag:       "abi3",
				PlatformTag:  "manylinux_2_17_x86_64.manylinux2014_x86_64",
				Requirements: []string{"maturin==1.15.0"},
				RustVersion:  "1.98.0",
				Maturin: &MaturinBuild{
					Policy:   "manylinux_2_17",
					Features: []string{"abi3"},
				},
				RegistryTime: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
			},
			want: rebuild.Instructions{
				Location: rebuild.Location{
					Repo: "https://github.com/example/hypothesis",
					Dir:  "hypothesis",
					Ref:  "deadbeef",
				},
				Source: expectedSource,
				Deps: textwrap.Dedent(`
					INTERPRETER=""
					if [ -d "/opt/python/cp310-abi3" ]; then
					  INTERPRETER="/opt/python/cp310-abi3/bin/python"
					else
					  for dir in /opt/python/cp310*; do
					    if [ -d "$dir" ]; then
					      INTERPRETER="$dir/bin/python"
					      break
					    fi
					  done
					fi
					if [ -z "$INTERPRETER" ]; then
					  echo "Error: Requested Python tag 'cp310' not found in /opt/python" >&2
					  exit 1
					fi
					$INTERPRETER -m venv /deps
					/deps/bin/pip install build wheel auditwheel
					export PIP_INDEX_URL=http://pypi:2026-09-01T00:00:00Z@localhost:8080/simple
					/deps/bin/pip install 'maturin==1.15.0'
					export CARGO_HOME=/root/.cargo RUSTUP_HOME=/root/.rustup
					curl --proto '=https' --tlsv1.2 -sSfL -o /tmp/rustup-init https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu/rustup-init
					curl --proto '=https' --tlsv1.2 -sSfL https://static.rust-lang.org/rustup/dist/x86_64-unknown-linux-gnu/rustup-init.sha256 | awk '{print $1 "  /tmp/rustup-init"}' | sha256sum -c -
					chmod +x /tmp/rustup-init
					/tmp/rustup-init -y --no-modify-path --profile minimal --default-host x86_64-unknown-linux-gnu --default-toolchain 1.98.0
					rm -f /tmp/rustup-init`)[1:],
				Build: textwrap.Dedent(`
					export CARGO_HOME=/root/.cargo RUSTUP_HOME=/root/.rustup RUSTUP_TOOLCHAIN=1.98.0 PATH=/root/.cargo/bin:$PATH CIBW_BUILD=1
					(cd hypothesis && /deps/bin/maturin build --release -i /deps/bin/python --compatibility manylinux_2_17 --features abi3 --out dist)`)[1:],
				OutputPath: "hypothesis/dist/foo-1.0.0-cp312-cp312-manylinux_2_28_x86_64.whl",
				Requires: rebuild.RequiredEnv{
					BaseImage:  platform.ImageManylinux2014X86_64,
					SystemDeps: []string{"git"},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := tc.strategy.GenerateFor(defaultTarget, be)
			if err != nil {
				t.Fatalf("GenerateFor() failed: %v", err)
			}
			if diff := cmp.Diff(tc.want, inst); diff != "" {
				t.Errorf("GenerateFor() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPlatformWheelBuildSanitizeSetupCfg(t *testing.T) {
	defaultTarget := rebuild.Target{
		Ecosystem: rebuild.PyPI,
		Package:   "SQLAlchemy",
		Version:   "2.0.52",
		Artifact:  "sqlalchemy-2.0.52-cp312-cp312-musllinux_1_2_x86_64.whl",
	}
	defaultEnv := rebuild.BuildEnv{
		TimewarpHost: "localhost:8081",
	}
	defaultTime := time.Date(2026, time.August, 11, 21, 16, 59, 0, time.UTC)
	tests := []struct {
		name      string
		strategy  *PlatformWheelBuild
		wantBuild string
	}{
		{
			name: "DefaultWithoutSanitize",
			strategy: &PlatformWheelBuild{
				Location: rebuild.Location{
					Repo: "https://github.com/sqlalchemy/sqlalchemy",
					Ref:  "190990768104d13e322d9c07261cb2778e7f256a",
				},
				PythonTag:    "cp312",
				ABITag:       "cp312",
				PlatformTag:  "musllinux_1_2_x86_64",
				Requirements: []string{"setuptools==84.0.0"},
				RegistryTime: defaultTime,
			},
			wantBuild: textwrap.Dedent(`
				/deps/bin/python3 -m build --wheel -n
				mkdir -p dist/repaired
				AUDITWHEEL="/deps/bin/auditwheel"
				if [ ! -x "$AUDITWHEEL" ]; then
				  AUDITWHEEL="auditwheel"
				fi
				if $AUDITWHEEL repair dist/*.whl --plat musllinux_1_2_x86_64 -w dist/repaired/; then
				  rm -f dist/*.whl
				  mv dist/repaired/*.whl dist/
				fi
				rm -rf dist/repaired
				/deps/bin/python3 -m wheel tags --remove --platform-tag musllinux_1_2_x86_64 dist/*.whl`)[1:],
		},
		{
			name: "WithSanitizeSetupCfgRoot",
			strategy: &PlatformWheelBuild{
				Location: rebuild.Location{
					Repo: "https://github.com/sqlalchemy/sqlalchemy",
					Ref:  "190990768104d13e322d9c07261cb2778e7f256a",
				},
				PythonTag:        "cp312",
				ABITag:           "cp312",
				PlatformTag:      "musllinux_1_2_x86_64",
				Requirements:     []string{"setuptools==84.0.0"},
				RegistryTime:     defaultTime,
				SanitizeSetupCfg: true,
			},
			wantBuild: textwrap.Dedent(`
				sed -i '/^\[egg_info\]/,/^\[/ { /^tag_build/d; /^tag_date/d; }' setup.cfg
				/deps/bin/python3 -m build --wheel -n
				mkdir -p dist/repaired
				AUDITWHEEL="/deps/bin/auditwheel"
				if [ ! -x "$AUDITWHEEL" ]; then
				  AUDITWHEEL="auditwheel"
				fi
				if $AUDITWHEEL repair dist/*.whl --plat musllinux_1_2_x86_64 -w dist/repaired/; then
				  rm -f dist/*.whl
				  mv dist/repaired/*.whl dist/
				fi
				rm -rf dist/repaired
				/deps/bin/python3 -m wheel tags --remove --platform-tag musllinux_1_2_x86_64 dist/*.whl`)[1:],
		},
		{
			name: "WithSanitizeSetupCfgSubdir",
			strategy: &PlatformWheelBuild{
				Location: rebuild.Location{
					Repo: "https://github.com/sqlalchemy/sqlalchemy",
					Ref:  "190990768104d13e322d9c07261cb2778e7f256a",
					Dir:  "python",
				},
				PythonTag:        "cp312",
				ABITag:           "cp312",
				PlatformTag:      "musllinux_1_2_x86_64",
				Requirements:     []string{"setuptools==84.0.0"},
				RegistryTime:     defaultTime,
				SanitizeSetupCfg: true,
			},
			wantBuild: textwrap.Dedent(`
				sed -i '/^\[egg_info\]/,/^\[/ { /^tag_build/d; /^tag_date/d; }' python/setup.cfg
				/deps/bin/python3 -m build --wheel -n python
				mkdir -p python/dist/repaired
				AUDITWHEEL="/deps/bin/auditwheel"
				if [ ! -x "$AUDITWHEEL" ]; then
				  AUDITWHEEL="auditwheel"
				fi
				if $AUDITWHEEL repair python/dist/*.whl --plat musllinux_1_2_x86_64 -w python/dist/repaired/; then
				  rm -f python/dist/*.whl
				  mv python/dist/repaired/*.whl python/dist/
				fi
				rm -rf python/dist/repaired
				/deps/bin/python3 -m wheel tags --remove --platform-tag musllinux_1_2_x86_64 python/dist/*.whl`)[1:],
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := tc.strategy.GenerateFor(defaultTarget, defaultEnv)
			if err != nil {
				t.Fatalf("GenerateFor() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantBuild, inst.Build); diff != "" {
				t.Errorf("GenerateFor().Build diff (-want +got):\n%s", diff)
			}
		})
	}
}
