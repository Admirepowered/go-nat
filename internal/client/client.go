package client

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"

	"gonet/internal/proto"
)

type Config struct {
	ServerAddr    string
	Group         string
	ControlProto  string
	PunchTimeout  time.Duration
	PredictSpread int
	ForceRelay    bool
}

type Client struct {
	cfg          Config
	serverUDP    *net.UDPAddr
	control      controlTransport
	id           string
	group        string
	controlProto string

	outMu sync.Mutex
	out   io.Writer

	membersMu sync.RWMutex
	members   []proto.MemberSummary

	pendingMu sync.Mutex
	pending   map[string]pendingConnect

	sessionsMu sync.Mutex
	sessions   map[string]*sessionState

	tunnelsMu sync.Mutex
	tunnels   map[string]*Tunnel

	relayMu    sync.Mutex
	relayConns map[string]*relayPacketConn
}

type pendingConnect struct {
	Token       string
	TargetSpec  string
	TargetPort  int
	TargetProto string
	LocalPort   int
	CreatedAt   time.Time
}

type sessionState struct {
	RequestID   string
	ClientToken string
	Role        string
	PeerID      string
	PeerIndex   int
	TargetPort  int
	TargetProto string
	LocalPort   int
	Conn        *net.UDPConn

	startMu sync.Mutex
	started bool
}

type Tunnel struct {
	RequestID  string
	PeerID     string
	Proto      string
	TargetPort int
	LocalPort  int
	StartedAt  time.Time
	Mode       string
}

type relayPacketConn struct {
	client    *Client
	requestID string
	peerAddr  net.Addr
	localAddr net.Addr

	readCh  chan []byte
	closeCh chan struct{}

	deadlineMu    sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
}

type relayAddr struct {
	value string
}

var errRelayRequested = errors.New("relay requested")

type controlTransport interface {
	Send(proto.Message) error
	Recv() (proto.Message, error)
	Close() error
}

type udpControl struct {
	conn    *net.UDPConn
	writeMu sync.Mutex
}

type tcpControl struct {
	conn    net.Conn
	decoder *json.Decoder
	writeMu sync.Mutex
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.ServerAddr) == "" {
		return nil, errors.New("server address is required")
	}
	if strings.TrimSpace(cfg.Group) == "" {
		return nil, errors.New("group is required")
	}
	if cfg.PunchTimeout <= 0 {
		cfg.PunchTimeout = 8 * time.Second
	}
	if cfg.PredictSpread <= 0 {
		cfg.PredictSpread = 4
	}
	serverUDP, err := net.ResolveUDPAddr("udp", cfg.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve server udp addr: %w", err)
	}
	cfg.ControlProto = proto.NormalizeControlProto(cfg.ControlProto)
	return &Client{
		cfg:        cfg,
		serverUDP:  serverUDP,
		pending:    make(map[string]pendingConnect),
		sessions:   make(map[string]*sessionState),
		tunnels:    make(map[string]*Tunnel),
		relayConns: make(map[string]*relayPacketConn),
	}, nil
}

func (c *Client) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	c.out = out
	control, err := c.dialControl()
	if err != nil {
		return err
	}
	c.control = control
	defer c.control.Close()

	if err := c.control.Send(proto.Message{
		Type:         proto.TypeRegister,
		Group:        c.cfg.Group,
		ControlProto: c.cfg.ControlProto,
	}); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	first, err := c.control.Recv()
	if err != nil {
		return fmt.Errorf("read register response: %w", err)
	}
	if first.Type == proto.TypeError {
		return errors.New(first.Error)
	}
	if first.Type != proto.TypeRegisterOK {
		return fmt.Errorf("unexpected first response %q", first.Type)
	}

	c.id = first.ClientID
	c.group = first.Group
	c.controlProto = first.ControlProto
	c.setMembers(first.Members)

	c.printf("client id: %s | group: %s | control: %s | server: %s", c.id, c.group, c.controlProto, c.cfg.ServerAddr)
	c.printMembers("current members")
	c.printHelp()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go c.controlLoop(runCtx, cancel)
	go c.heartbeatLoop(runCtx)

	err = c.repl(runCtx, in)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (c *Client) dialControl() (controlTransport, error) {
	switch c.cfg.ControlProto {
	case "tcp":
		conn, err := net.Dial("tcp", c.cfg.ServerAddr)
		if err != nil {
			return nil, fmt.Errorf("dial tcp control: %w", err)
		}
		return &tcpControl{conn: conn, decoder: json.NewDecoder(conn)}, nil
	default:
		conn, err := net.DialUDP("udp", nil, c.serverUDP)
		if err != nil {
			return nil, fmt.Errorf("dial udp control: %w", err)
		}
		return &udpControl{conn: conn}, nil
	}
}

