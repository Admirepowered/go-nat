package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"gonet/internal/proto"
)

type Config struct {
	Listen           string
	ClientStaleAfter time.Duration
	RequestTTL       time.Duration
	CleanupInterval  time.Duration
	PredictSpread    int
}

type Server struct {
	cfg  Config
	udp  *net.UDPConn
	tcp  net.Listener
	logf func(string, ...any)

	mu       sync.Mutex
	clients  map[string]*clientState
	groups   map[string][]string
	requests map[string]*connectRequest
}

type clientState struct {
	ID       string
	Group    string
	Control  string
	UDPAddr  *net.UDPAddr
	TCPConn  net.Conn
	JoinedAt time.Time
	LastSeen time.Time
	writeMu  sync.Mutex
}

type connectRequest struct {
	ID                string
	ClientToken       string
	Group             string
	InitiatorID       string
	TargetID          string
	TargetPort        int
	TargetProto       string
	LocalPort         int
	CreatedAt         time.Time
	InitiatorEndpoint *net.UDPAddr
	TargetEndpoint    *net.UDPAddr
	Planned           bool
	Relay             bool
	RelayStartedBy    map[string]bool
}

func New(cfg Config) *Server {
	if cfg.Listen == "" {
		cfg.Listen = ":7000"
	}
	if cfg.ClientStaleAfter <= 0 {
		cfg.ClientStaleAfter = 90 * time.Second
	}
	if cfg.RequestTTL <= 0 {
		cfg.RequestTTL = 2 * time.Minute
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = 15 * time.Second
	}
	if cfg.PredictSpread <= 0 {
		cfg.PredictSpread = 4
	}
	return &Server{
		cfg:      cfg,
		logf:     log.Printf,
		clients:  make(map[string]*clientState),
		groups:   make(map[string][]string),
		requests: make(map[string]*connectRequest),
	}
}

func (s *Server) Run(ctx context.Context) error {
	udpAddr, err := net.ResolveUDPAddr("udp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("resolve udp addr: %w", err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}
	tcpLn, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		_ = udpConn.Close()
		return fmt.Errorf("listen tcp: %w", err)
	}
	s.udp = udpConn
	s.tcp = tcpLn

	errCh := make(chan error, 2)
	go s.serveUDP(errCh)
	go s.serveTCP(errCh)
	go s.cleanupLoop(ctx)

	s.logf("gonet server listening on %s (udp/tcp)", s.cfg.Listen)

	select {
	case <-ctx.Done():
		_ = s.udp.Close()
		_ = s.tcp.Close()
		s.closeTCPClients()
		return nil
	case err := <-errCh:
		_ = s.udp.Close()
		_ = s.tcp.Close()
		s.closeTCPClients()
		return err
	}
}

func (s *Server) serveUDP(errCh chan<- error) {
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			errCh <- fmt.Errorf("udp read: %w", err)
			return
		}
		msg, err := proto.Decode(buf[:n])
		if err != nil {
			continue
		}
		s.handleUDPMessage(msg, addr)
	}
}

func (s *Server) serveTCP(errCh chan<- error) {
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			errCh <- fmt.Errorf("tcp accept: %w", err)
			return
		}
		go s.handleTCPConn(conn)
	}
}

func (s *Server) handleTCPConn(conn net.Conn) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	var clientID string
	for {
		var msg proto.Message
		if err := decoder.Decode(&msg); err != nil {
			if clientID != "" {
				s.removeClient(clientID)
			}
			return
		}
		id, err := s.handleControlMessage(msg, clientID, nil, conn)
		if err != nil {
			_ = sendJSON(conn, proto.Message{Type: proto.TypeError, Error: err.Error()})
		}
		if id != "" {
			clientID = id
		}
	}
}

func (s *Server) handleUDPMessage(msg proto.Message, addr *net.UDPAddr) {
	_, err := s.handleControlMessage(msg, msg.ClientID, addr, nil)
	if err != nil {
		_ = s.sendUDP(addr, proto.Message{Type: proto.TypeError, Error: err.Error()})
	}
}

