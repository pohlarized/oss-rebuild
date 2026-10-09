// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package gitscan

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/go-cmp/cmp"
	"github.com/google/oss-rebuild/pkg/archive"
	"github.com/google/oss-rebuild/pkg/archive/archivetest"
)

func blobHash(body string) plumbing.Hash {
	h := plumbing.NewHasher(plumbing.BlobObject, int64(len(body)))
	_, _ = h.Write([]byte(body))
	return h.Sum()
}

func TestBlobHashesFromTar(t *testing.T) {
	tests := []struct {
		name    string
		entries []archive.TarEntry
		want    []plumbing.Hash
	}{
		{
			name: "RegularFiles",
			entries: []archive.TarEntry{
				{Header: &tar.Header{Name: "pkg/a.py", Typeflag: tar.TypeReg, Size: 5, Mode: 0o644}, Body: []byte("hello")},
				{Header: &tar.Header{Name: "pkg/b.py", Typeflag: tar.TypeReg, Size: 5, Mode: 0o644}, Body: []byte("world")},
			},
			want: []plumbing.Hash{blobHash("hello"), blobHash("world")},
		},
		{
			name: "SkipsDirectoriesAndSymlinks",
			entries: []archive.TarEntry{
				{Header: &tar.Header{Name: "pkg/", Typeflag: tar.TypeDir, Mode: 0o755}},
				{Header: &tar.Header{Name: "pkg/link", Typeflag: tar.TypeSymlink, Linkname: "a.py"}},
				{Header: &tar.Header{Name: "pkg/a.py", Typeflag: tar.TypeReg, Size: 5, Mode: 0o644}, Body: []byte("hello")},
			},
			want: []plumbing.Hash{blobHash("hello")},
		},
		{
			name: "EmptyFile",
			entries: []archive.TarEntry{
				{Header: &tar.Header{Name: "pkg/__init__.py", Typeflag: tar.TypeReg, Size: 0, Mode: 0o644}, Body: nil},
			},
			want: []plumbing.Hash{blobHash("")},
		},
		{
			name:    "EmptyArchive",
			entries: nil,
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf, err := archivetest.TarFile(tt.entries)
			if err != nil {
				t.Fatalf("TarFile() error = %v", err)
			}
			got, err := BlobHashesFromTar(tar.NewReader(bytes.NewReader(buf.Bytes())))
			if err != nil {
				t.Fatalf("BlobHashesFromTar() error = %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("BlobHashesFromTar() diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBlobHashesFromZip(t *testing.T) {
	entries := []archive.ZipEntry{
		{FileHeader: &zip.FileHeader{Name: "pkg/a.py"}, Body: []byte("hello")},
		{FileHeader: &zip.FileHeader{Name: "pkg/b.py"}, Body: []byte("world")},
	}
	buf, err := archivetest.ZipFile(entries)
	if err != nil {
		t.Fatalf("ZipFile() error = %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("NewReader() error = %v", err)
	}
	got, err := BlobHashesFromZip(zr)
	if err != nil {
		t.Fatalf("BlobHashesFromZip() error = %v", err)
	}
	want := []plumbing.Hash{blobHash("hello"), blobHash("world")}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("BlobHashesFromZip() diff (-want +got):\n%s", diff)
	}
}