func (c *Client) controlLoop(ctx context.Context, cancel context.CancelFunc) {
	for {
		msg, err := c.control.Recv()
		if err != nil {
			if ctx.Err() == nil {
				c.printf("control connection closed: %v", err)
			}
			cancel()
			return
		}
		c.handleMessage(msg)
	}
}

func (c *Client) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.control.Send(proto.Message{Type: proto.TypeHeartbeat, ClientID: c.id})
		}
	}
}

func (c *Client) repl(ctx context.Context, in io.Reader) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		c.writePrompt()
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return err
			}
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if err := c.handleCommand(line); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			c.printf("command error: %v", err)
		}
	}
}

func (c *Client) handleCommand(line string) error {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}

	switch strings.ToLower(parts[0]) {
	case "help", "?":
		c.printHelp()
		return nil
	case "info":
		c.printf("id=%s group=%s control=%s server=%s", c.id, c.group, c.controlProto, c.cfg.ServerAddr)
		return nil
	case "list", "members":
		if err := c.control.Send(proto.Message{Type: proto.TypeList, ClientID: c.id}); err != nil {
			return err
		}
		c.printMembers("cached members")
		return nil
	case "tunnels":
		c.printTunnels()
		return nil
	case "connect":
		return c.handleConnectCommand(parts[1:])
	case "quit", "exit":
		return io.EOF
	default:
		return fmt.Errorf("unknown command %q", parts[0])
	}
}

func (c *Client) handleConnectCommand(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: connect <peer-id|#index> <target-port> [local-port] [tcp|udp]")
	}
	targetSpec := strings.TrimSpace(args[0])
	targetPort, err := strconv.Atoi(args[1])
	if err != nil || targetPort <= 0 || targetPort > 65535 {
		return fmt.Errorf("invalid target port %q", args[1])
	}

	localPort := 0
	targetProto := proto.DefaultAppProto
	if len(args) >= 3 {
		if parsedPort, portErr := strconv.Atoi(args[2]); portErr == nil {
			if parsedPort < 0 || parsedPort > 65535 {
				return fmt.Errorf("invalid local port %q", args[2])
			}
			localPort = parsedPort
		} else {
			targetProto = proto.NormalizeAppProto(args[2])
		}
	}
	if len(args) >= 4 {
		targetProto = proto.NormalizeAppProto(args[3])
	}

	token := randomHex(5)
	request := proto.Message{
		Type:        proto.TypeConnectReq,
		ClientID:    c.id,
		ClientToken: token,
		TargetPort:  targetPort,
		TargetProto: targetProto,
		LocalPort:   localPort,
	}
	if strings.HasPrefix(targetSpec, "#") {
		targetIndex, err := strconv.Atoi(strings.TrimPrefix(targetSpec, "#"))
		if err != nil || targetIndex <= 0 {
			return fmt.Errorf("invalid target index %q", targetSpec)
		}
		request.TargetIndex = targetIndex
	} else {
		request.TargetID = targetSpec
	}

	c.pendingMu.Lock()
	c.pending[token] = pendingConnect{
		Token:       token,
		TargetSpec:  targetSpec,
		TargetPort:  targetPort,
		TargetProto: targetProto,
		LocalPort:   localPort,
		CreatedAt:   time.Now(),
	}
	c.pendingMu.Unlock()

	if err := c.control.Send(request); err != nil {
		return err
	}

	c.printf("connect request sent: peer=%s target=%d/%s local=%s",
		targetSpec, targetPort, targetProto, formatLocalPort(localPort))
	return nil
}

