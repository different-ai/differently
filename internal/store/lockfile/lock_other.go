//go:build !unix

package lockfile

import "os"

// lock does nothing where there is no flock: on such a system nothing stops
// two servers from using one store, and they must not.
func lock(*os.File) error { return nil }
