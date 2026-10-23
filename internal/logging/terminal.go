//go:build linux || darwin

package logging

import (
	"os"
	"syscall"
	"unsafe"
)

// terminalWidth is the terminal's column count, or 0 when it cannot be read.
func terminalWidth(file *os.File) int {
	var size struct {
		rows, cols, xpixel, ypixel uint16
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0
	}
	return int(size.cols)
}
