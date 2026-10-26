package db

import (
	"sync/atomic"
	"testing"
)

// simulatedIdentityAuthorities counts the tests that currently simulate an
// external identity authority. Only SimulateIndependentIdentityForTest changes
// it, and that refuses to run outside a `go test` binary.
var simulatedIdentityAuthorities atomic.Int64

// SimulatedIdentityEnvForTest is set to "1" while a test simulates an identity
// authority, so that a test binary re-executed as a child process (such as the
// background execution worker) can tell that it belongs to such a test. The
// slb binary never reads it: setting it does not enable approvals.
const SimulatedIdentityEnvForTest = "SLB_TEST_SIMULATED_IDENTITY_AUTHORITY"

// independentIdentityAttested reports whether an authority outside the local
// database has attested reviewer and model identities. No such authority is
// implemented (issue #30), so in a production binary this is always false and
// approvals that need an independent reviewer or model fail closed with
// ErrIndependentIdentityUnavailable. testing.Testing is fixed when the binary
// is linked; it is true only in binaries built by `go test`.
func independentIdentityAttested() bool {
	return testing.Testing() && simulatedIdentityAuthorities.Load() > 0
}

// SimulateIndependentIdentityForTest makes the calling test behave as if an
// external identity authority had attested every local session as a distinct
// principal with its stated model. It exists so tests can keep exercising the
// retained peer-review machinery (quorum, self-review, different-model,
// signatures, expiry and execution claims) that an authority would rely on.
//
// It panics outside `go test` binaries: no flag, environment variable,
// configuration file or database value can enable it in the slb binary. The
// simulation is process-wide and ends when the test and its subtests finish.
// It uses t.Setenv, which the testing package refuses in parallel tests, so a
// simulating test cannot overlap a parallel test that asserts the production
// fail-closed behavior. Such tests must not call it.
func SimulateIndependentIdentityForTest(t testing.TB) {
	t.Helper()
	if !testing.Testing() {
		panic("db: SimulateIndependentIdentityForTest called outside go test")
	}
	t.Setenv(SimulatedIdentityEnvForTest, "1")
	simulatedIdentityAuthorities.Add(1)
	t.Cleanup(func() { simulatedIdentityAuthorities.Add(-1) })
}
