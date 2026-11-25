package secretsource

// KeePassXC browser-integration protocol, client side.
//
// Read from the KeePassXC source (src/browser/): BrowserHost.cpp (stream
// socket, one flat JSON object per message, no length prefix),
// BrowserShared.cpp (socket path), BrowserAction.cpp (actions and their
// fields), BrowserMessageBuilder.cpp (NaCl crypto_box, standard base64, the
// reply nonce is the request nonce plus one, sodium_increment) and
// BrowserService.cpp (association, entry lookup by URL, keepassxc://by-uuid/
// and keepassxc://by-path/).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"golang.org/x/crypto/nacl/box"
)

const (
	// maxFrame bounds one reply: KeePassXC's own socket buffer is 1 MiB
	// (BrowserShared.h SOCKET_BUFFER_SIZE).
	maxFrame = 1 << 20
	// maxSkipped bounds the unsolicited messages (database-locked,
	// database-unlocked) skipped while waiting for a reply.
	maxSkipped = 8
)

// ErrNotAssociated means this machine has no (or no longer a valid)
// association with the open KeePassXC database: run `secretsource keepassxc
// associate` once and approve the request in KeePassXC.
var ErrNotAssociated = errors.New("secretsource: not associated with the KeePassXC database")

// ErrDenied means the person refused the request in KeePassXC.
var ErrDenied = errors.New("secretsource: KeePassXC refused the request")

// outer is a message as it crosses the socket. Every field KeePassXC sends is
// a string; a field of any other type is a protocol error.
type outer struct {
	Action    string `json:"action"`
	Message   string `json:"message"`
	Nonce     string `json:"nonce"`
	PublicKey string `json:"publicKey"`
	Version   string `json:"version"`
	Success   string `json:"success"`
	Error     string `json:"error"`
	ErrorCode string `json:"errorCode"`
}

// decodeFrame reads the first JSON object of buf, ignoring bytes before its
// "{" as KeePassXC's own reader does. It returns io.ErrUnexpectedEOF when the
// object is not complete yet, so the caller reads more.
func decodeFrame(buf []byte) (outer, int, error) {
	start := bytes.IndexByte(buf, '{')
	if start < 0 {
		if len(buf) >= maxFrame {
			return outer{}, 0, errors.New("keepassxc: no JSON object in the reply")
		}
		return outer{}, 0, io.ErrUnexpectedEOF
	}
	dec := json.NewDecoder(bytes.NewReader(buf[start:]))
	var o outer
	if err := dec.Decode(&o); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return outer{}, 0, io.ErrUnexpectedEOF
		}
		return outer{}, 0, fmt.Errorf("keepassxc: malformed reply: %w", err)
	}
	return o, start + int(dec.InputOffset()), nil
}

// incrementNonce adds one to a 24-byte little-endian nonce, as libsodium's
// sodium_increment does.
func incrementNonce(n [24]byte) [24]byte {
	for i := range n {
		n[i]++
		if n[i] != 0 {
			break
		}
	}
	return n
}

// kpxError is an error reply (errorCode and error) from KeePassXC.
type kpxError struct {
	Code int
	Text string
}

func (e *kpxError) Error() string {
	return fmt.Sprintf("keepassxc: error %d: %s", e.Code, e.Text)
}

// Is maps the codes of BrowserMessageBuilder.h onto the package's errors.
func (e *kpxError) Is(target error) bool {
	switch target {
	case ErrUnavailable:
		return e.Code == 1 || e.Code == 5 // database not opened, timeout / not connected
	case ErrDenied:
		return e.Code == 6 // action cancelled or denied
	case ErrNotAssociated:
		return e.Code == 8 // association failed
	case ErrNotFound:
		return e.Code == 15 // no logins found
	}
	return false
}

// conn is one KeePassXC session: a socket, its key pair, the server's key.
type conn struct {
	c        net.Conn
	buf      []byte
	pub, prv *[32]byte
	srv      *[32]byte
	clientID string
	closeFn  func()
}

// close ends the session and wipes the private key.
func (k *conn) close() {
	if k.prv != nil {
		clear(k.prv[:])
	}
	k.closeFn()
}

// randReader is the source of keys and nonces; tests replace it.
var randReader io.Reader = rand.Reader

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func randNonce() ([24]byte, error) {
	var n [24]byte
	_, err := io.ReadFull(randReader, n[:])
	return n, err
}

// newConn runs change-public-keys on c, which ctx bounds.
func newConn(ctx context.Context, c net.Conn) (*conn, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	k := &conn{c: c}
	k.closeFn = func() { stop(); _ = c.Close() }
	var err error
	if k.pub, k.prv, err = box.GenerateKey(randReader); err != nil {
		k.closeFn()
		return nil, err
	}
	var id [12]byte
	if _, err = io.ReadFull(randReader, id[:]); err != nil {
		k.closeFn()
		return nil, err
	}
	k.clientID = b64(id[:])
	nonce, err := randNonce()
	if err != nil {
		k.closeFn()
		return nil, err
	}
	reply, err := k.exchange("change-public-keys", map[string]string{
		"action":    "change-public-keys",
		"publicKey": b64(k.pub[:]),
		"nonce":     b64(nonce[:]),
		"clientID":  k.clientID,
	})
	if err != nil {
		k.closeFn()
		return nil, err
	}
	want := incrementNonce(nonce)
	srv, err := base64.StdEncoding.DecodeString(reply.PublicKey)
	if err != nil || len(srv) != 32 || reply.Nonce != b64(want[:]) {
		k.closeFn()
		return nil, errors.New("keepassxc: bad reply to change-public-keys")
	}
	k.srv = new([32]byte)
	copy(k.srv[:], srv)
	return k, nil
}
