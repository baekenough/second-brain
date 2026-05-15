//go:build setup

package setup_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baekenough/second-brain/internal/setup"
	"github.com/rogpeppe/go-internal/testscript"
)

// TestMain registers the "envwrite" testscript command and runs the test suite.
//
// # Design: why envwrite instead of a full wizard command
//
// The wizard's interactive channel-selection form (huh.MultiSelect) requires a
// real or pseudo-TTY even in WithAccessible mode. testscript runs in a plain
// pipe environment, so invoking Run() directly would fail at the form step.
//
// Rather than mocking the TTY or adding a test-only bypass deep inside the
// wizard, we register an "envwrite" command that calls setup.WriteEnv directly
// with values supplied via txtar env vars. This covers the path that matters
// most for an integration smoke-test: the atomic-write + backup logic end-to-end,
// including file mode, header content, and backup creation.
//
// The new --channels / --value flags added to Run() are exercised separately
// by unit tests in wizard_validate_test.go and the messages_test.go suite.
func TestMain(m *testing.M) {
	testscript.RunMain(m, map[string]func() int{
		// envwrite KEY=VALUE[,KEY=VALUE,...] <dest>
		// Writes the given key=value pairs to <dest> in order, using setup.WriteEnv.
		// The final argument is always the destination path.
		// Example: envwrite FILESYSTEM_ENABLED=true,FILESYSTEM_PATH=/docs .env
		// envwrite KEY=VALUE[,KEY=VALUE,...] <dest>
		// Writes the given key=value pairs to <dest> in order, using setup.WriteEnv.
		// The final argument is always the destination path.
		//
		// If a backup file was created (because dest already existed), its base name
		// is printed to stdout so testscript scripts can assert its existence.
		//
		// Example: envwrite FILESYSTEM_ENABLED=true,FILESYSTEM_PATH=/docs .env
		"envwrite": func() int {
			args := os.Args[1:]
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: envwrite KEY=VALUE[,...] <dest>")
				return 1
			}
			dest := args[len(args)-1]
			kvRaw := strings.Join(args[:len(args)-1], ",")

			pairs := make(map[string]string)
			var order []string
			for _, kv := range strings.Split(kvRaw, ",") {
				kv = strings.TrimSpace(kv)
				if kv == "" {
					continue
				}
				idx := strings.IndexByte(kv, '=')
				if idx < 1 {
					fmt.Fprintf(os.Stderr, "envwrite: invalid KEY=VALUE pair: %q\n", kv)
					return 1
				}
				k, v := kv[:idx], kv[idx+1:]
				pairs[k] = v
				order = append(order, k)
			}

			// Capture whether a backup will be created before calling WriteEnv.
			destExists := false
			if _, err := os.Stat(dest); err == nil {
				destExists = true
			}

			if err := setup.WriteEnv(dest, pairs, order); err != nil {
				fmt.Fprintf(os.Stderr, "envwrite: %v\n", err)
				return 1
			}

			// If a backup was expected, find and report the backup file name so
			// testscript scripts can use "stdout" to assert it was created.
			if destExists {
				dir := filepath.Dir(dest)
				if dir == "" {
					dir = "."
				}
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					if strings.Contains(e.Name(), ".bak.") {
						fmt.Println("backup:" + e.Name())
						break
					}
				}
			}
			return 0
		},
	})
}

// TestWizardScripts runs txtar golden-file scripts from testdata/script/.
//
// To update golden files after intentional output changes:
//
//	UPDATE_GOLDEN=1 go test -tags setup ./internal/setup/... -run TestWizardScripts
func TestWizardScripts(t *testing.T) {
	t.Parallel()

	update := os.Getenv("UPDATE_GOLDEN") == "1"

	testscript.Run(t, testscript.Params{
		Dir:           "testdata/script",
		UpdateScripts: update,
	})
}
