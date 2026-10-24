package vlcli

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// Nothing listening is unhealthy, and the probe returns rather than starting
// a daemon.
func TestPingDaemon_Down(t *testing.T) {
	start := time.Now()
	url := "nats://127.0.0.1:" + strconv.Itoa(freePort(t))
	if err := pingDaemon(url); err == nil {
		t.Fatal("ping succeeded with no daemon")
	}
	if time.Since(start) > 2*healthTimeout {
		t.Error("ping did not give up promptly")
	}
}

// A NATS server without the API service is not a healthy daemon.
func TestPingDaemon_ServiceRequired(t *testing.T) {
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	defer ns.Shutdown()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	if err := pingDaemon(ns.ClientURL()); err == nil {
		t.Fatal("ping succeeded without the API service")
	}

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := micro.AddService(nc, micro.Config{Name: vlapi.ServiceName, Version: "0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := pingDaemon(ns.ClientURL()); err != nil {
		t.Fatalf("ping failed with the API service up: %v", err)
	}
}
