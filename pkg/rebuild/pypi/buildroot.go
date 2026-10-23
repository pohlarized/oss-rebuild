// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package pypi

import (
	"archive/zip"
	"bytes"
	"debug/dwarf"
	"debug/elf"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/pkg/errors"
)

// maxModuleSize bounds the extension modules that are searched for debug info,
// since each one is decompressed into memory to parse it. It admits common
// extension modules but skips the largest libraries, such as those of torch.
const maxModuleSize = 256 << 20

// compileUnit holds the source location of a DWARF compile unit.
type compileUnit struct {
	// Name is the path of the unit's primary source file as passed to the compiler.
	Name string
	// CompDir is the working directory of the compiler.
	CompDir string
}

// buildRootFromELF returns the absolute path that upstream built the
// repository at, as recorded in the ELF sections of the wheel's extension
// modules. DWARF debug info takes precedence over source paths in .rodata. It
// returns an empty string if neither refers to a file in tree. Modules that
// cannot be read are left out of the vote, so that one malformed module does
// not hide the root that the others record. skipped holds an error for each of
// them.
func buildRootFromELF(zr *zip.Reader, tree *object.Tree) (root string, skipped []error, err error) {
	units, rodataPaths, skipped := sourceUnits(zr)
	if len(units) == 0 && len(rodataPaths) == 0 {
		return "", skipped, nil
	}
	files, err := repoFiles(tree)
	if err != nil {
		return "", skipped, errors.Wrap(err, "listing repository files")
	}
	if root := voteBuildRoot(units, files); root != "" {
		return root, skipped, nil
	}
	return voteRodataBuildRoot(rodataPaths, files), skipped, nil
}

// sourceUnits returns the DWARF compile units and the .rodata source paths of
// the wheel's extension modules. skipped holds an error for each module or
// section that cannot be read. A module whose debug info is malformed still
// contributes its .rodata paths.
func sourceUnits(zr *zip.Reader) (units []compileUnit, rodataPaths []string, skipped []error) {
	for _, f := range zr.File {
		if !isExtensionModule(f.Name) || f.UncompressedSize64 > maxModuleSize {
			continue
		}
		ef, err := openELF(f)
		if err != nil {
			skipped = append(skipped, errors.Wrapf(err, "reading %s", f.Name))
			continue
		}
		if ef == nil {
			continue
		}
		du, err := moduleCompileUnits(ef)
		if err != nil {
			skipped = append(skipped, errors.Wrapf(err, "reading debug info of %s", f.Name))
		}
		units = append(units, du...)
		rp, err := moduleRodataPaths(ef)
		if err != nil {
			skipped = append(skipped, errors.Wrapf(err, "reading .rodata of %s", f.Name))
		}
		rodataPaths = append(rodataPaths, rp...)
	}
	return units, rodataPaths, skipped
}

// isExtensionModule reports whether the wheel member is a shared object built
// with the project. auditwheel vendors external libraries into a directory
// named "<name>.libs", or "<pkg>/.libs" in older releases.
func isExtensionModule(name string) bool {
	return strings.HasSuffix(name, ".so") && !strings.Contains(path.Dir(name)+"/", ".libs/")
}

// openELF parses a zip member as an ELF file, returning nil if it is not ELF.
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
		return nil, nil
	}
	ef, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return nil, errors.Wrap(err, "parsing ELF")
	}
	return ef, nil
}

// moduleCompileUnits returns the compile units in the debug info of ef.
// Stripped modules have none.
func moduleCompileUnits(ef *elf.File) ([]compileUnit, error) {
	// NOTE: DWARF fails without debug sections, so checking for them first
	// tells stripped modules apart from malformed debug info.
	if ef.Section(".debug_info") == nil && ef.Section(".zdebug_info") == nil {
		return nil, nil
	}
	d, err := ef.DWARF()
	if err != nil {
		return nil, errors.Wrap(err, "loading DWARF")
	}
	var units []compileUnit
	r := d.Reader()
	for {
		e, err := r.Next()
		if err != nil {
			return nil, errors.Wrap(err, "reading DWARF")
		}
		if e == nil {
			return units, nil
		}
		if e.Tag == dwarf.TagCompileUnit {
			name, _ := e.Val(dwarf.AttrName).(string)
			compDir, _ := e.Val(dwarf.AttrCompDir).(string)
			units = append(units, compileUnit{Name: name, CompDir: compDir})
		}
		r.SkipChildren()
	}
}

