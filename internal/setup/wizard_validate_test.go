// No build tag: validateOutputPath has no TUI dependency and can be tested
// without -tags setup. The function lives in wizard.go (build tag: setup),
// so this file must also carry the setup tag to access the unexported symbol.
//
//go:build setup

package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateOutputPath exercises the path validation logic covering
// safe relative paths, safe subdirectory paths, and various escape attempts.
func TestValidateOutputPath(t *testing.T) {
	t.Parallel()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		// --- Valid paths (must be inside cwd) ---
		{
			name:    "simple relative .env",
			path:    ".env",
			wantErr: false,
		},
		{
			name:    "explicit ./ prefix",
			path:    "./.env",
			wantErr: false,
		},
		{
			name:    "custom filename in cwd",
			path:    "foo.env",
			wantErr: false,
		},
		{
			name:    "subdirectory relative path",
			path:    "subdir/.env",
			wantErr: false,
		},
		{
			name:    "nested subdirectory",
			path:    "a/b/c.env",
			wantErr: false,
		},
		// --- Invalid: ".." escape ---
		{
			name:    "parent directory traversal",
			path:    "../escape.env",
			wantErr: true,
		},
		{
			name:    "double dot in middle",
			path:    "subdir/../../escape.env",
			wantErr: true,
		},
		// --- Invalid: sensitive absolute paths ---
		{
			name:    "/etc/passwd",
			path:    "/etc/passwd",
			wantErr: true,
		},
		{
			name:    "/root/.env",
			path:    "/root/.env",
			wantErr: true,
		},
		{
			name:    "/usr/local/.env",
			path:    "/usr/local/.env",
			wantErr: true,
		},
		// --- Invalid: absolute path outside cwd (not a sensitive prefix) ---
		{
			name: "absolute path outside cwd",
			// Use /tmp which is not a sensitive prefix but is not inside cwd.
			path:    "/tmp/outsider.env",
			wantErr: cwd != "/tmp", // only an error when cwd is not /tmp
		},
		// --- Invalid: absolute path that happens to equal cwd should be allowed ---
		{
			name:    "absolute path equal to cwd",
			path:    filepath.Join(cwd, ".env"),
			wantErr: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateOutputPath(tc.path)
			if tc.wantErr && err == nil {
				t.Errorf("validateOutputPath(%q): expected error, got nil", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateOutputPath(%q): unexpected error: %v", tc.path, err)
			}
		})
	}
}

// TestValidateOutputPath_SensitivePrefixes verifies that every sensitive system
// prefix is rejected even with a trailing filename.
func TestValidateOutputPath_SensitivePrefixes(t *testing.T) {
	t.Parallel()

	prefixes := []string{
		"/etc", "/root", "/usr", "/bin", "/sbin", "/sys", "/proc",
	}

	for _, p := range prefixes {
		p := p
		t.Run(p, func(t *testing.T) {
			t.Parallel()

			path := p + "/something.env"
			err := validateOutputPath(path)
			if err == nil {
				t.Errorf("validateOutputPath(%q): expected error for sensitive prefix, got nil", path)
			}
			// Error message should mention the path.
			if err != nil && !strings.Contains(err.Error(), p) {
				t.Errorf("error message %q does not mention rejected prefix %q", err.Error(), p)
			}
		})
	}
}
