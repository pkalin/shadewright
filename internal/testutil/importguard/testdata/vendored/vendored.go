// Package vendored imports a package outside the standard library and this module, for the guard's own test.
package vendored

import (
	_ "fmt"

	_ "k8s.io/api/core/v1"
)
