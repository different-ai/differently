//go:build !experimental_adapters

package wiring

// registerExperimental is empty in the default build: the experimental
// adapter kinds are compiled in only with -tags experimental_adapters.
func registerExperimental() {}
