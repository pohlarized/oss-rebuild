// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"archive/zip"
	"bytes"
	"context"
	"debug/elf"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/oss-rebuild/internal/gitx/gitxtest"
	"github.com/google/oss-rebuild/internal/httpx/httpxtest"
	"github.com/google/oss-rebuild/pkg/archive"
	"github.com/google/oss-rebuild/pkg/archive/archivetest"
	"github.com/google/oss-rebuild/pkg/rebuild/pypi/sysdeps"
	"github.com/google/oss-rebuild/pkg/rebuild/rebuild"
	pypireg "github.com/google/oss-rebuild/pkg/registry/pypi"
)

// makeTestCommentELF builds a minimal 64-bit little-endian ELF shared object
// containing a .comment section with the given NUL-separated strings.
func makeTestCommentELF(comments []string) []byte {
	shstrtab := []byte("\x00.shstrtab\x00.comment\x00")
	commentData := []byte(strings.Join(comments, "\x00") + "\x00")
	const ehdrSize = 64
	const shentSize = 64
	const numShdr = 3
	shstrtabOff := uint64(ehdrSize)
	commentOff := shstrtabOff + uint64(len(shstrtab))
	shoff := (commentOff + uint64(len(commentData)) + 7) &^ 7
	totalSize := shoff + numShdr*shentSize
	buf := make([]byte, totalSize)
	copy(buf[0:4], "\x7fELF")
	buf[4] = byte(elf.ELFCLASS64)
	buf[5] = byte(elf.ELFDATA2LSB)
	buf[6] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(buf[16:18], uint16(elf.ET_DYN))
	binary.LittleEndian.PutUint16(buf[18:20], uint16(elf.EM_X86_64))
	binary.LittleEndian.PutUint32(buf[20:24], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint64(buf[40:48], shoff)
	binary.LittleEndian.PutUint16(buf[52:54], ehdrSize)
	binary.LittleEndian.PutUint16(buf[58:60], shentSize)
	binary.LittleEndian.PutUint16(buf[60:62], numShdr)
	binary.LittleEndian.PutUint16(buf[62:64], 1)
	copy(buf[shstrtabOff:], shstrtab)
	copy(buf[commentOff:], commentData)
	sh1 := buf[shoff+shentSize : shoff+2*shentSize]
	binary.LittleEndian.PutUint32(sh1[0:4], 1)
	binary.LittleEndian.PutUint32(sh1[4:8], uint32(elf.SHT_STRTAB))
	binary.LittleEndian.PutUint64(sh1[24:32], shstrtabOff)
	binary.LittleEndian.PutUint64(sh1[32:40], uint64(len(shstrtab)))
	binary.LittleEndian.PutUint64(sh1[48:56], 1)
	sh2 := buf[shoff+2*shentSize : shoff+3*shentSize]
	binary.LittleEndian.PutUint32(sh2[0:4], 11)
	binary.LittleEndian.PutUint32(sh2[4:8], uint32(elf.SHT_PROGBITS))
	binary.LittleEndian.PutUint64(sh2[24:32], commentOff)
	binary.LittleEndian.PutUint64(sh2[32:40], uint64(len(commentData)))
	binary.LittleEndian.PutUint64(sh2[48:56], 1)
	return buf
}

func makeWheelZipWithFiles(t *testing.T, files map[string][]byte) *zip.Reader {
	t.Helper()
	var entries []archive.ZipEntry
	for name, body := range files {
		entries = append(entries, archive.ZipEntry{
			FileHeader: &zip.FileHeader{Name: name},
			Body:       body,
		})
	}
	buf, err := archivetest.ZipFile(entries)
	if err != nil {
		t.Fatalf("ZipFile(): %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("NewReader(): %v", err)
	}
	return zr
}

func TestInspectWheelRustELF(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string][]byte
		wantVer string
		wantLLD bool
	}{
		{
			name: "RustcAndBundledRustLLD",
			files: map[string][]byte{
				"pkg/_core.abi3.so": makeTestCommentELF([]string{
					"GCC: (GNU) 14.2.1",
					"rustc version 1.98.0 (88d9e12ae 2026-08-18)",
					"Linker: LLD 22.1.0 (/checkout/src/llvm-project 50ac70596)",
				}),
			},
			wantVer: "1.98.0",
			wantLLD: false,
		},
		{
			name: "RustcAndSystemLLD",
			files: map[string][]byte{
				"ddtrace/internal/_native.cpython-312-x86_64-linux-musl.so": makeTestCommentELF([]string{
					"rustc version 1.97.1 (72b8501c5 2026-07-15)",
					"Linker: LLD 20.1.8",
				}),
			},
			wantVer: "1.97.1",
			wantLLD: true,
		},
		{
			name: "SkipsWheelLibsDir",
			files: map[string][]byte{
				"pkg.libs/libvendored.so": makeTestCommentELF([]string{
					"rustc version 1.99.0 (abcdef 2026-10-01)",
				}),
				"pkg/_ext.so": makeTestCommentELF([]string{
					"rustc version 1.94.0 (123456 2026-03-05)",
				}),
			},
			wantVer: "1.94.0",
			wantLLD: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			zr := makeWheelZipWithFiles(t, tc.files)
			gotVer, gotLLD := inspectWheelRustELF(zr)
			if gotVer != tc.wantVer || gotLLD != tc.wantLLD {
				t.Errorf("inspectWheelRustELF() = (%q, %v), want (%q, %v)", gotVer, gotLLD, tc.wantVer, tc.wantLLD)
			}
		})
	}
}

