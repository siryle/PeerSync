package discover

import "encoding/json"

// Wire messages are JSON text frames on the websocket. "signal" payloads are
// opaque to the server. File names and file bytes are not part of this protocol.

type helloMsg struct {
	Type     string `json:"type"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	ShareID  string `json:"share_id"`
	Token    string `json:"token"`
}

type signalMsg struct {
	Type string `json:"type"`
	To   string `json:"to,omitempty"`
	From string `json:"from,omitempty"`
	Data string `json:"data,omitempty"`
}

type peerInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type serverMsg struct {
	Type     string     `json:"type"`
	PublicIP string     `json:"public_ip,omitempty"`
	Peers    []peerInfo `json:"peers,omitempty"`
	ID       string     `json:"id,omitempty"`
	Name     string     `json:"name,omitempty"`
	From     string     `json:"from,omitempty"`
	Data     string     `json:"data,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// SignalBody is the plaintext inside a sealed signal.
type SignalBody struct {
	From string   `json:"from"`
	To   string   `json:"to"`
	TS   int64    `json:"ts"`
	N    uint64   `json:"n"`
	TCP  []string `json:"tcp,omitempty"`
	UDP  []string `json:"udp,omitempty"`
}

func marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func unmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
