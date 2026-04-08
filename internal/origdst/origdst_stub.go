//go:build !linux

package origdst

import (
	"fmt"
	"net"
)

func Get(_ net.Conn) (string, int, error) {
	return "", 0, fmt.Errorf("original destination lookup is only supported on linux")
}
