// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

// Build root and isolated build environment path inference reads the paths that
// upstream built the repository and installed build dependencies at from the
// DWARF debug info or .rodata strings of a wheel's extension modules.
//
// Each compile unit (DWARF 4 §3.1.1 DW_TAG_compile_unit) records its primary
// source file in DW_AT_name and the working directory of the compiler in
// DW_AT_comp_dir. Their form depends on the toolchain:
//   - C and C++ (setuptools, meson-python): DW_AT_name is relative to
//     DW_AT_comp_dir, which is the project root or a "build/<abi>"
//     subdirectory.
//   - C and C++ (CMake): DW_AT_name is absolute, and DW_AT_comp_dir may be an
//     out-of-tree build directory.
//   - Rust (maturin, setuptools-rust): DW_AT_name of a workspace crate is
//     "<crate-root>/@/<cgu>" relative to DW_AT_comp_dir. External crates have
//     absolute paths under ".cargo".
//   - Generated C and C++ (Cython, mypyc): DW_AT_name is a generated file that
//     need not be in the repository, though its .pyx or .py source is.
//   - Toolchain and Link-Time Optimization (LTO) units: C runtime startup
//     objects and "<artificial>" LTO partitions name no file in the
//     repository and drop out.
//
// Stripped modules and CMake Release builds without "-g" have no .debug_info
// section, yet still embed NUL-terminated __FILE__ and include paths from
// assertions, logging macros, and Cython metadata in .rodata.
//
// Path inference resolves these in three steps:
//  1. Collection: Reads the DWARF compile units, DWARF 4 §6.2.4 .debug_line
//     file entries, and .rodata strings of each .so member up to maxModuleSize
//     in a single pass, except the libraries that auditwheel grafts into
//     "<distribution>.libs/" or "<pkg>/.libs/". Isolated build environment
//     paths ("/tmp/build-env-<id>" from "python -m build" and
//     "/tmp/pip-build-env-<id>/overlay" or "/normal" from "pip wheel") are
//     collected from DW_AT_name (resolved against absolute DW_AT_comp_dir when
//     relative), DW_AT_comp_dir, and .debug_line file entries (at most once per
//     path per compile unit), and from absolute "/tmp/..." or parent-relative
//     "../../tmp/..." .rodata strings. A module or section that cannot be read
//     is skipped so it does not hide paths recorded by the others.
//  2. Matching: For build roots, resolves each DW_AT_name against
//     DW_AT_comp_dir, stripping the rustc "/@/<cgu>" suffix from relative names
//     first. The longest suffix of the resulting path that names a file in the
//     repository is the repository-relative path, and the prefix before it is
//     the unit's candidate root. Taking the longest suffix keeps a top-level
//     file from shadowing a file of the same name in a subdirectory. A
//     generated .c or .cpp file also matches through its .pyx or .py sibling.
//     Relative names and generated files count only if DW_AT_comp_dir lies
//     inside the candidate root, which rejects mypyc and Cython build_dir
//     outputs written to build directories that mirror the package layout.
//     .rodata paths also match by longest suffix, but they have no
//     DW_AT_comp_dir and include strings from every linked header, so the
//     repository-relative path must span a directory and generated files do not
//     match through a .pyx or .py sibling.
//  3. Selection: Build root candidates inside "site-packages", "dist-packages",
//     ".cargo", or an isolated "/tmp/build-env-<id>" or
//     "/tmp/pip-build-env-<id>" tree (including headers outside "site-packages"
//     such as "<venv>/include/") hold unpacked dependencies and drop out, as do
//     those that validateBuildRoot rejects, such as paths that need shell
//     quoting. checkoutDir ("/src") stays a candidate so an upstream build in
//     the checkout can be told apart from a wheel with no evidence, and then
//     maps to "" since the build already runs there. Isolated build environment
//     candidates drop out if validateBuildEnvPath rejects them. In both
//     selections, the most common candidate wins, breaking ties by the
//     lexically smallest path, and .rodata candidates count only if DWARF
//     yields none.
//
// References:
//   - DWARF Debugging Information Format Version 4, §3.1.1 and §6.2.4:
//     https://dwarfstd.org/doc/DWARF4.pdf
//   - auditwheel wheel repair layout:
//     https://github.com/pypa/auditwheel
//   - rustc codegen-unit DW_AT_name suffix:
//     https://github.com/rust-lang/rust/issues/88521

