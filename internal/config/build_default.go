//go:build !experimental_adapters

package config

// experimentalBuilt is false in the default build, which leaves the
// ExperimentalAdapters out.
const experimentalBuilt = false
