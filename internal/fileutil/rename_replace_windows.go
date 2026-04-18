//go:build windows

package fileutil

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procMoveFileExW = kernel32.NewProc("MoveFileExW")
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

// RenameReplace renames src onto dst, replacing an existing destination.
// Windows rename does not overwrite by default, so use MoveFileExW with
// MOVEFILE_REPLACE_EXISTING.
func RenameReplace(src, dst string) error {
	srcPtr, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dstPtr, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	r1, _, e1 := procMoveFileExW.Call(
		uintptr(unsafe.Pointer(srcPtr)),
		uintptr(unsafe.Pointer(dstPtr)),
		uintptr(moveFileReplaceExisting|moveFileWriteThrough),
	)
	if r1 != 0 {
		return nil
	}
	if e1 != syscall.Errno(0) {
		return e1
	}
	// MoveFileExW reported failure (r1 == 0) but the syscall stub did not
	// surface an errno. This shouldn't happen under documented Win32
	// behavior, so don't disguise it as EINVAL (which callers may pattern-
	// match against to mean "bad argument"). Return a distinct wrapped
	// error so operators see the real shape in logs.
	return fmt.Errorf("MoveFileExW(%q -> %q) failed with no errno", src, dst)
}
