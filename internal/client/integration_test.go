package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"gonet/internal/server"
)

func TestClientCreatesTunnelToPeerTCPService(t *testing.T) {
	runTunnelTest(t, false)
}

func TestClientCreatesRelayTunnelToPeerTCPService(t *testing.T) {
	runTunnelTest(t, true)
}

func runTunnelTest(t *testing.T, forceRelay bool) {
	serverPort := reserveLocalPort(t)
	serverAddr := fmt.Sprintf("127.0.0.1:%d", serverPort)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := server.New(server.Config{
		Listen:           serverAddr,
		ClientStaleAfter: 30 * time.Second,
		RequestTTL:       30 * time.Second,
		CleanupInterval:  5 * time.Second,
		PredictSpread:    4,
	})

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.Run(ctx)
	}()

	waitFor(t, 3*time.Second, func() bool {
		conn, err := net.DialTimeout("tcp", serverAddr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	})

	echoLn, echoPort := startTCPEchoServer(t)
	defer echoLn.Close()

	initiator, err := New(Config{
		ServerAddr:    serverAddr,
		Group:         "it-test",
		ControlProto:  "udp",
		PunchTimeout:  5 * time.Second,
		PredictSpread: 4,
		ForceRelay:    forceRelay,
	})
	if err != nil {
		t.Fatalf("create initiator client: %v", err)
	}
	acceptor, err := New(Config{
		ServerAddr:    serverAddr,
		Group:         "it-test",
		ControlProto:  "udp",
		PunchTimeout:  5 * time.Second,
		PredictSpread: 4,
		ForceRelay:    forceRelay,
	})
	if err != nil {
		t.Fatalf("create acceptor client: %v", err)
	}

	initInR, initInW := io.Pipe()
	defer initInW.Close()
	defer initInR.Close()
	accInR, accInW := io.Pipe()
	defer accInW.Close()
	defer accInR.Close()

	initOut := &lockedBuffer{}
	accOut := &lockedBuffer{}

	initDone := make(chan error, 1)
	accDone := make(chan error, 1)

	go func() {
		initDone <- initiator.Run(ctx, initInR, initOut)
	}()
	go func() {
		accDone <- acceptor.Run(ctx, accInR, accOut)
	}()

	waitFor(t, 5*time.Second, func() bool {
		return initiator.id != "" && acceptor.id != ""
	})

	if _, err := io.WriteString(initInW, fmt.Sprintf("connect %s %d\n", acceptor.id, echoPort)); err != nil {
		t.Fatalf("write connect command: %v", err)
	}

	waitFor(t, 10*time.Second, func() bool {
		initiator.tunnelsMu.Lock()
		defer initiator.tunnelsMu.Unlock()
		return len(initiator.tunnels) > 0
	})

	var localPort int
	initiator.tunnelsMu.Lock()
	for _, tunnel := range initiator.tunnels {
		localPort = tunnel.LocalPort
	}
	initiator.tunnelsMu.Unlock()

	if localPort == 0 {
		t.Fatalf("expected local forwarded port, output:\n%s\n%s", initOut.String(), accOut.String())
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 3*time.Second)
	if err != nil {
		t.Fatalf("dial local forwarded port %d: %v\ninitiator output:\n%s\nacceptor output:\n%s",
			localPort, err, initOut.String(), accOut.String())
	}
	defer conn.Close()

	message := "hello through tunnel\n"
	if _, err := io.WriteString(conn, message); err != nil {
		t.Fatalf("write tunnel message: %v", err)
	}

	reply := make([]byte, len(message))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read tunnel reply: %v\ninitiator output:\n%s\nacceptor output:\n%s",
			err, initOut.String(), accOut.String())
	}

	if string(reply) != message {
		t.Fatalf("unexpected reply %q, want %q", string(reply), message)
	}

	cancel()
	_ = initInW.Close()
	_ = accInW.Close()

	select {
	case err := <-initDone:
		if err != nil {
			t.Fatalf("initiator exited with error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("initiator did not exit")
	}

	select {
	case err := <-accDone:
		if err != nil {
			t.Fatalf("acceptor exited with error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("acceptor did not exit")
	}

	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server exited with error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not exit")
	}
}

func reserveLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func startTCPEchoServer(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp echo: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(conn)
		}
	}()

	return ln, ln.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not satisfied before timeout")
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Clone(b.buf.String())
}
