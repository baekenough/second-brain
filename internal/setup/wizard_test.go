//go:build setup

package setup_test

import (
	"os"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	os.Exit(m.Run())
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