func (c *Client) handleMessage(msg proto.Message) {
	switch msg.Type {
	case proto.TypeNotice:
		if msg.Message != "" {
			c.printf(msg.Message)
		}
	case proto.TypeError:
		if msg.Error != "" {
			c.printf("server error: %s", msg.Error)
		}
	case proto.TypeListResp:
		c.setMembers(msg.Members)
		c.printMembers("members")
	case proto.TypePrepareDial:
		if err := c.handlePrepareDial(msg); err != nil {
			c.printf("prepare dial failed: %v", err)
		}
	case proto.TypePrepareAccept:
		if err := c.handlePrepareAccept(msg); err != nil {
			c.printf("prepare accept failed: %v", err)
		}
	case proto.TypeConnectPlan:
		if err := c.handleConnectPlan(msg); err != nil {
			c.printf("connect plan failed: %v", err)
		}
	case proto.TypeRelayStart:
		if err := c.handleRelayStart(msg); err != nil {
			c.printf("relay start failed: %v", err)
		}
	case proto.TypeRelayPacket:
		if err := c.handleRelayPacket(msg); err != nil {
			c.printf("relay packet failed: %v", err)
		}
	default:
		if msg.Message != "" {
			c.printf("%s", msg.Message)
		}
	}
}

func (c *Client) handlePrepareDial(msg proto.Message) error {
	localPort := msg.LocalPort
	if msg.ClientToken != "" {
		c.pendingMu.Lock()
		if pending, ok := c.pending[msg.ClientToken]; ok {
			if pending.LocalPort != 0 {
				localPort = pending.LocalPort
			}
			delete(c.pending, msg.ClientToken)
		}
		c.pendingMu.Unlock()
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fmt.Errorf("open dial udp socket: %w", err)
	}

	session := &sessionState{
		RequestID:   msg.RequestID,
		ClientToken: msg.ClientToken,
		Role:        "dial",
		PeerID:      msg.PeerID,
		PeerIndex:   msg.PeerIndex,
		TargetPort:  msg.TargetPort,
		TargetProto: proto.NormalizeAppProto(msg.TargetProto),
		LocalPort:   localPort,
		Conn:        conn,
	}
	c.sessionsMu.Lock()
	c.sessions[msg.RequestID] = session
	c.sessionsMu.Unlock()

	if err := c.sendSessionProbe(session); err != nil {
		return err
	}

	c.printf("request %s prepared as dialer, waiting for peer endpoint", msg.RequestID)
	return nil
}

func (c *Client) handlePrepareAccept(msg proto.Message) error {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fmt.Errorf("open accept udp socket: %w", err)
	}

	session := &sessionState{
		RequestID:   msg.RequestID,
		Role:        "accept",
		PeerID:      msg.PeerID,
		PeerIndex:   msg.PeerIndex,
		TargetPort:  msg.TargetPort,
		TargetProto: proto.NormalizeAppProto(msg.TargetProto),
		Conn:        conn,
	}
	c.sessionsMu.Lock()
	c.sessions[msg.RequestID] = session
	c.sessionsMu.Unlock()

	if err := c.sendSessionProbe(session); err != nil {
		return err
	}

	c.printf("request %s accepted for peer %s, preparing local target %d/%s",
		msg.RequestID, msg.PeerID, msg.TargetPort, msg.TargetProto)
	return nil
}

func (c *Client) handleConnectPlan(msg proto.Message) error {
	c.sessionsMu.Lock()
	session := c.sessions[msg.RequestID]
	c.sessionsMu.Unlock()
	if session == nil {
		return fmt.Errorf("session %s not found", msg.RequestID)
	}

	session.startMu.Lock()
	if session.started {
		session.startMu.Unlock()
		return nil
	}
	session.started = true
	session.startMu.Unlock()

	go c.runSession(session, msg)
	return nil
}

func (c *Client) runSession(session *sessionState, msg proto.Message) {
	switch session.Role {
	case "dial":
		if err := c.runDialSession(session, msg); err != nil {
			c.printf("dial session %s failed: %v", session.RequestID, err)
			_ = session.Conn.Close()
		}
	case "accept":
		if err := c.runAcceptSession(session, msg); err != nil {
			c.printf("accept session %s failed: %v", session.RequestID, err)
			_ = session.Conn.Close()
		}
	}
}

