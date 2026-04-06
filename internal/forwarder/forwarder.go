package forwarder

import (
	"io"
	"net"
	"sync"
)

// Forward performs bidirectional TCP copy between client and backend.
// Returns when either side closes or errors.
func Forward(client, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	copy := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src) //nolint:errcheck
		// Signal the other direction to stop by closing the write side.
		if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite() //nolint:errcheck
		} else {
			dst.Close()
		}
	}

	go copy(backend, client)
	go copy(client, backend)
	wg.Wait()
}