package pypi

import (
	"archive/zip"
	"bytes"
	"debug/dwarf"
	"debug/elf"
	"io"
	"path"
	re "regexp"
	"slices"
	"strings"

	"github.com/msuozzo/go-git2"
	"github.com/pkg/errors"
)

// maxModuleSize bounds the extension modules that are read into memory to
// parse their ELF sections. It admits common extension modules while skipping
// the largest libraries, such as those of torch.
const maxModuleSize = 256 << 20

type compileUnit struct {
	Name    string // primary source file as passed to the compiler (DW_AT_name)
	CompDir string // working directory of the compiler (DW_AT_comp_dir)
}

type wheelSources struct {
	units          []compileUnit
	rodataPaths    []string
	dwarfEnvPaths  []string
	rodataEnvPaths []string
}

func (ws wheelSources) candidateRoots(files map[string]bool) (dwarfRoots, rodataRoots []string) {
	for _, u := range ws.units {
		dwarfRoots = append(dwarfRoots, u.buildRoot(files))
	}
	for _, p := range ws.rodataPaths {
		rodataRoots = append(rodataRoots, rodataBuildRoot(p, files))
	}
	return dwarfRoots, rodataRoots
}

func (ws wheelSources) selectBuildEnvPath() (envPath, source string) {
	if envPath := mostCommonBuildEnvPath(ws.dwarfEnvPaths); envPath != "" {
		return envPath, "DWARF"
	}
	if envPath := mostCommonBuildEnvPath(ws.rodataEnvPaths); envPath != "" {
		return envPath, ".rodata"
	}
	return "", ""
}

// candidateRoots returns the candidate roots of the wheel's extension modules
// from their DWARF compile units and from their .rodata source paths, recording
// an error in skipped for each module or section that cannot be read.
func candidateRoots(zr *zip.Reader, files map[string]bool) (dwarfRoots, rodataRoots []string, skipped []error) {
	ws, skipped := readWheelSources(zr)
	dwarfRoots, rodataRoots = ws.candidateRoots(files)
	return dwarfRoots, rodataRoots, skipped
}

// buildEnvSubpathPat matches the isolated build environment directory created
// under "/tmp" by "python -m build" ("/tmp/build-env-<id>") or "pip wheel"
// ("/tmp/pip-build-env-<id>/overlay" or "/normal"), whether recorded as an
// absolute path or a parent-relative path such as
// "../../tmp/pip-build-env-<id>/overlay/...".
var buildEnvSubpathPat = re.MustCompile(`^(?:/|(?:\.\./)+)tmp/(build-env-[A-Za-z0-9_]+|pip-build-env-[A-Za-z0-9_]+/(?:overlay|normal))(?:/|$)`)

// buildEnvPathFromELF returns the most common isolated build environment path
// in the wheel's extension modules, or "" if none is recorded.
func buildEnvPathFromELF(zr *zip.Reader) (envPath string, skipped []error) {
	ws, skipped := readWheelSources(zr)
	envPath, _ = ws.selectBuildEnvPath()
	return envPath, skipped
}

func extractBuildEnvPath(p string) string {
	m := buildEnvSubpathPat.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	return "/tmp/" + m[1]
}

func mostCommonBuildEnvPath(paths []string) string {
	counts := make(map[string]int)
	for _, p := range paths {
		if p == "" || validateBuildEnvPath(p) != nil {
			continue
		}
		counts[p]++
	}
	var best string
	for p, n := range counts {
		if n > counts[best] || (n == counts[best] && p < best) {
			best = p
		}
	}
	return best
}