func (s *Server) handleControlMessage(msg proto.Message, knownClientID string, udpAddr *net.UDPAddr, tcpConn net.Conn) (string, error) {
	switch msg.Type {
	case proto.TypeRegister:
		return s.handleRegister(msg, udpAddr, tcpConn)
	case proto.TypeHeartbeat:
		return knownClientID, s.handleHeartbeat(knownClientID, msg.ClientID)
	case proto.TypeList:
		return knownClientID, s.handleList(knownClientID, msg.ClientID)
	case proto.TypeConnectReq:
		return knownClientID, s.handleConnectReq(knownClientID, msg.ClientID, msg)
	case proto.TypeSessionProbe:
		return knownClientID, s.handleSessionProbe(msg, udpAddr)
	case proto.TypeRelayStart:
		return knownClientID, s.handleRelayStart(knownClientID, msg.ClientID, msg)
	case proto.TypeRelayPacket:
		return knownClientID, s.handleRelayPacket(knownClientID, msg.ClientID, msg)
	default:
		return knownClientID, fmt.Errorf("unsupported message type %q", msg.Type)
	}
}

func (s *Server) handleRegister(msg proto.Message, udpAddr *net.UDPAddr, tcpConn net.Conn) (string, error) {
	group := strings.TrimSpace(msg.Group)
	if group == "" {
		return "", errors.New("group is required")
	}
	control := proto.NormalizeControlProto(msg.ControlProto)
	id := s.newID()
	now := time.Now()

	client := &clientState{
		ID:       id,
		Group:    group,
		Control:  control,
		UDPAddr:  udpAddr,
		TCPConn:  tcpConn,
		JoinedAt: now,
		LastSeen: now,
	}

	s.mu.Lock()
	s.clients[id] = client
	s.groups[group] = append(s.groups[group], id)
	members := s.membersLocked(group)
	s.mu.Unlock()

	s.logf("client %s joined group %s via %s", id, group, control)

	resp := proto.Message{
		Type:         proto.TypeRegisterOK,
		ClientID:     id,
		Group:        group,
		ControlProto: control,
		Members:      members,
		Message:      fmt.Sprintf("joined group %s", group),
	}
	if err := s.sendClient(client, resp); err != nil {
		s.removeClient(id)
		return "", err
	}
	return id, nil
}

func (s *Server) handleHeartbeat(knownClientID, msgClientID string) error {
	client := s.lookupClient(knownClientID, msgClientID)
	if client == nil {
		return errors.New("unknown client for heartbeat")
	}
	s.mu.Lock()
	client.LastSeen = time.Now()
	s.mu.Unlock()
	return nil
}

func (s *Server) handleList(knownClientID, msgClientID string) error {
	client := s.lookupClient(knownClientID, msgClientID)
	if client == nil {
		return errors.New("unknown client for list")
	}
	s.mu.Lock()
	client.LastSeen = time.Now()
	members := s.membersLocked(client.Group)
	s.mu.Unlock()
	return s.sendClient(client, proto.Message{
		Type:    proto.TypeListResp,
		Group:   client.Group,
		Members: members,
	})
}

