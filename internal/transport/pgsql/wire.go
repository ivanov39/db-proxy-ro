package pgsql

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// Message type bytes used by the proxy. Frontend and backend types overlap.
const (
	msgQuery        = 'Q'
	msgParse        = 'P'
	msgBind         = 'B'
	msgExecute      = 'E'
	msgDescribe     = 'D'
	msgClose        = 'C'
	msgSync         = 'S'
	msgFlush        = 'H'
	msgFunctionCall = 'F'
	msgTerminate    = 'X'
	msgCopyData     = 'd'
	msgCopyDone     = 'c'
	msgCopyFail     = 'f'

	msgReadyForQuery       = 'Z'
	msgErrorResponse       = 'E'
	msgNoticeResponse      = 'N'
	msgNotification        = 'A'
	msgParameterStatus     = 'S'
	msgDataRow             = 'D'
	msgCommandComplete     = 'C'
	msgEmptyQueryResponse  = 'I'
	msgPortalSuspended     = 's'
	msgAuthentication      = 'R'
	msgBackendKeyData      = 'K'
	msgNegotiateProtocolVn = 'v'
)

const (
	codeSSLRequest    = 80877103
	codeGSSEncRequest = 80877104
	codeCancelRequest = 80877102

	maxStartupLen   = 10000
	maxClientMsgLen = 64 << 20
	maxServerMsgLen = 1 << 30
	keepBufferBelow = 1 << 20
)

// frameReader reads protocol messages, reusing one buffer between calls.
type frameReader struct {
	r   *bufio.Reader
	max int
	buf []byte
}

// next returns the type and body of the next message. The body is valid only
// until the following call.
func (f *frameReader) next() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(f.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint32(hdr[1:])) - 4
	if n < 0 || n > f.max {
		return 0, nil, fmt.Errorf("message %q: invalid length %d", hdr[0], n)
	}
	if cap(f.buf) < n || (cap(f.buf) > keepBufferBelow && n <= keepBufferBelow) {
		f.buf = make([]byte, n)
	}
	body := f.buf[:n]
	if _, err := io.ReadFull(f.r, body); err != nil {
		return 0, nil, err
	}
	return hdr[0], body, nil
}

func writeFrame(w io.Writer, typ byte, body []byte) error {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(body)+4))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// cstring splits a NUL-terminated string off the front of b.
func cstring(b []byte) (string, []byte) {
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return string(b), nil
	}
	return string(b[:i]), b[i+1:]
}

func errorBody(severity, code, message string) []byte {
	var b []byte
	for _, f := range [][2]string{{"S", severity}, {"V", severity}, {"C", code}, {"M", message}} {
		b = append(b, f[0]...)
		b = append(b, f[1]...)
		b = append(b, 0)
	}
	return append(b, 0)
}

// errorMessage extracts the human-readable text of an ErrorResponse body.
func errorMessage(body []byte) string {
	var code, msg string
	for len(body) > 0 && body[0] != 0 {
		field := body[0]
		var v string
		v, body = cstring(body[1:])
		switch field {
		case 'C':
			code = v
		case 'M':
			msg = v
		}
	}
	return code + ": " + msg
}

func parameterStatusBody(name, value string) []byte {
	b := append([]byte(name), 0)
	return append(append(b, value...), 0)
}