func (c *Client) runDialSession(session *sessionState, msg proto.Message) error {
	var (
		mux  *smux.Session
		mode string
		err  error
	)

	if c.cfg.ForceRelay {
		mux, err = c.startRelayDial(session)
		mode = "relay"
	} else {
		mux, mode, err = c.startDialTransport(session, msg)
	}
	if err != nil {
		return err
	}

	localPort, err := c.startLocalForwarder(session, mux)
	if err != nil {
		_ = mux.Close()
		return err
	}

	c.tunnelsMu.Lock()
	c.tunnels[session.RequestID] = &Tunnel{
		RequestID:  session.RequestID,
		PeerID:     session.PeerID,
		Proto:      session.TargetProto,
		TargetPort: session.TargetPort,
		LocalPort:  localPort,
		StartedAt:  time.Now(),
		Mode:       mode,
	}
	c.tunnelsMu.Unlock()

	c.printf("tunnel ready (%s): 127.0.0.1:%d -> %s:%d/%s",
		mode, localPort, session.PeerID, session.TargetPort, session.TargetProto)
	return nil
}

func (c *Client) runAcceptSession(session *sessionState, msg proto.Message) error {
	var (
		mux  *smux.Session
		mode string
		err  error
	)

	if c.cfg.ForceRelay {
		mux, err = c.startRelayAccept(session)
		mode = "relay"
	} else {
		mux, mode, err = c.startAcceptTransport(session, msg)
	}
	if err != nil {
		return err
	}

	c.printf("peer %s connected for target %d/%s via %s", session.PeerID, session.TargetPort, session.TargetProto, mode)
	go c.serveIncomingStreams(mux)
	return nil
}

func (c *Client) startLocalForwarder(session *sessionState, mux *smux.Session) (int, error) {
	switch proto.NormalizeAppProto(session.TargetProto) {
	case "udp":
		return c.startUDPForwarder(session, mux)
	default:
		return c.startTCPForwarder(session, mux)
	}
}

func (c *Client) startDialTransport(session *sessionState, msg proto.Message) (*smux.Session, string, error) {
	candidates, err := buildCandidateAddrs(msg.Endpoint, msg.PredictedPorts)
	if err != nil {
		return nil, "", err
	}

	remoteAddr, punchErr := punchPhase(session.Conn, session.RequestID, c.id, session.Role, candidates, c.cfg.PunchTimeout)
	if punchErr == nil {
		time.Sleep(250 * time.Millisecond)
		drainUDP(session.Conn, 150*time.Millisecond)

		kcpConn, err := kcp.NewConn2(remoteAddr, nil, 0, 0, session.Conn)
		if err != nil {
			return nil, "", fmt.Errorf("create kcp dial session: %w", err)
		}
		configureKCPSession(kcpConn)

		mux, err := smux.Client(kcpConn, nil)
		if err != nil {
			_ = kcpConn.Close()
			return nil, "", fmt.Errorf("create smux client: %w", err)
		}
		return mux, "p2p", nil
	}

	c.printf("request %s punch failed, switching to relay: %v", session.RequestID, punchErr)
	mux, err := c.startRelayDial(session)
	if err != nil {
		return nil, "", err
	}
	return mux, "relay", nil
}

func (c *Client) startAcceptTransport(session *sessionState, msg proto.Message) (*smux.Session, string, error) {
	candidates, err := buildCandidateAddrs(msg.Endpoint, msg.PredictedPorts)
	if err != nil {
		return nil, "", err
	}

	if _, err := punchPhase(session.Conn, session.RequestID, c.id, session.Role, candidates, c.cfg.PunchTimeout); err != nil {
		c.printf("request %s accept-side punch failed, waiting for relay: %v", session.RequestID, err)
		mux, relayErr := c.startRelayAccept(session)
		if relayErr != nil {
			return nil, "", relayErr
		}
		return mux, "relay", nil
	}

	time.Sleep(250 * time.Millisecond)
	drainUDP(session.Conn, 150*time.Millisecond)

	listener, err := kcp.ServeConn(nil, 0, 0, session.Conn)
	if err != nil {
		return nil, "", fmt.Errorf("create kcp listener: %w", err)
	}
	_ = listener.SetDeadline(time.Now().Add(c.cfg.PunchTimeout + time.Second))

	kcpConn, err := listener.AcceptKCP()
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			c.printf("request %s accept timeout, switching to relay", session.RequestID)
			_ = listener.Close()
			mux, relayErr := c.startRelayAccept(session)
			if relayErr != nil {
				return nil, "", relayErr
			}
			return mux, "relay", nil
		}
		return nil, "", fmt.Errorf("accept kcp: %w", err)
	}
	configureKCPSession(kcpConn)

	mux, err := smux.Server(kcpConn, nil)
	if err != nil {
		_ = kcpConn.Close()
		return nil, "", fmt.Errorf("create smux server: %w", err)
	}
	return mux, "p2p", nil
}

