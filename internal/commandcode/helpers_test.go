package commandcode

import (
	"errors"
	"testing"
)

func errorCode(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var target *Error
	if errors.As(err, &target) {
		return target.Code, true
	}
	return "", false
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	got, ok := errorCode(err)
	if !ok || got != code {
		t.Fatalf("err = %v, want code %q", err, code)
	}
}
