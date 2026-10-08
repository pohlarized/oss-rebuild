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

// buildRootFromDWARF returns the absolute path that upstream built the
// repository at, as recorded in the debug info of the wheel's extension
// modules. It returns an empty string if no debug info refers to a file in
// tree. Modules whose debug info cannot be read are left out of the vote, so
// that one malformed module does not hide the root that the others record.
// skipped holds an error for each of them.
func buildRootFromDWARF(zr *zip.Reader, tree *object.Tree) (root string, skipped []error, err error) {
	units, skipped := compileUnits(zr)
	if len(units) == 0 {
		return "", skipped, nil
	}
	files, err := repoFiles(tree)
	if err != nil {
		return "", skipped, errors.Wrap(err, "listing repository files")
	}
	return voteBuildRoot(units, files), skipped, nil
}

// compileUnits returns the DWARF compile units of the wheel's extension
// modules. skipped holds an error for each module whose units cannot be read.
func compileUnits(zr *zip.Reader) (units []compileUnit, skipped []error) {
	for _, f := range zr.File {
		if !isExtensionModule(f.Name) || f.UncompressedSize64 > maxModuleSize {
			continue
		}
		moduleUnits, err := moduleCompileUnits(f)
		if err != nil {
			skipped = append(skipped, errors.Wrapf(err, "reading %s", f.Name))
			continue
		}
		units = append(units, moduleUnits...)
	}
	return units, skipped
}

// isExtensionModule reports whether the wheel member is a shared object built
// with the project. auditwheel vendors external libraries into a directory
// named "<name>.libs", or "<pkg>/.libs" in older releases.
func isExtensionModule(name string) bool {
	return strings.HasSuffix(name, ".so") && !strings.Contains(path.Dir(name)+"/", ".libs/")
}

// moduleCompileUnits returns the compile units in the debug info of an
// extension module. Stripped modules and files other than ELF have none.
func moduleCompileUnits(f *zip.File) ([]compileUnit, error) {
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

// voteBuildRoot returns the build root that the most units agree on, breaking
// ties by the lexically smallest root. Units whose source is not in files have
// no say.
func voteBuildRoot(units []compileUnit, files map[string]bool) string {
	votes := make(map[string]int)
	for _, u := range units {
		if root := u.buildRoot(files); root != "" {
			votes[root]++
		}
	}
	var best string
	for root, n := range votes {
		if n > votes[best] || (n == votes[best] && root < best) {
			best = root
		}
	}
	return best
}

// buildRoot returns what remains of the unit's absolute source path after its
// longest suffix that is a path in files, or an empty string if no suffix is.
// Taking the longest keeps a file at the top of the repository from shadowing
// its namesake in a subdirectory.
func (u compileUnit) buildRoot(files map[string]bool) string {
	compDir := path.Clean(u.CompDir)
	src := u.Name
	if !path.IsAbs(src) {
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
