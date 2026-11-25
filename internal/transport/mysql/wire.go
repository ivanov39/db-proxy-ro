package mysql

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// Capability flags.
const (
	capLongPassword     uint32 = 1 << 0
	capFoundRows        uint32 = 1 << 1
	capLongFlag         uint32 = 1 << 2
	capConnectWithDB    uint32 = 1 << 3
	capNoSchema         uint32 = 1 << 4
	capODBC             uint32 = 1 << 6
	capIgnoreSpace      uint32 = 1 << 8
	capProtocol41       uint32 = 1 << 9
	capInteractive      uint32 = 1 << 10
	capSSL              uint32 = 1 << 11
	capIgnoreSigpipe    uint32 = 1 << 12
	capTransactions     uint32 = 1 << 13
	capSecureConnection uint32 = 1 << 15
	capMultiStatements  uint32 = 1 << 16
	capMultiResults     uint32 = 1 << 17
	capPSMultiResults   uint32 = 1 << 18
	capPluginAuth       uint32 = 1 << 19
	capConnectAttrs     uint32 = 1 << 20
	capPluginAuthLenenc uint32 = 1 << 21
)

// offeredCaps is what the proxy offers to clients. It deliberately leaves out
// every optional feature that changes the packet format (CLIENT_DEPRECATE_EOF,
// CLIENT_QUERY_ATTRIBUTES, CLIENT_SESSION_TRACK, compression, ...): the same
// format must be valid on both sides of the proxy, whatever the backend
// version, because packets are relayed unchanged.
const offeredCaps uint32 = capLongPassword | capFoundRows | capLongFlag | capConnectWithDB | capNoSchema |
	capODBC | capIgnoreSpace | capProtocol41 | capInteractive | capIgnoreSigpipe | capTransactions |
	capSecureConnection | capMultiStatements | capMultiResults | capPSMultiResults | capPluginAuth |
	capConnectAttrs | capPluginAuthLenenc

// passedCaps are client choices repeated towards the backend.
const passedCaps = capFoundRows | capLongFlag | capNoSchema | capODBC | capIgnoreSpace | capInteractive |
	capMultiStatements | capMultiResults | capPSMultiResults

// Commands.
const (
	comQuit            = 0x01
	comInitDB          = 0x02
	comQuery           = 0x03
	comFieldList       = 0x04
	comStatistics      = 0x09
	comPing            = 0x0e
	comStmtPrepare     = 0x16
	comStmtExecute     = 0x17
	comStmtSendLong    = 0x18
	comStmtClose       = 0x19
	comStmtReset       = 0x1a
	comSetOption       = 0x1b
	comStmtFetch       = 0x1c
	comResetConnection = 0x1f
)

var commandNames = map[byte]string{
	0x00: "COM_SLEEP", 0x05: "COM_CREATE_DB", 0x06: "COM_DROP_DB", 0x07: "COM_REFRESH",
	0x08: "COM_SHUTDOWN", 0x0a: "COM_PROCESS_INFO", 0x0c: "COM_PROCESS_KILL", 0x0d: "COM_DEBUG",
	0x11: "COM_CHANGE_USER", 0x12: "COM_BINLOG_DUMP", 0x13: "COM_TABLE_DUMP",
	0x15: "COM_REGISTER_SLAVE", 0x1e: "COM_BINLOG_DUMP_GTID", 0xfa: "COM_STMT_BULK_EXECUTE",
}

const (
	pktOK          = 0x00
	pktAuthMore    = 0x01
	pktLocalInfile = 0xfb
	pktEOF         = 0xfe
	pktERR         = 0xff

	statusAutocommit   uint16 = 0x0002
	statusMoreResults  uint16 = 0x0008
	statusCursorExists uint16 = 0x0040

	maxFrame        = 0xffffff
	maxCommandLen   = 64 << 20
	keepBufferBelow = 1 << 20
	headLen         = 1024

	nativePassword = "mysql_native_password"
	cachingSHA2    = "caching_sha2_password"
	sha256Password = "sha256_password"
	clearPassword  = "mysql_clear_password"
)

// conn frames MySQL packets over a network connection.
type conn struct {
	net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
	seq uint8 // sequence number of the next frame in either direction
	buf []byte
}

func newConn(c net.Conn) *conn {
	return &conn{Conn: c, r: bufio.NewReaderSize(c, 32<<10), w: bufio.NewWriterSize(c, 32<<10)}
}

// readFrame reads one physical frame. The payload is valid until the next read.
func (c *conn) readFrame() ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return nil, err
	}
	n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
	if hdr[3] != c.seq {
		return nil, fmt.Errorf("packets out of order: got sequence %d, want %d", hdr[3], c.seq)
	}
	c.seq++
	if cap(c.buf) < n || (cap(c.buf) > keepBufferBelow && n <= keepBufferBelow) {
		c.buf = make([]byte, n)
	}
	payload := c.buf[:n]
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// readPacket reads one logical packet, joining frames of a packet that was
// split because it exceeds 16 MB. The result is valid until the next read.
func (c *conn) readPacket(limit int) ([]byte, error) {
	payload, err := c.readFrame()
	if err != nil || len(payload) < maxFrame {
		return payload, err
	}
	whole := append([]byte(nil), payload...)
	for len(payload) == maxFrame {
		if payload, err = c.readFrame(); err != nil {
			return nil, err
		}
		if len(whole)+len(payload) > limit {
			return nil, fmt.Errorf("packet exceeds the %d byte limit", limit)
		}
		whole = append(whole, payload...)
	}
	return whole, nil
}

