package mobile

import "testing"

// Compile-time contract: gomobile callers must receive hcore.Close failures.
var _ func(int) error = Close

func TestCloseExposesErrorResult(t *testing.T) {}
