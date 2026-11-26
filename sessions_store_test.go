package dax

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/cas"
)

// The store a run records in, and the admin modes act on: -sessions,
// else the user's config's sessions, else ~/.dax/sessions. The
// directory store is .dax/sessions where dax starts, private, with a
// .gitignore of "*" so git add -A leaves the record out.
func TestSessionsStoreChoice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string // the user's config; "" for none
		args   []string
		where  string // "dir", "home" or "" for no store made
	}{
		{"the default is the user's store", "", []string{"-gc", "pack"}, "home"},
		{"-sessions user", "", []string{"-sessions", "user", "-gc", "pack"}, "home"},
		{"-sessions directory", "", []string{"-sessions", "directory", "-gc", "pack"}, "dir"},
		{"the config's directory", `{"sessions":"directory"}`, []string{"-gc", "pack"}, "dir"},
		{"-sessions over the config's", `{"sessions":"directory"}`, []string{"-sessions", "user", "-gc", "pack"}, "home"},
		{"-bare leaves the config's out", `{"sessions":"directory"}`, []string{"-bare", "-gc", "pack"}, "home"},
		{"-list makes no store", "", []string{"-sessions", "directory", "-list"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := home(t)
			if tc.config != "" {
				write(t, filepath.Join(h, ".config", "dax", "config.json"), tc.config)
			}
			dir := t.TempDir()
			t.Chdir(dir)
			if err := run(context.Background(), tc.args, program{name: "dax", version: "test"}); err != nil {
				t.Fatal(err)
			}
			local := filepath.Join(dir, ".dax", "sessions")
			user := filepath.Join(h, ".dax", "sessions")
			switch tc.where {
			case "dir":
				if !cas.IsStore(local) || cas.IsStore(user) {
					t.Fatalf("the store is not %s alone (user store: %v)", local, cas.IsStore(user))
				}
				got, err := os.ReadFile(filepath.Join(local, ".gitignore"))
				if err != nil || string(got) != "*\n" {
					t.Fatalf(".gitignore = %q, %v; want \"*\\n\"", got, err)
				}
				if fi, err := os.Stat(local); err != nil || fi.Mode().Perm() != 0o700 {
					t.Fatalf("the store's mode: %v, %v; want 0700", fi.Mode().Perm(), err)
				}
			case "home":
				if !cas.IsStore(user) {
					t.Fatalf("no store at %s", user)
				}
				if _, err := os.Stat(local); !os.IsNotExist(err) {
					t.Fatalf("%s was made: %v", local, err)
				}
			default:
				if _, err := os.Stat(local); !os.IsNotExist(err) {
					t.Fatalf("%s was made: %v", local, err)
				}
			}
		})
	}
}

// A .gitignore the project's store already has is its own; a link in
// the store's way, which a repository could ship to send the record
// elsewhere, is refused.
func TestDirectoryStore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		write bool
		err   string
		want  string // the .gitignore after
	}{
		{"made with its .gitignore", func(*testing.T, string) {}, true, "", "*\n"},
		{"a .gitignore of its own is kept", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, ".dax", "sessions", ".gitignore"), "objects/\n")
		}, true, "", "objects/\n"},
		{"read-only makes nothing", func(*testing.T, string) {}, false, "", ""},
		{"the store a link", func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, ".dax"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".dax", "sessions")); err != nil {
				t.Fatal(err)
			}
		}, true, "is a link", ""},
		{"the project's .dax a link", func(t *testing.T, dir string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".dax")); err != nil {
				t.Fatal(err)
			}
		}, false, "is a link", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			root := filepath.Join(dir, ".dax", "sessions")
			err := directoryStore(root, tc.write)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(root, ".gitignore"))
			if tc.want == "" {
				if !os.IsNotExist(err) {
					t.Fatalf(".gitignore: %q, %v; want none", got, err)
				}
				return
			}
			if string(got) != tc.want {
				t.Fatalf(".gitignore = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