func (c *Client) startRelayDial(session *sessionState) (*smux.Session, error) {
	conn, created := c.getOrCreateRelayConn(session.RequestID, session.PeerID)
	if created {
		if err := c.control.Send(proto.Message{
			Type:      proto.TypeRelayStart,
			ClientID:  c.id,
			RequestID: session.RequestID,
		}); err != nil {
			return nil, fmt.Errorf("request relay start: %w", err)
		}
	}

	kcpConn, err := kcp.NewConn2(conn.peerAddr, nil, 0, 0, conn)
	if err != nil {
		return nil, fmt.Errorf("create relay kcp dial session: %w", err)
	}
	configureKCPSession(kcpConn)

	mux, err := smux.Client(kcpConn, nil)
	if err != nil {
		_ = kcpConn.Close()
		return nil, fmt.Errorf("create relay smux client: %w", err)
	}
	return mux, nil
}

func (c *Client) startRelayAccept(session *sessionState) (*smux.Session, error) {
	conn, created := c.getOrCreateRelayConn(session.RequestID, session.PeerID)
	if created {
		if err := c.control.Send(proto.Message{
			Type:      proto.TypeRelayStart,
			ClientID:  c.id,
			RequestID: session.RequestID,
		}); err != nil {
			return nil, fmt.Errorf("request relay start: %w", err)
		}
	}

	listener, err := kcp.ServeConn(nil, 0, 0, conn)
	if err != nil {
		return nil, fmt.Errorf("create relay listener: %w", err)
	}
	_ = listener.SetDeadline(time.Now().Add(15 * time.Second))

	kcpConn, err := listener.AcceptKCP()
	if err != nil {
		return nil, fmt.Errorf("accept relay kcp: %w", err)
	}
	configureKCPSession(kcpConn)

	mux, err := smux.Server(kcpConn, nil)
	if err != nil {
		_ = kcpConn.Close()
		return nil, fmt.Errorf("create relay smux server: %w", err)
	}
	return mux, nil
}

func (c *Client) startTCPForwarder(session *sessionState, mux *smux.Session) (int, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", session.LocalPort))
	if err != nil {
		return 0, fmt.Errorf("listen local tcp: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port

	go func() {
		defer listener.Close()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(localConn net.Conn) {
				stream, err := mux.OpenStream()
				if err != nil {
					c.printf("open smux stream failed: %v", err)
					_ = localConn.Close()
					return
				}
				header := proto.StreamHeader{Proto: "tcp", TargetPort: session.TargetPort}
				if err := proto.WriteJSONFrame(stream, header); err != nil {
					c.printf("write stream header failed: %v", err)
					_ = stream.Close()
					_ = localConn.Close()
					return
				}
				pipeBoth(localConn, stream)
			}(conn)
		}
	}()

	return localPort, nil
}

func (c *Client) startUDPForwarder(session *sessionState, mux *smux.Session) (int, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: session.LocalPort})
	if err != nil {
		return 0, fmt.Errorf("listen local udp: %w", err)
	}
	localPort := conn.LocalAddr().(*net.UDPAddr).Port

	type localUDPStream struct {
		stream *smux.Stream
		addr   *net.UDPAddr
	}

	var (
		mu      sync.Mutex
		streams = make(map[string]*localUDPStream)
	)

	go func() {
		defer conn.Close()
		buf := make([]byte, 64*1024)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}

			key := addr.String()
			mu.Lock()
			item := streams[key]
			if item == nil {
				stream, err := mux.OpenStream()
				if err == nil {
					header := proto.StreamHeader{Proto: "udp", TargetPort: session.TargetPort}
					err = proto.WriteJSONFrame(stream, header)
				}
				if err != nil {
					mu.Unlock()
					c.printf("open udp stream failed: %v", err)
					continue
				}
				item = &localUDPStream{stream: stream, addr: cloneUDPAddr(addr)}
				streams[key] = item
				go func(key string, stream *smux.Stream, targetAddr *net.UDPAddr) {
					defer func() {
						_ = stream.Close()
						mu.Lock()
						delete(streams, key)
						mu.Unlock()
					}()
					for {
						frame, err := proto.ReadFrame(stream)
						if err != nil {
							return
						}
						if _, err := conn.WriteToUDP(frame, targetAddr); err != nil {
							return
						}
					}
				}(key, stream, cloneUDPAddr(addr))
			}
			mu.Unlock()

			if err := proto.WriteFrame(item.stream, append([]byte(nil), buf[:n]...)); err != nil {
				c.printf("write udp frame failed: %v", err)
			}
		}
	}()

	return localPort, nil
}

