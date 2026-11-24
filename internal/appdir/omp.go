package appdir

import "path/filepath"

// OMPConfigRoot is OMP's profile-independent config root.
func OMPConfigRoot(home string) string {
	name := Getenv("PI_CONFIG_DIR")
	if name == "" {
		name = ".omp"
	}
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	return filepath.Join(home, name)
}
