// file: internal/langs/embed.go
package langs

import "embed"

//go:embed all:builtin
var builtinFS embed.FS

// LoadBuiltin loads the manifests compiled into the binary. Copy languages/*.yaml into
// internal/langs/builtin/ during the build (see the Makefile target below) so a deployed
// image needs no external config to start.
func LoadBuiltin() (*Registry, error) {
	r := NewRegistry()
	if err := r.LoadFS(builtinFS, "builtin"); err != nil {
		return nil, err
	}
	return r, nil
}
