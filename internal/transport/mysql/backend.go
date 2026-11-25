package mysql

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ivanov39/db-proxy-ro/internal/domain"
)

// readOnlyStatement makes every following transaction of the session,
// including each autocommit statement, read-only.
const readOnlyStatement = "SET SESSION TRANSACTION READ ONLY"

// clientHello is what the proxy learned from the client's handshake.
type clientHello struct {
	caps      uint32
	maxPacket uint32
	charset   byte
	user      string
	database  string
	plugin    string
	auth      []byte
	attrs     []byte // raw connection attributes, forwarded as they are
}

// dialBackend opens an authenticated, read-only session to the real server.
// It returns the connection and the server status flags.
func dialBackend(ctx context.Context, t domain.Target, hello clientHello) (*conn, uint16, error) {
	c, status, err := dialBackendTLS(ctx, t, hello, t.TLS != domain.TLSDisable)
	var tlsErr *tlsHandshakeError
	if errors.As(err, &tlsErr) && t.TLS == domain.TLSPrefer {
		c, status, err = dialBackendTLS(ctx, t, hello, false)
	}
	return c, status, err
}

type tlsHandshakeError struct{ err error }

func (e *tlsHandshakeError) Error() string { return "TLS handshake with the backend: " + e.err.Error() }
func (e *tlsHandshakeError) Unwrap() error { return e.err }

func dialBackendTLS(ctx context.Context, t domain.Target, hello clientHello, wantTLS bool) (*conn, uint16, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", t.Addr())
	if err != nil {
		return nil, 0, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		raw.SetDeadline(deadline)
	}
	b := &backendHandshake{target: t, hello: hello, conn: newConn(raw)}
	status, err := b.run(ctx, wantTLS)
	if err != nil {
		b.conn.Close()
		return nil, 0, err
	}
	b.conn.SetDeadline(time.Time{})
	return b.conn, status, nil
}

type backendHandshake struct {
	target domain.Target
	hello  clientHello
	conn   *conn
	secure bool // the connection is encrypted
}

