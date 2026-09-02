//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// winsize asks the terminal how wide it is. TIOCGWINSZ is the only thing here
// that needs a syscall, and it needs eight lines, so it does not need
// golang.org/x/term.
func winsize(f *os.File) int {
	var ws struct{ Row, Col, Xpixel, Ypixel uint16 }
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.Col)
}