func (c *Client) serveIncomingStreams(mux *smux.Session) {
	for {
		stream, err := mux.AcceptStream()
		if err != nil {
			return
		}
		go c.handleIncomingStream(stream)
	}
}

func (c *Client) handleIncomingStream(stream *smux.Stream) {
	defer stream.Close()

	var header proto.StreamHeader
	if err := proto.ReadJSONFrame(stream, &header); err != nil {
		c.printf("read stream header failed: %v", err)
		return
	}

	switch proto.NormalizeAppProto(header.Proto) {
	case "udp":
		c.handleIncomingUDP(stream, header.TargetPort)
	default:
		c.handleIncomingTCP(stream, header.TargetPort)
	}
}

func (c *Client) handleIncomingTCP(stream *smux.Stream, targetPort int) {
	upstream, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", targetPort))
	if err != nil {
		c.printf("dial local tcp target %d failed: %v", targetPort, err)
		return
	}
	pipeBoth(upstream, stream)
}

func (c *Client) handleIncomingUDP(stream *smux.Stream, targetPort int) {
	targetAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", targetPort))
	if err != nil {
		c.printf("resolve local udp target %d failed: %v", targetPort, err)
		return
	}
	upstream, err := net.DialUDP("udp", nil, targetAddr)
	if err != nil {
		c.printf("dial local udp target %d failed: %v", targetPort, err)
		return
	}
	defer upstream.Close()

	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := upstream.Read(buf)
			if err != nil {
				_ = stream.Close()
				return
			}
			if err := proto.WriteFrame(stream, append([]byte(nil), buf[:n]...)); err != nil {
				_ = stream.Close()
				return
			}
		}
	}()

	for {
		frame, err := proto.ReadFrame(stream)
		if err != nil {
			return
		}
		if _, err := upstream.Write(frame); err != nil {
			return
		}
	}
}

func (c *Client) sendSessionProbe(session *sessionState) error {
	msg := proto.Message{
		Type:      proto.TypeSessionProbe,
		ClientID:  c.id,
		RequestID: session.RequestID,
		Role:      session.Role,
	}
	payload, err := proto.Encode(msg)
	if err != nil {
		return err
	}
	_, err = session.Conn.WriteToUDP(payload, c.serverUDP)
	return err
}

func (c *Client) handleRelayStart(msg proto.Message) error {
	c.sessionsMu.Lock()
	session := c.sessions[msg.RequestID]
	c.sessionsMu.Unlock()
	if session == nil {
		return fmt.Errorf("relay session %s not found", msg.RequestID)
	}

	c.getOrCreateRelayConn(msg.RequestID, session.PeerID)
	if msg.Message != "" {
		c.printf("request %s: %s", msg.RequestID, msg.Message)
	}
	return nil
}

func (c *Client) handleRelayPacket(msg proto.Message) error {
	c.relayMu.Lock()
	conn := c.relayConns[msg.RequestID]
	c.relayMu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.enqueue(msg.Payload)
}

