package forwarder

import (
	"io"
	"net"
	"sync"
)

func Proxy(client, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
			return
		}
		_ = dst.Close()
	}

	go pipe(backend, client)
	go pipe(client, backend)
	wg.Wait()
}
