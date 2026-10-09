// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"strings"
	"testing"

	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
)

func TestPlatformWheelBuild_AuditwheelUsage(t *testing.T) {
	b := &PlatformWheelBuild{
		PythonTag:   "cp313",
		ABITag:      "cp313",
		PlatformTag: "musllinux_1_2_x86_64",
	}
	insts, err := b.GenerateFor(rebuild.Target{}, rebuild.BuildEnv{})
	if err != nil {
		t.Fatalf("GenerateFor failed: %v", err)
	}

	// Verify that pip install in deps does not install auditwheel or wheel from PyPI before timewarp
	if strings.Contains(insts.Deps, "pip install build wheel auditwheel") || strings.Contains(insts.Deps, "pip install build auditwheel") {
		t.Errorf("deps step unexpectedly installed auditwheel from PyPI before timewarp: %s", insts.Deps)
	}
	if !strings.Contains(insts.Deps, "pip install build\n") && !strings.Contains(insts.Deps, "pip install build ") {
		t.Errorf("deps step expected to install build: %s", insts.Deps)
	}

	// Verify that build step resolves AUDITWHEEL and exports PATH
	if !strings.Contains(insts.Build, `AUDITWHEEL="/deps/bin/auditwheel"`) {
		t.Errorf("build step expected to check locator auditwheel: %s", insts.Build)
	}
	if !strings.Contains(insts.Build, `export PATH="/deps/bin/:$PATH"`) {
		t.Errorf("build step expected to export locator to PATH: %s", insts.Build)
	}

	// Verify that setting AuditwheelVersion installs auditwheel into tool venv
	bWithVer := &PlatformWheelBuild{
		PythonTag:         "cp39",
		ABITag:            "cp39",
		PlatformTag:       "musllinux_1_2_x86_64",
		AuditwheelVersion: "==6.6.0",
	}
	instsWithVer, err := bWithVer.GenerateFor(rebuild.Target{}, rebuild.BuildEnv{})
	if err != nil {
		t.Fatalf("GenerateFor failed: %v", err)
	}
	if !strings.Contains(instsWithVer.Deps, "pip install 'auditwheel==6.6.0'") {
		t.Errorf("deps step expected to install auditwheel==6.6.0: %s", instsWithVer.Deps)
	}
}
