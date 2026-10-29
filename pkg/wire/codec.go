package wire

import (
	"encoding/json"
	"fmt"
)

// Decode 根据 kind 解码为具体消息类型。
//
// 未知字段被忽略，便于三端滚动升级；未知版本或未知 kind 直接报错。
func Decode(data []byte) (any, error) {
	var head struct {
		V    int  `json:"v"`
		Kind Kind `json:"kind"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("wire: failed to parse message header: %w", err)
	}
	if head.V != Version {
		return nil, fmt.Errorf("wire: unsupported protocol version %d (want %d)", head.V, Version)
	}

	switch head.Kind {
	case KindEvent:
		var m Envelope
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return &m, nil
	case KindDecision:
		var m Decision
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return &m, nil
	case KindAck:
		var m Ack
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		if m.MsgID == "" {
			return nil, fmt.Errorf("wire: ack missing msg_id")
		}
		return &m, nil
	case KindHello:
		var m Hello
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return &m, nil
	case KindHelloAck:
		var m HelloAck
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case KindPing:
		var m Ping
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case KindPong:
		var m Pong
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case KindResync:
		var m Resync
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return &m, nil
	case KindResyncResult:
		var m ResyncResult
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	case KindLAToken:
		var m LAToken
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return &m, nil
	case KindLATokenAck:
		var m LATokenAck
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		return &m, nil
	default:
		return nil, fmt.Errorf("wire: unknown message kind %q", head.Kind)
	}
}

// Encode 序列化消息。
func Encode(v any) ([]byte, error) {
	return json.Marshal(v)
}