func (s *Server) handleConnectReq(knownClientID, msgClientID string, msg proto.Message) error {
	initiator := s.lookupClient(knownClientID, msgClientID)
	if initiator == nil {
		return errors.New("unknown client for connect request")
	}
	if msg.TargetPort <= 0 || msg.TargetPort > 65535 {
		return fmt.Errorf("invalid target port %d", msg.TargetPort)
	}
	targetProto := proto.NormalizeAppProto(msg.TargetProto)

	s.mu.Lock()
	initiator.LastSeen = time.Now()
	target := s.resolveTargetLocked(initiator.Group, msg.TargetID, msg.TargetIndex)
	if target == nil {
		s.mu.Unlock()
		return fmt.Errorf("target not found in group %s", initiator.Group)
	}
	if target.ID == initiator.ID {
		s.mu.Unlock()
		return errors.New("cannot connect to yourself")
	}
	request := &connectRequest{
		ID:             s.newRequestIDLocked(),
		ClientToken:    msg.ClientToken,
		Group:          initiator.Group,
		InitiatorID:    initiator.ID,
		TargetID:       target.ID,
		TargetPort:     msg.TargetPort,
		TargetProto:    targetProto,
		LocalPort:      msg.LocalPort,
		CreatedAt:      time.Now(),
		RelayStartedBy: make(map[string]bool),
	}
	s.requests[request.ID] = request
	targetIndex := s.clientIndexLocked(initiator.Group, target.ID)
	initIndex := s.clientIndexLocked(initiator.Group, initiator.ID)
	s.mu.Unlock()

	if err := s.sendClient(target, proto.Message{
		Type:        proto.TypePrepareAccept,
		RequestID:   request.ID,
		Role:        "accept",
		PeerID:      initiator.ID,
		PeerIndex:   initIndex,
		TargetPort:  request.TargetPort,
		TargetProto: request.TargetProto,
		Message:     "incoming connect request",
	}); err != nil {
		s.removeRequest(request.ID)
		return err
	}

	if err := s.sendClient(initiator, proto.Message{
		Type:        proto.TypePrepareDial,
		RequestID:   request.ID,
		ClientToken: request.ClientToken,
		Role:        "dial",
		PeerID:      target.ID,
		PeerIndex:   targetIndex,
		TargetPort:  request.TargetPort,
		TargetProto: request.TargetProto,
		LocalPort:   request.LocalPort,
		Message:     "peer is preparing the tunnel",
	}); err != nil {
		s.removeRequest(request.ID)
		return err
	}

	return s.sendClient(initiator, proto.Message{
		Type:      proto.TypeNotice,
		RequestID: request.ID,
		Message:   fmt.Sprintf("connect request queued for %s:%d/%s", target.ID, request.TargetPort, request.TargetProto),
	})
}