func (c *conn) writeFrame(payload []byte) error {
	hdr := [4]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), c.seq}
	c.seq++
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.w.Write(payload)
	return err
}

// writePacket writes one logical packet, splitting it into frames as needed.
// Nothing reaches the network until flush.
func (c *conn) writePacket(payload []byte) error {
	for len(payload) >= maxFrame {
		if err := c.writeFrame(payload[:maxFrame]); err != nil {
			return err
		}
		payload = payload[maxFrame:]
	}
	return c.writeFrame(payload)
}

func (c *conn) flush() error { return c.w.Flush() }

// send writes a packet and flushes it.
func (c *conn) send(payload []byte) error {
	if err := c.writePacket(payload); err != nil {
		return err
	}
	return c.flush()
}

// serverError is an ERR packet received from, or sent on behalf of, a server.
type serverError struct {
	Code    uint16
	State   string
	Message string
}

func (e *serverError) Error() string {
	return fmt.Sprintf("%d (%s): %s", e.Code, e.State, e.Message)
}

func (e *serverError) packet() []byte {
	state := e.State
	if len(state) != 5 {
		state = "HY000"
	}
	b := []byte{pktERR, byte(e.Code), byte(e.Code >> 8), '#'}
	b = append(b, state...)
	return append(b, e.Message...)
}

func parseERR(p []byte) *serverError {
	e := &serverError{State: "HY000"}
	if len(p) < 3 {
		e.Message = "malformed error packet"
		return e
	}
	e.Code = binary.LittleEndian.Uint16(p[1:])
	p = p[3:]
	if len(p) >= 6 && p[0] == '#' {
		e.State, p = string(p[1:6]), p[6:]
	}
	e.Message = string(p)
	return e
}

func okPacket(status uint16) []byte {
	return []byte{pktOK, 0, 0, byte(status), byte(status >> 8), 0, 0}
}

func isERR(p []byte) bool { return len(p) > 0 && p[0] == pktERR }
func isEOF(p []byte) bool { return len(p) > 0 && len(p) < 9 && p[0] == pktEOF }

// okStatus returns the status flags of an OK packet.
func okStatus(p []byte) uint16 {
	r := reader{b: p[1:]}
	r.lenenc() // affected rows
	r.lenenc() // last insert id
	return r.uint16()
}

// eofStatus returns the status flags of an EOF packet.
func eofStatus(p []byte) uint16 {
	if len(p) < 5 {
		return 0
	}
	return binary.LittleEndian.Uint16(p[3:])
}

// reader decodes packet fields. After a short read it keeps returning zero
// values and reports the problem once, through err.
type reader struct {
	b   []byte
	bad bool
}

func (r *reader) err() error {
	if r.bad {
		return errors.New("malformed packet")
	}
	return nil
}

func (r *reader) bytes(n int) []byte {
	if n < 0 || n > len(r.b) {
		r.bad, r.b = true, nil
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) byte() byte {
	if v := r.bytes(1); v != nil {
		return v[0]
	}
	return 0
}

func (r *reader) uint16() uint16 {
	if v := r.bytes(2); v != nil {
		return binary.LittleEndian.Uint16(v)
	}
	return 0
}

func (r *reader) uint32() uint32 {
	if v := r.bytes(4); v != nil {
		return binary.LittleEndian.Uint32(v)
	}
	return 0
}

func (r *reader) lenenc() uint64 {
	switch first := r.byte(); first {
	case 0xfc:
		return uint64(r.uint16())
	case 0xfd:
		v := r.bytes(3)
		if v == nil {
			return 0
		}
		return uint64(v[0]) | uint64(v[1])<<8 | uint64(v[2])<<16
	case 0xfe:
		if v := r.bytes(8); v != nil {
			return binary.LittleEndian.Uint64(v)
		}
		return 0
	default:
		return uint64(first)
	}
}

// cstring reads a NUL-terminated string.
func (r *reader) cstring() string {
	for i, c := range r.b {
		if c == 0 {
			v := string(r.b[:i])
			r.b = r.b[i+1:]
			return v
		}
	}
	r.bad, r.b = true, nil
	return ""
}

func appendLenenc(b []byte, n uint64) []byte {
	switch {
	case n < 0xfb:
		return append(b, byte(n))
	case n <= 0xffff:
		return append(b, 0xfc, byte(n), byte(n>>8))
	case n <= 0xffffff:
		return append(b, 0xfd, byte(n), byte(n>>8), byte(n>>16))
	default:
		return binary.LittleEndian.AppendUint64(append(b, 0xfe), n)
	}
}