// readWheelSources extracts the DWARF compile units, .rodata source paths, and
// isolated build environment paths of the wheel's extension modules in a single
// pass. A module whose debug info is malformed still contributes its .rodata
// paths.
func readWheelSources(zr *zip.Reader) (ws wheelSources, skipped []error) {
	for _, f := range zr.File {
		if !isExtensionModule(f.Name) || f.UncompressedSize64 > maxModuleSize {
			continue
		}
		ef, err := openELF(f)
		if err != nil {
			skipped = append(skipped, errors.Wrapf(err, "reading %s", f.Name))
			continue
		}
		du, dp, err := moduleDWARF(ef)
		if err != nil {
			skipped = append(skipped, errors.Wrapf(err, "reading debug info of %s", f.Name))
		}
		ws.units = append(ws.units, du...)
		ws.dwarfEnvPaths = append(ws.dwarfEnvPaths, dp...)
		rp, ep, err := moduleRodata(ef)
		if err != nil {
			skipped = append(skipped, errors.Wrapf(err, "reading .rodata of %s", f.Name))
		}
		ws.rodataPaths = append(ws.rodataPaths, rp...)
		ws.rodataEnvPaths = append(ws.rodataEnvPaths, ep...)
	}
	return ws, skipped
}

// isExtensionModule reports whether name is a shared object outside the
// "<distribution>.libs" and "<pkg>/.libs" directories where auditwheel vendors
// external libraries.
func isExtensionModule(name string) bool {
	return strings.HasSuffix(name, ".so") && !strings.Contains(path.Dir(name)+"/", ".libs/")
}

var errNotELF = errors.New("not an ELF file")

// openELF parses f as an ELF file, returning errNotELF if f is not an ELF file.
func openELF(f *zip.File) (*elf.File, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, errors.Wrap(err, "opening module")
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, errors.Wrap(err, "reading module")
	}
	if !bytes.HasPrefix(b, []byte(elf.ELFMAG)) {
		return nil, errNotELF
	}
	ef, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return nil, errors.Wrap(err, "parsing ELF")
	}
	return ef, nil
}

// moduleDWARF returns the compile units and isolated build environment paths in
// the debug info of ef, or nil, nil, nil if ef is stripped. Each compile unit
// records each build environment path at most once.
func moduleDWARF(ef *elf.File) (units []compileUnit, envPaths []string, err error) {
	// NOTE: ef.DWARF returns an error when debug sections are absent, so
	// checking for them first distinguishes stripped modules from malformed
	// debug info.
	if ef.Section(".debug_info") == nil && ef.Section(".zdebug_info") == nil {
		return nil, nil, nil
	}
	d, err := ef.DWARF()
	if err != nil {
		return nil, nil, errors.Wrap(err, "loading DWARF")
	}
	r := d.Reader()
	for {
		e, err := r.Next()
		if err != nil {
			return nil, nil, errors.Wrap(err, "reading DWARF")
		}
		if e == nil {
			return units, envPaths, nil
		}
		if e.Tag == dwarf.TagCompileUnit {
			name, _ := e.Val(dwarf.AttrName).(string)
			compDir, _ := e.Val(dwarf.AttrCompDir).(string)
			units = append(units, compileUnit{Name: name, CompDir: compDir})
			seen := make(map[string]bool)
			addEnvPath := func(s string) {
				if envPath := extractBuildEnvPath(s); envPath != "" && !seen[envPath] {
					seen[envPath] = true
					envPaths = append(envPaths, envPath)
				}
			}
			src := name
			if !path.IsAbs(src) && path.IsAbs(compDir) {
				src = path.Join(compDir, src)
			}
			addEnvPath(src)
			addEnvPath(compDir)
			// NOTE: A malformed or unsupported line table is ignored so that
			// the compile unit's Name and CompDir still participate in build
			// root selection.
			if lr, err := d.LineReader(e); err == nil && lr != nil {
				for _, lf := range lr.Files() {
					if lf != nil {
						addEnvPath(lf.Name)
					}
				}
			}
		}
		r.SkipChildren()
	}
}

// moduleRodata returns the NUL-terminated C and C++ source paths and isolated
// build environment paths in the .rodata section of ef.
func moduleRodata(ef *elf.File) (sourcePaths, envPaths []string, err error) {
	sec := ef.Section(".rodata")
	if sec == nil {
		return nil, nil, nil
	}
	b, err := sec.Data()
	if err != nil {
		return nil, nil, errors.Wrap(err, "reading .rodata")
	}
	for len(b) > 0 {
		var entry []byte
		entry, b, _ = bytes.Cut(b, []byte{0})
		if isSourcePath(entry) {
			sourcePaths = append(sourcePaths, string(entry))
		}
		if envPath := extractBuildEnvPath(string(entry)); envPath != "" {
			envPaths = append(envPaths, envPath)
		}
	}
	return sourcePaths, envPaths, nil
}

