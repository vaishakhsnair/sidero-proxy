//go:build linux

package source

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

type TransparentDialer struct{}

func (TransparentDialer) Dial(ctx context.Context, sourceIP, destIP string, destPort int) (net.Conn, error) {
	src := net.ParseIP(sourceIP)
	if src == nil {
		return nil, fmt.Errorf("invalid source ip %q", sourceIP)
	}

	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: src},
		Control: func(network, address string, c syscall.RawConn) error {
			var controlErr error
			if err := c.Control(func(fd uintptr) {
				if err := unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_TRANSPARENT, 1); err != nil {
					controlErr = fmt.Errorf("set IP_TRANSPARENT: %w", err)
					return
				}
				if err := unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1); err != nil {
					controlErr = fmt.Errorf("set IP_FREEBIND: %w", err)
				}
			}); err != nil {
				return err
			}
			return controlErr
		},
	}

	return dialer.DialContext(ctx, "tcp", net.JoinHostPort(destIP, strconv.Itoa(destPort)))
}
