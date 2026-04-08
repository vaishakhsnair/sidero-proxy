//go:build linux

package origdst

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func Get(conn net.Conn) (string, int, error) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return "", 0, fmt.Errorf("not a TCP connection")
	}

	raw, err := tc.SyscallConn()
	if err != nil {
		return "", 0, fmt.Errorf("syscall conn: %w", err)
	}

	var addr syscall.RawSockaddrInet4
	var sockErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		size := uint32(unsafe.Sizeof(addr))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			unix.SOL_IP,
			unix.SO_ORIGINAL_DST,
			uintptr(unsafe.Pointer(&addr)),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			sockErr = errno
		}
	})
	if ctrlErr != nil {
		return "", 0, fmt.Errorf("control socket: %w", ctrlErr)
	}
	if sockErr != nil {
		return "", 0, fmt.Errorf("getsockopt SO_ORIGINAL_DST: %w", sockErr)
	}

	ip := net.IP(addr.Addr[:]).String()
	port := int(addr.Port>>8) | int(addr.Port&0xff)<<8
	return ip, port, nil
}