func TestInferRustVersionFallbacks(t *testing.T) {
	repo := must(gitxtest.CreateRepoFromYAML(`commits:
  - id: with-toolchain
    files:
      rust-toolchain.toml: |
        [toolchain]
        channel = "1.96.0"
  - id: no-toolchain
    files:
      rust-toolchain.toml: |
        [toolchain]
        channel = "stable"
`, nil))
	commitWithToolchain, err := repo.Repository.CommitObject(repo.Commits["with-toolchain"])
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	treeWithToolchain, err := commitWithToolchain.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	commitNoToolchain, err := repo.Repository.CommitObject(repo.Commits["no-toolchain"])
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	treeNoToolchain, err := commitNoToolchain.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	emptyZr := makeWheelZipWithFiles(t, nil)
	if got, _ := inferRustVersion(emptyZr, treeWithToolchain, "", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)); got != "1.96.0" {
		t.Errorf("inferRustVersion(toolchain file) = %q, want 1.96.0", got)
	}
	if got, _ := inferRustVersion(emptyZr, treeNoToolchain, "", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)); got != "1.98.0" {
		t.Errorf("inferRustVersion(registryTime fallback) = %q, want 1.98.0", got)
	}
}

func TestInferMaturinBuild(t *testing.T) {
	tests := []struct {
		name     string
		repoYAML string
		dir      string
		tags     WheelTags
		want     *MaturinBuild
	}{
		{
			name: "DatefinderDefaultABI3InCargoToml",
			repoYAML: `commits:
  - id: head
    files:
      pyproject.toml: |
        [build-system]
        requires = ["maturin>=1.0,<2.0"]
        build-backend = "maturin"
      Cargo.toml: |
        [package]
        name = "datefinder"
        version = "1.0.0"
        [dependencies]
        pyo3 = { version = "0.22.6", features = ["extension-module", "abi3-py39"] }
`,
			tags: WheelTags{
				Python:   "cp39",
				ABI:      "abi3",
				Platform: "manylinux_2_17_x86_64.manylinux2014_x86_64",
			},
			want: &MaturinBuild{
				Policy: "manylinux_2_17",
			},
		},
		{
			name: "HypothesisCrateABI3AndABI3TFeatures",
			repoYAML: `commits:
  - id: head
    files:
      hypothesis/pyproject.toml: |
        [build-system]
        requires = ["maturin>=1.14,<2.0"]
        build-backend = "maturin"
        [tool.maturin]
        manifest-path = "rust/Cargo.toml"
      hypothesis/rust/Cargo.toml: |
        [package]
        name = "conjecture_rust"
        version = "0.1.0"
        [features]
        abi3 = ["pyo3/abi3-py310"]
        abi3t = ["pyo3/abi3t-py315"]
        [dependencies]
        pyo3 = { version = "0.28.2", features = ["extension-module"] }
`,
			dir: "hypothesis",
			tags: WheelTags{
				Python:   "cp315",
				ABI:      "abi3.abi3t",
				Platform: "manylinux_2_17_x86_64.manylinux2014_x86_64",
			},
			want: &MaturinBuild{
				Policy:   "manylinux_2_17",
				Features: []string{"abi3t"},
			},
		},
		{
			name: "LitellmDirectPyo3ABI3Feature",
			repoYAML: `commits:
  - id: head
    files:
      pyproject.toml: |
        [build-system]
        requires = ["maturin>=1.0,<2.0"]
        build-backend = "maturin"
        [tool.maturin]
        manifest-path = "rust/litellm_core/Cargo.toml"
      rust/litellm_core/Cargo.toml: |
        [package]
        name = "litellm_core"
        version = "1.99.0"
        [dependencies]
        pyo3 = { workspace = true, features = ["extension-module"] }
`,
			tags: WheelTags{
				Python:   "cp310",
				ABI:      "abi3",
				Platform: "manylinux_2_28_x86_64",
			},
			want: &MaturinBuild{
				Policy:   "manylinux_2_28",
				Features: []string{"pyo3/abi3-py310"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := must(gitxtest.CreateRepoFromYAML(tc.repoYAML, nil))
			commit, err := repo.Repository.CommitObject(repo.Commits["head"])
			if err != nil {
				t.Fatalf("CommitObject: %v", err)
			}
			tree, err := commit.Tree()
			if err != nil {
				t.Fatalf("Tree: %v", err)
			}
			got := inferMaturinBuild(tree, tc.dir, tc.tags)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("inferMaturinBuild() diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestInferRustSysdeps(t *testing.T) {
	repo := must(gitxtest.CreateRepoFromYAML(`commits:
  - id: head
    files:
      pyproject.toml: |
        [[tool.setuptools-rust.ext-modules]]
        path = "native/angr/Cargo.toml"
      native/angr/Cargo.toml: |
        [dependencies]
        libafl_bolts = "0.15.4"
`, nil))
	commit, err := repo.Repository.CommitObject(repo.Commits["head"])
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	gotMusl := inferRustSysdeps("musllinux_1_2_x86_64", true, tree, "")
	wantMusl := []sysdeps.DependencyIdentifier{
		{Namespace: sysdeps.NamespaceApk, Name: "lld", Provenance: "wheel:comment:lld"},
		{Namespace: sysdeps.NamespaceApk, Name: "libucontext-dev", Provenance: "cargo:libafl_bolts"},
	}
	if diff := cmp.Diff(wantMusl, gotMusl); diff != "" {
		t.Errorf("inferRustSysdeps(musl) diff (-want +got):\n%s", diff)
	}
	gotGlibc := inferRustSysdeps("manylinux_2_28_x86_64", false, tree, "")
	if len(gotGlibc) != 0 {
		t.Errorf("inferRustSysdeps(glibc) = %v, want empty", gotGlibc)
	}
}

func TestAdvanceCoReleaseRegistryTime(t *testing.T) {
	wheelTime := time.Date(2026, 9, 2, 20, 22, 39, 48040000, time.UTC)
	sdistTime := time.Date(2026, 9, 2, 20, 22, 42, 848992000, time.UTC)
	rel := &pypireg.Release{
		Artifacts: []pypireg.Artifact{
			{Filename: "angr-9.3.4-cp312-abi3-manylinux_2_28_x86_64.whl", UploadTime: wheelTime},
			{Filename: "angr-9.3.4.tar.gz", UploadTime: sdistTime},
		},
	}
	mux := rebuild.RegistryMux{
		PyPI: pypireg.HTTPRegistry{
			Client: &httpxtest.MockClient{
				Calls: []httpxtest.Call{
					{
						URL:      "https://pypi.org/pypi/pyvex/9.3.4/json",
						Response: &http.Response{StatusCode: 404, Body: httpxtest.Body("not found")},
					},
				},
				URLValidator: httpxtest.NewURLValidator(t),
			},
		},
	}
	reqs := []string{"setuptools>=68", "setuptools-rust", "pyvex==9.3.4"}
	got := advanceCoReleaseRegistryTime(context.Background(), mux, "angr", "9.3.4", rel, reqs, wheelTime)
	want := time.Date(2026, 9, 2, 20, 22, 43, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("advanceCoReleaseRegistryTime() = %v, want %v", got, want)
	}
}
