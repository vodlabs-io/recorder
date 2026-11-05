package flv

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/vodlabs-io/recorder/internal/netguard"
)

func Client() *http.Client {
	return &http.Client{Transport: transport(nil)}
}

// GuardedClient is Client for addresses others supply: it dials only the
// public internet, or the proxy the environment configures (netguard.Transport).
func GuardedClient() *http.Client {
	return &http.Client{Transport: netguard.Transport(transport(netguard.ControlContext))}
}

// transport is the FLV transport, with control as its dialer's ControlContext.
func transport(control func(ctx context.Context, network, address string, c syscall.RawConn) error) *http.Transport {
	connTimeout := 10 * time.Second
	readTimeout := 5 * time.Second

	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{
				Timeout:        connTimeout,
				KeepAlive:      30 * time.Second,
				ControlContext: control,
			}
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &timeoutConn{
				Conn:  conn,
				read:  readTimeout,
				write: 15 * time.Second,
			}, nil
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	return t
}

// timeoutConn wraps a net.Conn with read and write timeouts
type timeoutConn struct {
	net.Conn
	read  time.Duration
	write time.Duration
}

func (c *timeoutConn) Read(b []byte) (int, error) {
	if c.read > 0 {
		c.Conn.SetReadDeadline(time.Now().Add(c.read))
	}
	return c.Conn.Read(b)
}

func (c *timeoutConn) Write(b []byte) (int, error) {
	if c.write > 0 {
		c.Conn.SetWriteDeadline(time.Now().Add(c.write))
	}
	return c.Conn.Write(b)
}
