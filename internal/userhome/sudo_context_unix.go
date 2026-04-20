//go:build !windows

package userhome

import (
	"os"
	"strings"
)

func isSudoRootContext() bool {
	return os.Geteuid() == 0 &&
		strings.TrimSpace(os.Getenv("SUDO_USER")) != "" &&
		strings.TrimSpace(os.Getenv("SUDO_UID")) != ""
}
