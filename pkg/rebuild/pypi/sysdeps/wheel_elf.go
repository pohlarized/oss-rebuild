// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package sysdeps

import (
	"archive/zip"
	"bytes"
	"debug/elf"
	"encoding/binary"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pkg/errors"
)

var auditwheelHashPattern = regexp.MustCompile(`-[0-9a-fA-F]{8,}\.so`)
var sonameMajorPattern = regexp.MustCompile(`^(lib[a-zA-Z0-9_\-+]+)\.so\.(\d+)`)

// ExtractWheelElfDependencies extracts shared library sonames and C library stems from ELF binaries inside a wheel.
func ExtractWheelElfDependencies(zr *zip.Reader) ([]DependencyIdentifier, error) {
	var ids []DependencyIdentifier
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".so") && !strings.Contains(f.Name, ".so.") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, errors.Wrapf(err, "opening file %s in wheel", f.Name)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, errors.Wrapf(err, "reading file %s in wheel", f.Name)
		}

		// auditwheel appends hashes to .so filenames in `.libs/` directory, but we want the "pure"
		// names as they would appear on a host system
		if strings.Contains(f.Name, ".libs/") {
			base := filepath.Base(f.Name)
			unhashed := auditwheelHashPattern.ReplaceAllString(base, ".so")
			if soname := extractSonameFromFilename(unhashed); soname != "" && !isStandardBaseLibrary(soname) {
				ids = append(ids, DependencyIdentifier{
					Namespace:  NamespaceSoname,
					Name:       soname,
					Provenance: "wheel:libs:" + f.Name,
				})
			}
			if stem := extractCLibStem(unhashed); stem != "" {
				ids = append(ids, DependencyIdentifier{
					Namespace:  NamespaceCLib,
					Name:       stem,
					Provenance: "wheel:libs:" + f.Name,
				})
			}
		}
		elfFile, err := elf.NewFile(bytes.NewReader(body))
		if err != nil {
			// Skip non-ELF files or architecture mismatches
			continue
		}
		// Extract all the libraries that the shared object expresses to depend on
		libs, err := elfFile.ImportedLibraries()
		if err != nil {
			continue
		}
		for _, lib := range libs {
			if isStandardBaseLibrary(lib) {
				continue
			}
			ids = append(ids, DependencyIdentifier{
				Namespace:  NamespaceSoname,
				Name:       lib,
				Provenance: "wheel:dt_needed:" + f.Name,
			})
			if stem := extractCLibStem(lib); stem != "" {
				ids = append(ids, DependencyIdentifier{
					Namespace:  NamespaceCLib,
					Name:       stem,
					Provenance: "wheel:dt_needed:" + f.Name,
				})
			}
		}
	}
	return DeduplicateIdentifiers(ids), nil
}

// Determine whether a library is a base library already installed in the build images
func isStandardBaseLibrary(soname string) bool {
	switch {
	case soname == "libc.so.6",
		soname == "libm.so.6",
		soname == "libdl.so.2",
		soname == "libpthread.so.0",
		soname == "librt.so.1",
		soname == "libutil.so.1",
		soname == "libresolv.so.2",
		soname == "libnsl.so.1",
		soname == "libcrypt.so.1",
		soname == "libgcc_s.so.1",
		soname == "libstdc++.so.6",
		strings.HasPrefix(soname, "ld-linux"),
		strings.HasPrefix(soname, "ld-musl"),
		strings.HasPrefix(soname, "libpython"):
		return true
	}
	return false
}

func extractCLibStem(name string) string {
	cleaned := auditwheelHashPattern.ReplaceAllString(name, ".so")
	base := filepath.Base(cleaned)
	if !strings.HasPrefix(base, "lib") {
		return ""
	}
	stem := strings.TrimPrefix(base, "lib")
	idx := strings.Index(stem, ".so")
	if idx <= 0 {
		return ""
	}
	return stem[:idx]
}