func (c *Client) getOrCreateRelayConn(requestID, peerID string) (*relayPacketConn, bool) {
	c.relayMu.Lock()
	defer c.relayMu.Unlock()

	if conn, ok := c.relayConns[requestID]; ok {
		return conn, false
	}

	conn := &relayPacketConn{
		client:    c,
		requestID: requestID,
		peerAddr:  relayAddr{value: "relay-peer-" + peerID},
		localAddr: relayAddr{value: "relay-local-" + c.id},
		readCh:    make(chan []byte, 1024),
		closeCh:   make(chan struct{}),
	}
	c.relayConns[requestID] = conn
	return conn, true
}

func (c *Client) setMembers(members []proto.MemberSummary) {
	c.membersMu.Lock()
	c.members = append([]proto.MemberSummary(nil), members...)
	c.membersMu.Unlock()
}

func (c *Client) printMembers(title string) {
	c.membersMu.RLock()
	members := append([]proto.MemberSummary(nil), c.members...)
	c.membersMu.RUnlock()

	c.printf("%s:", title)
	if len(members) == 0 {
		c.printf("  no members")
		return
	}
	for _, member := range members {
		c.printf("  #%d %s", member.Index, member.ID)
	}
}

func (c *Client) printTunnels() {
	c.tunnelsMu.Lock()
	defer c.tunnelsMu.Unlock()
	c.printf("active tunnels:")
	if len(c.tunnels) == 0 {
		c.printf("  none")
		return
	}
	for _, tunnel := range c.tunnels {
		c.printf("  %s | 127.0.0.1:%d -> %s:%d/%s",
			tunnel.RequestID, tunnel.LocalPort, tunnel.PeerID, tunnel.TargetPort, tunnel.Proto)
	}
}

func (c *Client) printHelp() {
	c.printf("commands:")
	c.printf("  help")
	c.printf("  info")
	c.printf("  list")
	c.printf("  tunnels")
	c.printf("  connect <peer-id|#index> <target-port> [local-port] [tcp|udp]")
	c.printf("  exit")
}

func (c *Client) writePrompt() {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	_, _ = fmt.Fprintf(c.out, "gonet[%s]> ", c.id)
}

func (c *Client) printf(format string, args ...any) {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	_, _ = fmt.Fprintf(c.out, format+"\n", args...)
}

func (u *udpControl) Send(msg proto.Message) error {
	payload, err := proto.Encode(msg)
	if err != nil {
		return err
	}
	u.writeMu.Lock()
	defer u.writeMu.Unlock()
	_, err = u.conn.Write(payload)
	return err
}

func (u *udpControl) Recv() (proto.Message, error) {
	buf := make([]byte, 64*1024)
	n, err := u.conn.Read(buf)
	if err != nil {
		return proto.Message{}, err
	}
	return proto.Decode(buf[:n])
}

func (u *udpControl) Close() error {
	return u.conn.Close()
}

func (t *tcpControl) Send(msg proto.Message) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return json.NewEncoder(t.conn).Encode(msg)
}

func (t *tcpControl) Recv() (proto.Message, error) {
	var msg proto.Message
	err := t.decoder.Decode(&msg)
	return msg, err
}

func (t *tcpControl) Close() error {
	return t.conn.Close()
}

func buildCandidateAddrs(endpoint string, predictedPorts []int) ([]*net.UDPAddr, error) {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %q: %w", endpoint, err)
	}
	basePort, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint port %q: %w", portText, err)
	}

	ports := append([]int{basePort}, predictedPorts...)
	seen := make(map[string]struct{})
	var addrs []*net.UDPAddr
	for _, port := range ports {
		if port <= 0 || port > 65535 {
			continue
		}
		key := fmt.Sprintf("%s:%d", host, port)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		addr, err := net.ResolveUDPAddr("udp", key)
		if err != nil {
			continue
		}
		addrs = append(addrs, addr)
	}
	if len(addrs) == 0 {
		return nil, errors.New("no usable candidate addresses")
	}
	return addrs, nil
}