func (s *Server) handleSessionProbe(msg proto.Message, addr *net.UDPAddr) error {
	if addr == nil {
		return errors.New("session probe must arrive over udp")
	}
	if msg.ClientID == "" || msg.RequestID == "" {
		return errors.New("session probe requires client_id and request_id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	request := s.requests[msg.RequestID]
	if request == nil {
		return fmt.Errorf("request %s not found", msg.RequestID)
	}

	switch msg.Role {
	case "dial":
		if msg.ClientID != request.InitiatorID {
			return errors.New("dial probe client mismatch")
		}
		request.InitiatorEndpoint = cloneUDPAddr(addr)
	case "accept":
		if msg.ClientID != request.TargetID {
			return errors.New("accept probe client mismatch")
		}
		request.TargetEndpoint = cloneUDPAddr(addr)
	default:
		return fmt.Errorf("invalid probe role %q", msg.Role)
	}

	if request.Planned || request.InitiatorEndpoint == nil || request.TargetEndpoint == nil {
		return nil
	}

	initiator := s.clients[request.InitiatorID]
	target := s.clients[request.TargetID]
	if initiator == nil || target == nil {
		return errors.New("request peer left before plan")
	}

	request.Planned = true
	initPlan := proto.Message{
		Type:           proto.TypeConnectPlan,
		RequestID:      request.ID,
		ClientToken:    request.ClientToken,
		Role:           "dial",
		PeerID:         target.ID,
		PeerIndex:      s.clientIndexLocked(request.Group, target.ID),
		TargetPort:     request.TargetPort,
		TargetProto:    request.TargetProto,
		LocalPort:      request.LocalPort,
		Endpoint:       request.TargetEndpoint.String(),
		PredictedPorts: proto.PredictPorts(request.TargetEndpoint.Port, s.cfg.PredictSpread),
		Message:        "peer endpoint ready",
	}
	targetPlan := proto.Message{
		Type:           proto.TypeConnectPlan,
		RequestID:      request.ID,
		Role:           "accept",
		PeerID:         initiator.ID,
		PeerIndex:      s.clientIndexLocked(request.Group, initiator.ID),
		TargetPort:     request.TargetPort,
		TargetProto:    request.TargetProto,
		Endpoint:       request.InitiatorEndpoint.String(),
		PredictedPorts: proto.PredictPorts(request.InitiatorEndpoint.Port, s.cfg.PredictSpread),
		Message:        "dial endpoint ready",
	}

	go func(initClient, targetClient *clientState, initMsg, targetMsg proto.Message) {
		if err := s.sendClient(initClient, initMsg); err != nil {
			s.logf("send dial plan failed: %v", err)
		}
		if err := s.sendClient(targetClient, targetMsg); err != nil {
			s.logf("send accept plan failed: %v", err)
		}
	}(initiator, target, initPlan, targetPlan)

	return nil
}

func (s *Server) handleRelayStart(knownClientID, msgClientID string, msg proto.Message) error {
	client := s.lookupClient(knownClientID, msgClientID)
	if client == nil {
		return errors.New("unknown client for relay start")
	}
	if msg.RequestID == "" {
		return errors.New("relay start requires request_id")
	}

	s.mu.Lock()
	request := s.requests[msg.RequestID]
	if request == nil {
		s.mu.Unlock()
		return fmt.Errorf("request %s not found", msg.RequestID)
	}
	if client.ID != request.InitiatorID && client.ID != request.TargetID {
		s.mu.Unlock()
		return errors.New("client is not part of request")
	}
	request.Relay = true
	request.RelayStartedBy[client.ID] = true

	initiator := s.clients[request.InitiatorID]
	target := s.clients[request.TargetID]
	if initiator == nil || target == nil {
		s.mu.Unlock()
		return errors.New("request peer left before relay")
	}

	if !request.RelayStartedBy[request.InitiatorID] || !request.RelayStartedBy[request.TargetID] {
		peerID := request.InitiatorID
		if client.ID == request.InitiatorID {
			peerID = request.TargetID
		}
		peer := s.clients[peerID]
		s.mu.Unlock()
		if peer == nil {
			return errors.New("relay peer not found")
		}
		return s.sendClient(peer, proto.Message{
			Type:      proto.TypeRelayStart,
			RequestID: msg.RequestID,
			Message:   "peer requested relay fallback",
		})
	}
	s.mu.Unlock()

	if err := s.sendClient(initiator, proto.Message{
		Type:      proto.TypeRelayStart,
		RequestID: msg.RequestID,
		Role:      "dial",
		PeerID:    target.ID,
		Message:   "relay fallback active",
	}); err != nil {
		return err
	}
	return s.sendClient(target, proto.Message{
		Type:      proto.TypeRelayStart,
		RequestID: msg.RequestID,
		Role:      "accept",
		PeerID:    initiator.ID,
		Message:   "relay fallback active",
	})
}

func (s *Server) handleRelayPacket(knownClientID, msgClientID string, msg proto.Message) error {
	client := s.lookupClient(knownClientID, msgClientID)
	if client == nil {
		return errors.New("unknown client for relay packet")
	}
	if msg.RequestID == "" {
		return errors.New("relay packet requires request_id")
	}

	s.mu.Lock()
	request := s.requests[msg.RequestID]
	if request == nil {
		s.mu.Unlock()
		return fmt.Errorf("request %s not found", msg.RequestID)
	}
	if !request.Relay {
		s.mu.Unlock()
		return errors.New("relay not active for request")
	}

	var peer *clientState
	switch client.ID {
	case request.InitiatorID:
		peer = s.clients[request.TargetID]
	case request.TargetID:
		peer = s.clients[request.InitiatorID]
	default:
		s.mu.Unlock()
		return errors.New("client is not part of request")
	}
	s.mu.Unlock()

	if peer == nil {
		return errors.New("relay peer offline")
	}

	return s.sendClient(peer, proto.Message{
		Type:      proto.TypeRelayPacket,
		RequestID: msg.RequestID,
		ClientID:  client.ID,
		Payload:   append([]byte(nil), msg.Payload...),
	})
}

func (s *Server) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.cleanupClients()
			s.cleanupRequests()
		}
	}
}

