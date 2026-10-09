// Copyright 2025 Google LLC
// SPDX-License-Identifier: Apache-2.0

package rebuild

import (
	"testing"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestMatchTag(t *testing.T) {
	tests := []struct {
		tag     string
		pkg     string
		version string
		strict  bool
		approx  bool
	}{
		{"v1.0.0", "mypackage", "1.0.0", true, true},
		{"v1.0.0+meta", "mypackage", "1.0.0+meta", true, true},
		{"v1x0x0", "mypackage", "1.0.0", false, false},
		{"v1.0.0-rc1", "mypackage", "1.0.0", false, true},
		{"mypackage-1.0.0", "mypackage", "1.0.0", true, true},
		{"1.0.0", "mypackage", "1.0.0", true, true},
		{"v1.0.1", "mypackage", "1.0.0", false, false},
		{"v1.0", "mypackage", "1.0.0", false, false},
		{"v1", "mypackage", "1.0.0", false, false},
		{"org/mypackage-1.0.0", "org/mypackage", "1.0.0", true, true},
		{"mypackage-1.0.0", "org/mypackage", "1.0.0", true, true},
		{"org/otherpackage-1.0.0", "org/mypackage", "1.0.0", false, true}, // org-but-not-package special case
		{"otherpackage-1.0.0", "org/mypackage", "1.0.0", true, true},
		{"rel_2_0_52", "sqlalchemy", "2_0_52", true, true},
		{"rel_2_0_52", "sqlalchemy", "2.0.52", false, false},
		{"rel_2_0_5_post1", "sqlalchemy", "2_0_52", false, false},
		{"rel_1_0_0_beta1", "mypackage", "1_0_0", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.tag+"-"+tt.version, func(t *testing.T) {
			strict, approx := MatchTag(tt.tag, tt.pkg, tt.version)
			if strict != tt.strict || approx != tt.approx {
				t.Errorf("MatchTag(%q, %q, %q) = (%v, %v), want (%v, %v)", tt.tag, tt.pkg, tt.version, strict, approx, tt.strict, tt.approx)
			}
		})
	}
}

func TestVersionSpellings(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    []string
	}{
		{name: "Dotted", version: "2.0.52", want: []string{"2.0.52", "2_0_52"}},
		{name: "NoDots", version: "7", want: []string{"7"}},
		{name: "LocalVersion", version: "1.0.0+meta", want: []string{"1.0.0+meta", "1_0_0+meta"}},
		{name: "PreRelease", version: "2.0.0b1", want: []string{"2.0.0b1", "2_0_0b1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, versionSpellings(tt.version)); diff != "" {
				t.Errorf("versionSpellings(%q) diff (-want +got):\n%s", tt.version, diff)
			}
		})
	}
}

func TestFindTagMatch(t *testing.T) {
	repo := must(git.Init(memory.NewStorage(), memfs.New()))

	c1 := createCommit(repo, "commit1")
	c2 := createCommit(repo, "commit2")
	c3 := createCommit(repo, "commit3")
	createLightweightTag(repo, "v1.0.0", c1)
	createLightweightTag(repo, "v1.1.0", c2)
	createAnnotatedTag(repo, "v1.0.0-alpha", c3)
	createLightweightTag(repo, "rel_2_0_5", c1)
	createLightweightTag(repo, "rel_2_0_52", c2)
	createLightweightTag(repo, "rel_1_1_0", c3)
	createLightweightTag(repo, "v3.23.0", c1)
	createLightweightTag(repo, "v3.23.0x", c2)

	tests := []struct {
		pkg     string
		version string
		want    string
		wantErr bool
	}{
		{"mypackage", "1.0.0", c1, false},
		{"mypackage", "1.1.0", c2, false},
		{"otherpackage", "1.0.0", c1, false},
		{"otherpackage", "1.0.0-alpha", c3, false},
		{"sqlalchemy", "2.0.52", c2, false},
		{"sqlalchemy", "2.0.5", c1, false},
		{"pycryptodome", "3.23.0", c1, false},
		{"pycryptodomex", "3.23.0", c2, false},
		{"mypackage", "2.0.0", "", false}, // No match
		// TODO: Add error test cases.
	}

	for _, tt := range tests {
		t.Run(tt.pkg+"-"+tt.version, func(t *testing.T) {
			got, err := FindTagMatch(tt.pkg, tt.version, repo)
			if (err != nil) != tt.wantErr {
				t.Errorf("FindTagMatch() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("FindTagMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllTags(t *testing.T) {
	repo := must(git.Init(memory.NewStorage(), memfs.New()))

	c1 := createCommit(repo, "commit1")
	c2 := createCommit(repo, "commit2")
	c3 := createCommit(repo, "commit3")
	createLightweightTag(repo, "v1.0.0", c1)
	createLightweightTag(repo, "v1.1.0", c2)
	createAnnotatedTag(repo, "v1.0.0-alpha", c3)

	want := []string{"v1.0.0", "v1.1.0", "v1.0.0-alpha"}

	got, err := allTags(repo)
	if err != nil {
		t.Errorf("allTags() error = %v", err)
	}

	stringLess := func(a, b string) bool { return a < b }
	if diff := cmp.Diff(got, want, cmpopts.SortSlices(stringLess)); diff != "" {
		t.Errorf("allTags() diff\n%s", diff)
	}
}

func createCommit(repo *git.Repository, name string) string {
	worktree := must(repo.Worktree())
	must(worktree.Filesystem.Create(name))
	must(worktree.Add(name))
	commit := must(worktree.Commit("Test commit", &git.CommitOptions{
		Author:    &object.Signature{Name: "Test Author", Email: "test@example.com"},
		Committer: &object.Signature{Name: "Test Author", Email: "test@example.com"},
	}))
	return commit.String()
}

func createLightweightTag(repo *git.Repository, tag, targetCommit string) {
	commit := must(repo.CommitObject(plumbing.NewHash(targetCommit)))
	must(repo.CreateTag(tag, commit.Hash, nil))
}

func createAnnotatedTag(repo *git.Repository, tag, targetCommit string) {
	commit := must(repo.CommitObject(plumbing.NewHash(targetCommit)))
	must(repo.CreateTag(tag, commit.Hash, &git.CreateTagOptions{
		Message: "Test annotated tag",
		Tagger:  &object.Signature{Name: "Test Author", Email: "test@example.com"},
	}))
}

func must[T any](t T, err error) T {
	if err != nil {
		panic(err)
	}
	return t
}
