// Package catalogdata contains the reviewed, version-bound compatibility facts.
package catalogdata

import _ "embed"

const (
	CLIVersion   = "1.88.5"
	BundleSHA256 = "c75b34df6487de034c4419b8919432309c966505c20b76a361dea9db80cda2fc"
	SnapshotDate = "2026-11-09"
)

//go:embed cli.json
var CLIJSON []byte

//go:embed provider.json
var ProviderJSON []byte