func (s *Server) cleanupClients() {
	cutoff := time.Now().Add(-s.cfg.ClientStaleAfter)
	var stale []string
	s.mu.Lock()
	for id, client := range s.clients {
		if client.LastSeen.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	s.mu.Unlock()
	for _, id := range stale {
		s.logf("removing stale client %s", id)
		s.removeClient(id)
	}
}

func (s *Server) cleanupRequests() {
	cutoff := time.Now().Add(-s.cfg.RequestTTL)
	var expired []string
	s.mu.Lock()
	for id, request := range s.requests {
		if request.CreatedAt.Before(cutoff) {
			expired = append(expired, id)
		}
	}
	s.mu.Unlock()
	for _, id := range expired {
		s.removeRequest(id)
	}
}

func (s *Server) membersLocked(group string) []proto.MemberSummary {
	ids := s.groups[group]
	members := make([]proto.MemberSummary, 0, len(ids))
	index := 1
	for _, id := range ids {
		client := s.clients[id]
		if client == nil {
			continue
		}
		members = append(members, proto.MemberSummary{
			ID:       client.ID,
			Index:    index,
			Group:    client.Group,
			JoinedAt: client.JoinedAt.Format(time.RFC3339),
			LastSeen: client.LastSeen.Format(time.RFC3339),
		})
		index++
	}
	return members
}

func (s *Server) resolveTargetLocked(group, targetID string, targetIndex int) *clientState {
	if targetID != "" {
		client := s.clients[targetID]
		if client != nil && client.Group == group {
			return client
		}
		return nil
	}
	if targetIndex <= 0 {
		return nil
	}
	index := 1
	for _, id := range s.groups[group] {
		client := s.clients[id]
		if client == nil {
			continue
		}
		if index == targetIndex {
			return client
		}
		index++
	}
	return nil
}

func (s *Server) clientIndexLocked(group, clientID string) int {
	index := 1
	for _, id := range s.groups[group] {
		client := s.clients[id]
		if client == nil {
			continue
		}
		if id == clientID {
			return index
		}
		index++
	}
	return 0
}

func (s *Server) lookupClient(knownClientID, msgClientID string) *clientState {
	clientID := knownClientID
	if clientID == "" {
		clientID = msgClientID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients[clientID]
}

func (s *Server) sendClient(client *clientState, msg proto.Message) error {
	if client == nil {
		return errors.New("client is nil")
	}
	switch client.Control {
	case "tcp":
		client.writeMu.Lock()
		defer client.writeMu.Unlock()
		return sendJSON(client.TCPConn, msg)
	default:
		return s.sendUDP(client.UDPAddr, msg)
	}
}

func (s *Server) sendUDP(addr *net.UDPAddr, msg proto.Message) error {
	if addr == nil {
		return errors.New("udp address missing")
	}
	payload, err := proto.Encode(msg)
	if err != nil {
		return err
	}
	_, err = s.udp.WriteToUDP(payload, addr)
	return err
}

func (s *Server) removeClient(id string) {
	s.mu.Lock()
	client := s.clients[id]
	delete(s.clients, id)

	if client != nil {
		ids := s.groups[client.Group]
		next := ids[:0]
		for _, item := range ids {
			if item != id {
				next = append(next, item)
			}
		}
		if len(next) == 0 {
			delete(s.groups, client.Group)
		} else {
			s.groups[client.Group] = next
		}
	}

	for requestID, request := range s.requests {
		if request.InitiatorID == id || request.TargetID == id {
			delete(s.requests, requestID)
		}
	}
	s.mu.Unlock()

	if client != nil && client.TCPConn != nil {
		_ = client.TCPConn.Close()
	}
}

func (s *Server) removeRequest(id string) {
	s.mu.Lock()
	delete(s.requests, id)
	s.mu.Unlock()
}

func (s *Server) closeTCPClients() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, client := range s.clients {
		if client.TCPConn != nil {
			_ = client.TCPConn.Close()
		}
	}
}

func (s *Server) newID() string {
	for {
		id := "c-" + randomHex(4)
		s.mu.Lock()
		_, exists := s.clients[id]
		s.mu.Unlock()
		if !exists {
			return id
		}
	}
}

func (s *Server) newRequestIDLocked() string {
	for {
		id := "r-" + randomHex(5)
		if _, exists := s.requests[id]; !exists {
			return id
		}
	}
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

func sendJSON(conn net.Conn, msg proto.Message) error {
	encoder := json.NewEncoder(conn)
	return encoder.Encode(msg)
}

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
