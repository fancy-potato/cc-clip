//go:build windows

package userhome

func isSudoRootContext() bool {
	return false
}