func punchPhase(conn *net.UDPConn, requestID, clientID, role string, candidates []*net.UDPAddr, timeout time.Duration) (*net.UDPAddr, error) {
	payload, err := proto.Encode(proto.Message{
		Type:      proto.TypePunch,
		RequestID: requestID,
		ClientID:  clientID,
		Role:      role,
	})
	if err != nil {
		return nil, err
	}

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		ticker := time.NewTicker(180 * time.Millisecond)
		defer ticker.Stop()
		sendAll := func() {
			for _, addr := range candidates {
				_, _ = conn.WriteToUDP(payload, addr)
			}
		}
		sendAll()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sendAll()
			}
		}
	}()

	deadline := time.Now().Add(timeout)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return nil, err
		}
		msg, err := proto.Decode(buf[:n])
		if err != nil {
			continue
		}
		if msg.Type == proto.TypePunch && msg.RequestID == requestID {
			_ = conn.SetReadDeadline(time.Time{})
			return addr, nil
		}
	}

	_ = conn.SetReadDeadline(time.Time{})
	return nil, errors.New("timed out waiting for peer punch")
}

func drainUDP(conn *net.UDPConn, duration time.Duration) {
	buf := make([]byte, 4096)
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		if _, _, err := conn.ReadFromUDP(buf); err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				break
			}
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
}

func configureKCPSession(session *kcp.UDPSession) {
	session.SetNoDelay(1, 10, 2, 1)
	session.SetWindowSize(1024, 1024)
	session.SetACKNoDelay(true)
	_ = session.SetReadBuffer(4 * 1024 * 1024)
	_ = session.SetWriteBuffer(4 * 1024 * 1024)
}

func pipeBoth(left io.ReadWriteCloser, right io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(left, right)
		_ = left.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(right, left)
		_ = right.Close()
	}()

	wg.Wait()
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	copyAddr := *addr
	if addr.IP != nil {
		copyAddr.IP = append(net.IP(nil), addr.IP...)
	}
	return &copyAddr
}

func formatLocalPort(port int) string {
	if port == 0 {
		return "auto"
	}
	return strconv.Itoa(port)
}

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

func (a relayAddr) Network() string {
	return "relay"
}

func (a relayAddr) String() string {
	return a.value
}

func (r *relayPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		r.deadlineMu.Lock()
		deadline := r.readDeadline
		r.deadlineMu.Unlock()

		var timer <-chan time.Time
		if !deadline.IsZero() {
			wait := time.Until(deadline)
			if wait <= 0 {
				return 0, nil, deadlineExceededError{}
			}
			timer = time.After(wait)
		}

		select {
		case payload := <-r.readCh:
			n := copy(p, payload)
			return n, r.peerAddr, nil
		case <-r.closeCh:
			return 0, nil, net.ErrClosed
		case <-timer:
			return 0, nil, deadlineExceededError{}
		}
	}
}

func (r *relayPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	select {
	case <-r.closeCh:
		return 0, net.ErrClosed
	default:
	}

	msg := proto.Message{
		Type:      proto.TypeRelayPacket,
		ClientID:  r.client.id,
		RequestID: r.requestID,
		Payload:   append([]byte(nil), p...),
	}
	if err := r.client.control.Send(msg); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *relayPacketConn) Close() error {
	select {
	case <-r.closeCh:
		return nil
	default:
		close(r.closeCh)
	}

	r.client.relayMu.Lock()
	delete(r.client.relayConns, r.requestID)
	r.client.relayMu.Unlock()
	return nil
}

func (r *relayPacketConn) LocalAddr() net.Addr {
	return r.localAddr
}

func (r *relayPacketConn) SetDeadline(t time.Time) error {
	r.deadlineMu.Lock()
	r.readDeadline = t
	r.writeDeadline = t
	r.deadlineMu.Unlock()
	return nil
}

func (r *relayPacketConn) SetReadDeadline(t time.Time) error {
	r.deadlineMu.Lock()
	r.readDeadline = t
	r.deadlineMu.Unlock()
	return nil
}

func (r *relayPacketConn) SetWriteDeadline(t time.Time) error {
	r.deadlineMu.Lock()
	r.writeDeadline = t
	r.deadlineMu.Unlock()
	return nil
}

func (r *relayPacketConn) enqueue(payload []byte) error {
	packet := append([]byte(nil), payload...)
	select {
	case <-r.closeCh:
		return net.ErrClosed
	case r.readCh <- packet:
		return nil
	default:
		return errors.New("relay receive queue full")
	}
}

type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "i/o timeout" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return true }
