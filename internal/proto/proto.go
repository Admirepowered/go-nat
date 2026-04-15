package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	TypeRegister        = "register"
	TypeRegisterOK      = "register_ok"
	TypeHeartbeat       = "heartbeat"
	TypeList            = "list"
	TypeListResp        = "list_resp"
	TypeConnectReq      = "connect_req"
	TypePrepareDial     = "prepare_dial"
	TypePrepareAccept   = "prepare_accept"
	TypeSessionProbe    = "session_probe"
	TypeConnectPlan     = "connect_plan"
	TypePunch           = "punch"
	TypeRelayStart      = "relay_start"
	TypeRelayPacket     = "relay_packet"
	TypeNotice          = "notice"
	TypeError           = "error"
	DefaultAppProto     = "tcp"
	DefaultControlProto = "udp"
)

type MemberSummary struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Group    string `json:"group,omitempty"`
	JoinedAt string `json:"joined_at,omitempty"`
	LastSeen string `json:"last_seen,omitempty"`
}

type Message struct {
	Type           string          `json:"type"`
	ClientID       string          `json:"client_id,omitempty"`
	Group          string          `json:"group,omitempty"`
	ControlProto   string          `json:"control_proto,omitempty"`
	RequestID      string          `json:"request_id,omitempty"`
	ClientToken    string          `json:"client_token,omitempty"`
	Role           string          `json:"role,omitempty"`
	PeerID         string          `json:"peer_id,omitempty"`
	PeerIndex      int             `json:"peer_index,omitempty"`
	TargetID       string          `json:"target_id,omitempty"`
	TargetIndex    int             `json:"target_index,omitempty"`
	TargetPort     int             `json:"target_port,omitempty"`
	TargetProto    string          `json:"target_proto,omitempty"`
	LocalPort      int             `json:"local_port,omitempty"`
	Endpoint       string          `json:"endpoint,omitempty"`
	PredictedPorts []int           `json:"predicted_ports,omitempty"`
	Payload        []byte          `json:"payload,omitempty"`
	Members        []MemberSummary `json:"members,omitempty"`
	Message        string          `json:"message,omitempty"`
	Error          string          `json:"error,omitempty"`
}

type StreamHeader struct {
	Proto      string `json:"proto"`
	TargetPort int    `json:"target_port"`
}

func Encode(msg Message) ([]byte, error) {
	return json.Marshal(msg)
}

func Decode(data []byte) (Message, error) {
	var msg Message
	err := json.Unmarshal(data, &msg)
	return msg, err
}

func NormalizeAppProto(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", DefaultAppProto:
		return DefaultAppProto
	case "udp":
		return "udp"
	default:
		return DefaultAppProto
	}
}

func NormalizeControlProto(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", DefaultControlProto:
		return DefaultControlProto
	case "tcp":
		return "tcp"
	default:
		return DefaultControlProto
	}
}

func PredictPorts(basePort int, spread int) []int {
	if basePort <= 0 {
		return nil
	}
	ports := []int{basePort}
	for offset := 1; offset <= spread; offset++ {
		up := basePort + offset
		down := basePort - offset
		if up <= 65535 {
			ports = append(ports, up)
		}
		if down > 0 {
			ports = append(ports, down)
		}
	}
	slices.Sort(ports)
	return slices.Compact(ports)
}

func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > int(^uint32(0)) {
		return fmt.Errorf("frame too large: %d", len(payload))
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

func ReadFrame(r io.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header)
	payload := make([]byte, size)
	if size == 0 {
		return payload, nil
	}
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func WriteJSONFrame(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return WriteFrame(w, payload)
}

func ReadJSONFrame(r io.Reader, value any) error {
	payload, err := ReadFrame(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, value)
}
