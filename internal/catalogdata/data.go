// Package catalogdata contains the reviewed, version-bound compatibility facts.
package catalogdata

import _ "embed"

const (
	CLIVersion   = "1.79.2"
	BundleSHA256 = "d976c2ad89f93a2b81af4d150dca99aeedfd679f15e8b8c032314e61e484da7b"
	SnapshotDate = "2026-10-11"
)

//go:embed cli.json
var CLIJSON []byte

//go:embed provider.json
var ProviderJSON []byte
