package vlcli

import (
	"context"
	"fmt"
	"net"
	"testing"
)

func TestDaemonDownOnRefusedDial(t *testing.T) {
	// Grab a free port, then close it so nothing listens there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("nats://%s", ln.Addr())
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err = initNATS(ctx, url, cancel, func() bool { return true })
	if err == nil {
		t.Fatal("connect to a closed port succeeded")
	}
	if !daemonDown(err) {
		t.Fatalf("daemonDown(%v) = false, want true", err)
	}
}
