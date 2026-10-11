// Package static contains dependency-free checks over the delivery artifacts
// (CI workflows, registry, packaging scripts and configuration examples). It
// imports neither the business packages nor the CLIProxyAPI SDK, so it can run
// before the CLI/backend/UI slices are merged.
package static
