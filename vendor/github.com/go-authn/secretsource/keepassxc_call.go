package secretsource

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"golang.org/x/crypto/nacl/box"
)

// exchange sends one plain JSON object and returns the reply whose action is
// action, skipping the unsolicited notices KeePassXC broadcasts (database
// locked or unlocked). An error reply becomes a *kpxError.
func (k *conn) exchange(action string, req map[string]string) (outer, error) {
	b, _ := json.Marshal(req) // a map of strings always marshals
	if _, err := k.c.Write(b); err != nil {
		return outer{}, fmt.Errorf("%w: keepassxc: %v", ErrUnavailable, err)
	}
	for skipped := 0; skipped <= maxSkipped; skipped++ {
		o, err := k.readFrame()
		if err != nil {
			return outer{}, err
		}
		if o.Action != action {
			continue
		}
		if o.ErrorCode != "" || o.Error != "" {
			code, _ := strconv.Atoi(o.ErrorCode)
			return outer{}, &kpxError{Code: code, Text: clip(o.Error)}
		}
		return o, nil
	}
	return outer{}, errors.New("keepassxc: no reply to " + action)
}

// readFrame reads from the socket until one object is complete.
func (k *conn) readFrame() (outer, error) {
	for {
		o, n, err := decodeFrame(k.buf)
		if err == nil {
			k.buf = k.buf[n:]
			return o, nil
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			return outer{}, err
		}
		if len(k.buf) >= maxFrame {
			return outer{}, errors.New("keepassxc: reply longer than 1 MiB")
		}
		chunk := make([]byte, 4096)
		m, rerr := k.c.Read(chunk)
		k.buf = append(k.buf, chunk[:m]...)
		if m == 0 && rerr != nil {
			return outer{}, fmt.Errorf("%w: keepassxc: %v", ErrUnavailable, rerr)
		}
	}
}

// errNotSucceeded is a reply whose success field is not "true".
var errNotSucceeded = errors.New("keepassxc: the request did not succeed")

// call sends an encrypted request and decrypts the reply into out. The reply
// carries the request nonce plus one, which is checked: a reply that does not
// is not an answer to this request. triggerUnlock makes KeePassXC ask the
// person to unlock the database when it is locked.
func (k *conn) call(action string, inner map[string]any, out any) error {
	nonce, err := randNonce()
	if err != nil {
		return err
	}
	inner["action"] = action
	plain, _ := json.Marshal(inner) // strings and maps of strings always marshal
	sealed := box.Seal(nil, plain, &nonce, k.srv, k.prv)
	clear(plain)
	o, err := k.exchange(action, map[string]string{
		"action":        action,
		"message":       b64(sealed),
		"nonce":         b64(nonce[:]),
		"clientID":      k.clientID,
		"triggerUnlock": "true",
	})
	if err != nil {
		return err
	}
	want := incrementNonce(nonce)
	if subtle.ConstantTimeCompare([]byte(o.Nonce), []byte(b64(want[:]))) != 1 {
		return errors.New("keepassxc: the reply does not carry the nonce of the request")
	}
	raw, err := base64.StdEncoding.DecodeString(o.Message)
	if err != nil {
		return errors.New("keepassxc: the reply is not base64")
	}
	opened, ok := box.Open(nil, raw, &want, k.srv, k.prv)
	if !ok {
		return errors.New("keepassxc: the reply does not decrypt")
	}
	defer clear(opened)
	var status struct {
		Success string `json:"success"`
	}
	if err := json.Unmarshal(opened, &status); err != nil {
		return fmt.Errorf("keepassxc: malformed reply: %w", err)
	}
	if status.Success != "true" {
		return errNotSucceeded
	}
	return json.Unmarshal(opened, out)
}
