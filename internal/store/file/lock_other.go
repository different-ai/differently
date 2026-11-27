//go:build !unix

package file

import "os"

// lockFile does nothing where there is no flock: on such a system nothing
// stops two servers from using one directory, and they must not.
func lockFile(*os.File) error { return nil }
