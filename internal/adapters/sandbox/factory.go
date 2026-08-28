// file: internal/adapters/sandbox/factory.go
package sandbox

import (
	"fmt"

	"github.com/AnishPrakash/arena/internal/ports"
)

// New selects a sandbox backend by name. This one function is the entire cost of swapping
// isolation technology — the documented upgrade path to gVisor is a new case here plus
// `--runtime=runsc` in the docker adapter's arg list.
func New(driver, env string) (ports.Sandbox, error) {
	switch driver {
	case "docker", "":
		return NewDocker(), nil
	case "process":
		if env == "prod" {
			return nil, fmt.Errorf("sandbox: the process driver provides NO isolation and is forbidden in prod")
		}
		return NewProcess(), nil
	default:
		return nil, fmt.Errorf("sandbox: unknown driver %q", driver)
	}
}
