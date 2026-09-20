package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestNewPostgresParseErrorsDoNotExposeCredentials(t *testing.T) {
	for _, dsn := range []string{
		"postgres://brain:prefix/secret:tail+value=@localhost:5432/brain",
		"postgres://brain:secret%ZZ@localhost:5432/brain",
		"postgres://brain:secret@localhost:invalid/brain",
		"postgres://brain:secret@localhost/brain?pool_max_conns=secret",
		"host=localhost password='secret",
	} {
		pg, err := NewPostgres(context.Background(), dsn)
		if pg != nil || err == nil {
			t.Fatal("invalid connection configuration must fail before connecting")
		}
		const want = "invalid database connection configuration; check DATABASE_URL and PG* settings"
		if got := fmt.Sprintf("%+v", err); got != want {
			t.Errorf("unexpected diagnostic: %s", got)
		}
		if errors.Unwrap(err) != nil {
			t.Error("original parse error must not be recoverable")
		}
	}
}
