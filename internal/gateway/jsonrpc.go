package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const methodToolsCall = "tools/call"

// rpcMessage is one parsed JSON-RPC message. Keys are matched exactly.
type rpcMessage struct {
	Method string
	ID     json.RawMessage // nil for notifications
	Name   string          // params.name
	// Subject is params._meta["attest/subject"], "" when absent or not a string.
	Subject string
}

var errDuplicateKey = errors.New("duplicate object key")

// checkKeys rejects objects whose keys collide case-insensitively. Parsers
// disagree on which duplicate wins (and some fold case), so a request the
// gateway reads as one method could be read upstream as another.
func checkKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := walk(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func walk(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for dec.More() {
		if d == '{' {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			folded := strings.ToLower(k.(string))
			if seen[folded] {
				return errDuplicateKey
			}
			seen[folded] = true
		}
		if err := walk(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token() // closing delimiter
	return err
}

// parseBody classifies a POST body. batch reports a JSON array; for one it
// returns whether any element is a tools/call.
func parseBody(raw []byte) (msg rpcMessage, batch, hasCall bool, err error) {
	if err = checkKeys(raw); err != nil {
		return
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var items []json.RawMessage
		if err = json.Unmarshal(trimmed, &items); err != nil {
			return
		}
		for _, it := range items {
			m, perr := parseMessage(it)
			if perr != nil {
				return msg, true, false, perr
			}
			hasCall = hasCall || m.Method == methodToolsCall
		}
		return msg, true, hasCall, nil
	}
	msg, err = parseMessage(trimmed)
	return msg, false, msg.Method == methodToolsCall, err
}

func parseMessage(raw []byte) (rpcMessage, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return rpcMessage{}, err
	}
	var m rpcMessage
	_ = json.Unmarshal(env["method"], &m.Method) // non-string method stays ""
	m.ID = env["id"]
	if m.Method != methodToolsCall {
		return m, nil
	}
	var params struct {
		Name string                     `json:"name"`
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal(env["params"], &params)
	m.Name = params.Name
	_ = json.Unmarshal(params.Meta["attest/subject"], &m.Subject)
	return m, nil
}
