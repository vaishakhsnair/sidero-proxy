//go:build !linux

package source

import (
	"context"
	"fmt"
	"net"
)

type TransparentDialer struct{}

func (TransparentDialer) Dial(_ context.Context, _, _ string, _ int) (net.Conn, error) {
	return nil, fmt.Errorf("transparent dial is only supported on linux")
}