// isSourcePath reports whether b is an absolute C or C++ source or header path
// made of characters valid in a build root.
func isSourcePath(b []byte) bool {
	// NOTE: Rust .rs paths are left out because rustc records absolute paths only
	// for external Cargo dependencies, whose "src/lib.rs" suffix would match the
	// workspace's own crate.
	switch path.Ext(string(b)) {
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx":
		return buildRootPat.Match(b)
	default:
		return false
	}
}

func repoFiles(tree *git2.Tree) (map[string]bool, error) {
	files := make(map[string]bool)
	err := tree.Paths(func(p string, _ git2.FileMode, _ git2.Hash) error {
		files[p] = true
		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "walking tree")
	}
	return files, nil
}

// mostCommonRoot returns the usable candidate root that appears most often,
// breaking ties by the lexically smallest root.
func mostCommonRoot(roots []string) string {
	counts := make(map[string]int)
	for _, root := range roots {
		if root == "" || isDependencyTree(root) || (root != checkoutDir && validateBuildRoot(root) != nil) {
			continue
		}
		counts[root]++
	}
	var best string
	for root, n := range counts {
		if n > counts[best] || (n == counts[best] && root < best) {
			best = root
		}
	}
	return best
}

// dependencyTreeDirs names the directories where pip and cargo unpack
// dependencies.
var dependencyTreeDirs = []string{"site-packages", "dist-packages", ".cargo"}

func isDependencyTree(root string) bool {
	if extractBuildEnvPath(root) != "" || extractBuildEnvPath(root+"/overlay") != "" {
		return true
	}
	return slices.ContainsFunc(strings.Split(root, "/"), func(dir string) bool {
		return slices.Contains(dependencyTreeDirs, dir)
	})
}

// rodataBuildRoot returns the prefix of p before its longest multi-segment
// suffix in files, or "" if no suffix matches.
func rodataBuildRoot(p string, files map[string]bool) string {
	p = path.Clean(p)
	for i := 1; i < len(p); i++ {
		if p[i] != '/' {
			continue
		}
		root, rel := p[:i], p[i+1:]
		if !strings.Contains(rel, "/") {
			return ""
		}
		if files[rel] {
			return root
		}
	}
	return ""
}

// buildRoot returns the prefix of the unit's source path before its longest
// suffix in files, or "" if no suffix matches.
func (u compileUnit) buildRoot(files map[string]bool) string {
	compDir := path.Clean(u.CompDir)
	src := u.Name
	if !path.IsAbs(src) {
		// NOTE: Strip the codegen-unit suffix that rustc appends to relative
		// crate roots (https://github.com/rust-lang/rust/issues/88521).
		src, _, _ = strings.Cut(src, "/@/")
		src = path.Join(compDir, src)
	}
	if !path.IsAbs(src) {
		return ""
	}
	src = path.Clean(src)
	for i := 1; i < len(src); i++ {
		if src[i] != '/' {
			continue
		}
		root, rel := src[:i], src[i+1:]
		// NOTE: Relative names and generated sources require CompDir inside
		// root so out-of-tree build directories that mirror the package layout
		// cannot match.
		inRoot := compDir == root || strings.HasPrefix(compDir, root+"/")
		if files[rel] && (inRoot || path.IsAbs(u.Name)) {
			return root
		}
		if inRoot && hasPythonSource(files, rel) {
			return root
		}
	}
	return ""
}

// hasPythonSource reports whether files contains a Cython or Python source
// next to the generated C or C++ file at rel.
func hasPythonSource(files map[string]bool, rel string) bool {
	ext := path.Ext(rel)
	if ext != ".c" && ext != ".cpp" {
		return false
	}
	stem := strings.TrimSuffix(rel, ext)
	return files[stem+".pyx"] || files[stem+".py"]
}
