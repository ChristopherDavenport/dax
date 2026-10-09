package private

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestSecureDir(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before os.FileMode // 0: absent
		warn   string
	}{
		{"a missing directory is created private", 0, ""},
		{"a private one is left alone", 0o700, ""},
		{"a group-readable one is made private, with a warning", 0o750, "(mode 0750); it is now private (0700)"},
		{"a world-readable one too", 0o755, "(mode 0755); it is now private (0700)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := filepath.Join(t.TempDir(), "a", "b")
			if tc.before != 0 {
				if err := os.MkdirAll(d, tc.before); err != nil {
					t.Fatal(err)
				}
				os.Chmod(d, tc.before)
			}
			var got strings.Builder
			restore := Capture(&got)
			err := SecureDir(d)
			restore()
			if err != nil {
				t.Fatal(err)
			}
			if m := mode(t, d); m != 0o700 {
				t.Errorf("mode %04o, want 0700", m)
			}
			if tc.warn == "" && got.Len() != 0 || !strings.Contains(got.String(), tc.warn) {
				t.Errorf("warned %q, want %q", got.String(), tc.warn)
			}
			if Warnings() != os.Stderr {
				t.Error("Capture's restore did not put standard error back")
			}
		})
	}
}

func TestAFileIsNotADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0o600)
	if err := SecureDir(f); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v", err)
	}
}
