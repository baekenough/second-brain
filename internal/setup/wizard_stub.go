//go:build !setup

package setup

import "errors"

// Run is a stub that informs the user they must rebuild with -tags setup.
// This stub is compiled into the default (production) binary so that
// cmd/collector/main.go can always import the setup package without
// pulling in the charm.land/huh/v2 TUI dependency.
func Run(_ []string) error {
	return errors.New(msg("stub_error"))
}