// moduleRodataPaths returns the absolute C and C++ source paths in the .rodata
// section of ef. Stripped modules lose their DWARF compile units but retain
// __FILE__ strings from assertions and logging macros in .rodata.
func moduleRodataPaths(ef *elf.File) ([]string, error) {
	sec := ef.Section(".rodata")
	if sec == nil {
		return nil, nil
	}
	b, err := sec.Data()
	if err != nil {
		return nil, errors.Wrap(err, "reading .rodata")
	}
	var paths []string
	for len(b) > 0 {
		var entry []byte
		entry, b, _ = bytes.Cut(b, []byte{0})
		if isSourcePath(entry) {
			paths = append(paths, string(entry))
		}
	}
	return paths, nil
}

// isSourcePath reports whether b is an absolute path to a C or C++ source or
// header file made of the characters a build root may contain. Rust paths are
// excluded because rustc records relative paths for workspace crates and
// absolute paths only for external Cargo dependencies, whose "src/lib.rs"
// suffix would match the workspace's own crate.
func isSourcePath(b []byte) bool {
	switch path.Ext(string(b)) {
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx":
		return buildRootPat.Match(b)
	default:
		return false
	}
}

// repoFiles returns the set of file paths in tree.
func repoFiles(tree *object.Tree) (map[string]bool, error) {
	files := make(map[string]bool)
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	for {
		name, entry, err := w.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, errors.Wrap(err, "walking tree")
		}
		if entry.Mode.IsFile() {
			files[name] = true
		}
	}
}

// voteBuildRoot returns the build root that the most units agree on. Units
// whose source is not in files have no say.
func voteBuildRoot(units []compileUnit, files map[string]bool) string {
	roots := make([]string, 0, len(units))
	for _, u := range units {
		roots = append(roots, u.buildRoot(files))
	}
	return mostCommonRoot(roots)
}

// voteRodataBuildRoot returns the build root that the most .rodata source
// paths agree on. Paths whose source is not in files have no say.
func voteRodataBuildRoot(paths []string, files map[string]bool) string {
	roots := make([]string, 0, len(paths))
	for _, p := range paths {
		roots = append(roots, rodataBuildRoot(p, files))
	}
	return mostCommonRoot(roots)
}

// mostCommonRoot returns the root that occurs most often, breaking ties by the
// lexically smallest root. Empty roots, roots that the build step cannot use
// and roots in dependency trees are skipped. The checkout stays a candidate so
// that a build there can be told apart from no evidence.
func mostCommonRoot(roots []string) string {
	votes := make(map[string]int)
	for _, root := range roots {
		if root == "" || isDependencyTree(root) || (root != checkoutDir && validateBuildRoot(root) != nil) {
			continue
		}
		votes[root]++
	}
	var best string
	for root, n := range votes {
		if n > votes[best] || (n == votes[best] && root < best) {
			best = root
		}
	}
	return best
}

// dependencyTreeDirs name the directories that pip and Cargo unpack
// dependencies into. A root inside one holds a dependency that shares a path
// with a repository file, never the repository itself.
var dependencyTreeDirs = []string{"site-packages", "dist-packages", ".cargo"}

func isDependencyTree(root string) bool {
	return slices.ContainsFunc(strings.Split(root, "/"), func(dir string) bool {
		return slices.Contains(dependencyTreeDirs, dir)
	})
}

// rodataBuildRoot returns what remains of the absolute source path p after its
// longest suffix that is a path in files, or an empty string if no suffix is.
// Unlike a compile unit, a .rodata path has no working directory to anchor it
// and may come from any header linked into the module. So the suffix has to
// span a directory, which keeps a top-level file such as "config.h" from
// matching an unrelated header of that name. Generated sources do not match,
// since without a working directory a generated file in a build directory
// cannot be told apart from one in the source tree.
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

// buildRoot returns what remains of the unit's absolute source path after its
// longest suffix that is a path in files, or an empty string if no suffix is.
// Taking the longest keeps a file at the top of the repository from shadowing
// its namesake in a subdirectory.
func (u compileUnit) buildRoot(files map[string]bool) string {
	compDir := path.Clean(u.CompDir)
	src := u.Name
	if !path.IsAbs(src) {
		// NOTE: rustc appends the codegen unit to the crate root to keep unit
		// names unique, as in "src/lib.rs/@/ext.1a2b3c4d-cgu.0".
		// https://github.com/rust-lang/rust/issues/88521
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
		// NOTE: The compiler resolves a relative name against its working
		// directory, which lies within the build root of setuptools and meson
		// builds. Generated sources need the same check because mypyc and
		// Cython's build_dir option write them to build directories that mirror
		// the package layout.
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

// hasPythonSource reports whether files holds a Cython or Python source that
// the C or C++ file at rel could have been generated from.
func hasPythonSource(files map[string]bool, rel string) bool {
	ext := path.Ext(rel)
	if ext != ".c" && ext != ".cpp" {
		return false
	}
	stem := strings.TrimSuffix(rel, ext)
	return files[stem+".pyx"] || files[stem+".py"]
}