func (b *backendHandshake) run(ctx context.Context, wantTLS bool) (uint16, error) {
	p, err := b.conn.readPacket(maxCommandLen)
	if err != nil {
		return 0, fmt.Errorf("read server greeting: %w", err)
	}
	if isERR(p) {
		return 0, parseERR(p)
	}
	serverCaps, salt, plugin, err := parseGreeting(p)
	if err != nil {
		return 0, err
	}
	const required = capProtocol41 | capSecureConnection | capPluginAuth
	if serverCaps&required != required {
		return 0, errors.New("backend server is too old: MySQL 5.5 or newer is required")
	}

	caps := required | capLongPassword | capTransactions | capConnectWithDB |
		(serverCaps & (capPluginAuthLenenc | b.hello.caps&passedCaps))
	if len(b.hello.attrs) > 0 {
		caps |= serverCaps & capConnectAttrs
	}
	maxPacket := b.hello.maxPacket
	if maxPacket == 0 {
		maxPacket = 1 << 24
	}
	head := make([]byte, 0, 32)
	appendHead := func(caps uint32) []byte {
		h := appendUint32(head[:0], caps)
		h = appendUint32(h, maxPacket)
		h = append(h, b.hello.charset)
		return append(h, make([]byte, 23)...)
	}

	switch {
	case wantTLS && serverCaps&capSSL != 0:
		caps |= capSSL
		if err := b.conn.send(appendHead(caps)); err != nil {
			return 0, err
		}
		tlsConn := tls.Client(b.conn.Conn, &tls.Config{
			ServerName:         b.target.Host,
			InsecureSkipVerify: b.target.TLS != domain.TLSVerifyFull,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return 0, &tlsHandshakeError{err}
		}
		seq := b.conn.seq
		b.conn = newConn(tlsConn)
		b.conn.seq = seq
		b.secure = true
	case b.target.TLS == domain.TLSRequire || b.target.TLS == domain.TLSVerifyFull:
		return 0, fmt.Errorf("sslmode=%s but the backend does not support TLS", b.target.TLS)
	}

	auth, err := b.authData(plugin, salt)
	if err != nil {
		// Unknown default plugin: start with the common one, the server
		// will ask to switch if it wants something else.
		plugin = nativePassword
		auth = nativeScramble(b.target.Password, salt)
	}
	resp := appendHead(caps)
	resp = append(append(resp, b.target.User...), 0)
	if caps&capPluginAuthLenenc != 0 {
		resp = appendLenenc(resp, uint64(len(auth)))
	} else {
		resp = append(resp, byte(len(auth)))
	}
	resp = append(resp, auth...)
	resp = append(append(resp, b.target.Database...), 0)
	resp = append(append(resp, plugin...), 0)
	if caps&capConnectAttrs != 0 {
		resp = appendLenenc(resp, uint64(len(b.hello.attrs)))
		resp = append(resp, b.hello.attrs...)
	}
	if err := b.conn.send(resp); err != nil {
		return 0, err
	}

	if err := b.authenticate(plugin, salt); err != nil {
		return 0, err
	}
	return setReadOnly(b.conn)
}

// authenticate drives the authentication exchange to its final OK.
func (b *backendHandshake) authenticate(plugin string, salt []byte) error {
	for {
		p, err := b.conn.readPacket(maxCommandLen)
		if err != nil {
			return fmt.Errorf("read authentication reply: %w", err)
		}
		if len(p) == 0 {
			return errors.New("empty authentication reply")
		}
		var reply []byte
		switch p[0] {
		case pktOK:
			return nil
		case pktERR:
			return parseERR(p)
		case pktEOF: // AuthSwitchRequest
			if len(p) == 1 {
				return errors.New("backend asks for the pre-4.1 password format, which is not supported")
			}
			r := reader{b: p[1:]}
			plugin = r.cstring()
			salt = append([]byte(nil), bytes.TrimRight(r.b, "\x00")...)
			if reply, err = b.authData(plugin, salt); err != nil {
				return err
			}
		case pktAuthMore:
			if reply, err = b.moreData(plugin, salt, p[1:]); err != nil {
				return err
			}
			if reply == nil {
				continue
			}
		default:
			return fmt.Errorf("unexpected authentication packet 0x%02x", p[0])
		}
		if err := b.conn.send(reply); err != nil {
			return err
		}
	}
}

// authData builds the first message of an authentication plugin.
func (b *backendHandshake) authData(plugin string, salt []byte) ([]byte, error) {
	password := b.target.Password
	switch plugin {
	case nativePassword:
		return nativeScramble(password, salt), nil
	case cachingSHA2:
		return sha2Scramble(password, salt), nil
	case sha256Password:
		switch {
		case password == "":
			return []byte{0}, nil
		case b.secure:
			return append([]byte(password), 0), nil
		default:
			return []byte{1}, nil // ask for the server's public key
		}
	case clearPassword:
		if !b.secure {
			return nil, errors.New("backend asks for a clear-text password over an unencrypted connection")
		}
		return append([]byte(password), 0), nil
	}
	return nil, fmt.Errorf("authentication plugin %q is not supported", plugin)
}

// moreData answers an AuthMoreData packet. A nil reply means "keep reading".
func (b *backendHandshake) moreData(plugin string, salt, data []byte) ([]byte, error) {
	switch plugin {
	case cachingSHA2:
		switch {
		case len(data) == 1 && data[0] == 3: // fast authentication succeeded
			return nil, nil
		case len(data) == 1 && data[0] == 4: // full authentication needed
			if b.secure {
				return append([]byte(b.target.Password), 0), nil
			}
			return []byte{2}, nil // ask for the server's public key
		default: // the public key
			return encryptPassword(b.target.Password, salt, data)
		}
	case sha256Password:
		return encryptPassword(b.target.Password, salt, data)
	}
	return nil, fmt.Errorf("unexpected authentication data for plugin %q", plugin)
}

// setReadOnly switches the session to read-only transactions and returns the
// server status flags. It must succeed, or the session is not used.
func setReadOnly(c *conn) (uint16, error) {
	c.seq = 0
	if err := c.send(append([]byte{comQuery}, readOnlyStatement...)); err != nil {
		return 0, err
	}
	p, err := c.readPacket(maxCommandLen)
	if err != nil {
		return 0, fmt.Errorf("enter read-only mode: %w", err)
	}
	if len(p) == 0 || p[0] != pktOK {
		if isERR(p) {
			return 0, fmt.Errorf("backend refused read-only mode: %w", parseERR(p))
		}
		return 0, errors.New("backend refused read-only mode")
	}
	return okStatus(p), nil
}

func parseGreeting(p []byte) (caps uint32, salt []byte, plugin string, err error) {
	r := reader{b: p}
	if version := r.byte(); version != 10 {
		return 0, nil, "", fmt.Errorf("unsupported server protocol version %d", version)
	}
	r.cstring() // server version
	r.uint32()  // connection id
	salt = append(salt, r.bytes(8)...)
	r.byte() // filler
	caps = uint32(r.uint16())
	if err = r.err(); err != nil {
		return 0, nil, "", err
	}
	if len(r.b) == 0 {
		return caps, salt, "", nil
	}
	r.byte()   // character set
	r.uint16() // status flags
	caps |= uint32(r.uint16()) << 16
	authLen := int(r.byte())
	r.bytes(10) // reserved
	if caps&capSecureConnection != 0 {
		n := max(13, authLen-8)
		salt = append(salt, bytes.TrimRight(r.bytes(n), "\x00")...)
	}
	if caps&capPluginAuth != 0 && len(r.b) > 0 {
		// Some servers omit the terminating NUL.
		plugin = string(bytes.TrimRight(r.b, "\x00"))
	}
	return caps, salt, plugin, r.err()
}

func appendUint32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