func extractSonameFromFilename(name string) string {
	cleaned := auditwheelHashPattern.ReplaceAllString(name, ".so")
	base := filepath.Base(cleaned)
	if m := sonameMajorPattern.FindStringSubmatch(base); len(m) == 3 {
		return m[1] + ".so." + m[2]
	}
	if strings.HasSuffix(base, ".so") {
		return base
	}
	return ""
}

const gnuPropertyStackSize = 1

// WheelHasGnuPropertyStackSize inspects ELF binaries in zr for .note.gnu.property sections
// and reports whether any binary contains a GNU_PROPERTY_STACK_SIZE note.
func WheelHasGnuPropertyStackSize(zr *zip.Reader) (bool, error) {
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".so") && !strings.Contains(f.Name, ".so.") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return false, errors.Wrapf(err, "opening file %s in wheel", f.Name)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return false, errors.Wrapf(err, "reading file %s in wheel", f.Name)
		}
		elfFile, err := elf.NewFile(bytes.NewReader(body))
		if err != nil {
			continue
		}
		sec := elfFile.Section(".note.gnu.property")
		if sec == nil {
			continue
		}
		data, err := sec.Data()
		if err != nil || len(data) < 12 {
			continue
		}
		var byteOrder binary.ByteOrder = binary.LittleEndian
		if elfFile.ByteOrder != nil {
			byteOrder = elfFile.ByteOrder
		}
		namesz := byteOrder.Uint32(data[0:4])
		descsz := byteOrder.Uint32(data[4:8])
		namePadding := (namesz + 3) &^ 3
		descOffset := 12 + namePadding
		if int(descOffset+descsz) > len(data) {
			continue
		}
		desc := data[descOffset : descOffset+descsz]
		var align uint32 = 4
		if elfFile.Class == elf.ELFCLASS64 {
			align = 8
		}
		pOffset := uint32(0)
		for pOffset+8 <= uint32(len(desc)) {
			prType := byteOrder.Uint32(desc[pOffset : pOffset+4])
			prDatasz := byteOrder.Uint32(desc[pOffset+4 : pOffset+8])
			if prType == gnuPropertyStackSize {
				return true, nil
			}
			itemLen := 8 + ((prDatasz + (align - 1)) &^ (align - 1))
			if itemLen < 8 {
				break
			}
			pOffset += itemLen
		}
	}
	return false, nil
}

// StripMode indicates the symbol/debug stripping mode for an ELF binary.
type StripMode string

const (
	StripModeNone  StripMode = ""
	StripModeDebug StripMode = "debug"
	StripModeAll   StripMode = "all"
)

// ExtractWheelStripModes inspects all ELF shared objects in an upstream wheel and returns
// a map of file paths to their required strip mode ("debug" or "all") for binaries that
// lack debug sections or symbol tables in upstream.
func ExtractWheelStripModes(zr *zip.Reader) (map[string]string, error) {
	modes := make(map[string]string)
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".so") && !strings.Contains(f.Name, ".so.") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, errors.Wrapf(err, "opening file %s in wheel", f.Name)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, errors.Wrapf(err, "reading file %s in wheel", f.Name)
		}

		elfFile, err := elf.NewFile(bytes.NewReader(body))
		if err != nil {
			// Skip non-ELF files
			continue
		}
		hasDebug := false
		for _, s := range elfFile.Sections {
			if strings.HasPrefix(s.Name, ".debug_") || strings.HasPrefix(s.Name, ".zdebug_") || s.Name == ".gdb_index" {
				hasDebug = true
				break
			}
		}
		hasSymtab := elfFile.Section(".symtab") != nil
		if !hasDebug {
			mode := string(StripModeDebug)
			if !hasSymtab {
				mode = string(StripModeAll)
			}
			modes[f.Name] = mode
			if strings.Contains(f.Name, ".libs/") {
				unhashed := auditwheelHashPattern.ReplaceAllString(filepath.Base(f.Name), ".so")
				modes[unhashed] = mode
			}
		}
	}
	return modes, nil
}
