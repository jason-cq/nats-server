// Copyright 2020 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type wsOpCode int

const (
	// From https://tools.ietf.org/html/rfc6455#section-5.2
	wsTextMessage   = wsOpCode(1)
	wsBinaryMessage = wsOpCode(2)
	wsCloseMessage  = wsOpCode(8)
	wsPingMessage   = wsOpCode(9)
	wsPongMessage   = wsOpCode(10)

	wsFinalBit = 1 << 7
	wsRsv1Bit  = 1 << 6 // Used for compression, from https://tools.ietf.org/html/rfc7692#section-6
	wsRsv2Bit  = 1 << 5
	wsRsv3Bit  = 1 << 4

	wsMaskBit = 1 << 7

	wsContinuationFrame     = 0
	wsMaxControlPayloadSize = 125

	// From https://tools.ietf.org/html/rfc6455#section-11.7
	wsCloseStatusNormalClosure      = 1000
	wsCloseStatusGoingAway          = 1001
	wsCloseStatusProtocolError      = 1002
	wsCloseStatusUnsupportedData    = 1003
	wsCloseStatusNoStatusReceived   = 1005
	wsCloseStatusAbnormalClosure    = 1006
	wsCloseStatusInvalidPayloadData = 1007
	wsCloseStatusPolicyViolation    = 1008
	wsCloseStatusMessageTooBig      = 1009
	wsCloseStatusInternalSrvError   = 1011
	wsCloseStatusTLSHandshake       = 1015
)

const (
	minCompressionLevel     = flate.HuffmanOnly
	maxCompressionLevel     = flate.BestCompression
	defaultCompressionLevel = flate.BestSpeed
)

var (
	compressorPool   [maxCompressionLevel - minCompressionLevel + 1]sync.Pool
	decompressorPool sync.Pool
)

// From https://tools.ietf.org/html/rfc6455#section-1.3
var wsGUID = []byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11")

type wsUpgradeResult struct {
	conn     net.Conn
	compress bool
}

type wsReadInfo struct {
	rem   int
	fs    bool
	ff    bool
	fc    bool
	mkpos byte
	mkey  [4]byte
	buf   []byte
}

func (r *wsReadInfo) init() {
	r.fs, r.ff = true, true
}

// Returns a slice containing `needed` bytes from the given buffer `buf`
// starting at position `pos`, and possibly read from the given reader `r`.
// When bytes are present in `buf`, the `pos` is incremented by the number
// of bytes found up to `needed` and the new position is returned. If not
// enough bytes are found, the bytes found in `buf` are copied to the returned
// slice and the remaning bytes are read from `r`.
func wsGet(r io.Reader, buf []byte, pos, needed int) ([]byte, int, error) {
	avail := len(buf) - pos
	if avail >= needed {
		return buf[pos : pos+needed], pos + needed, nil
	}
	b := make([]byte, needed)
	start := copy(b, buf[pos:])
	for start != needed {
		n, err := r.Read(b[start:cap(b)])
		if err != nil {
			return nil, 0, err
		}
		start += n
	}
	return b, pos + avail, nil
}

// Returns a slice of byte slices corresponding to payload of websocket frames.
// The byte slice `buf` is filled with bytes from the connection's read loop.
// This function will decode the frame headers and unmask the payload(s).
// It is possible that the returned slices point to the given `buf` slice, so
// `buf` should not be overwritten until the returned slices have been parsed.
//
// Client lock MUST NOT be held on entry.
func (c *client) wsRead(r *wsReadInfo, ior io.Reader, buf []byte) ([][]byte, error) {
	var (
		bufs   [][]byte
		tmpBuf []byte
		err    error
		pos    int
		max    = len(buf)
	)
	for pos != max {
		if r.fs {
			b0 := buf[pos]
			frameType := wsOpCode(b0 & 0xF)
			final := b0&wsFinalBit != 0
			compressed := b0&wsRsv1Bit != 0
			pos++

			tmpBuf, pos, err = wsGet(ior, buf, pos, 1)
			if err != nil {
				return bufs, err
			}
			b1 := tmpBuf[0]

			// Clients MUST set the mask bit. If not set, reject.
			if b1&wsMaskBit == 0 {
				return bufs, c.wsHandleProtocolError("mask bit missing")
			}

			// Store size in case it is < 125
			r.rem = int(b1 & 0x7F)

			switch frameType {
			case wsPingMessage, wsPongMessage, wsCloseMessage:
				if r.rem > wsMaxControlPayloadSize {
					return bufs, c.wsHandleProtocolError(
						fmt.Sprintf("control frame length bigger than maximum allowed of %v bytes",
							wsMaxControlPayloadSize))
				}
				if !final {
					return bufs, c.wsHandleProtocolError("control frame does not have final bit set")
				}
			case wsTextMessage, wsBinaryMessage:
				if !r.ff {
					return bufs, c.wsHandleProtocolError("new message started before final frame for previous message was received")
				}
				r.ff = final
				r.fc = compressed
			case wsContinuationFrame:
				// Compressed bit must be only set in the first frame
				if r.ff || compressed {
					return bufs, c.wsHandleProtocolError("invalid continuation frame")
				}
				r.ff = final
			default:
				return bufs, c.wsHandleProtocolError(fmt.Sprintf("unknown opcode %v", frameType))
			}

			switch r.rem {
			case 126:
				tmpBuf, pos, err = wsGet(ior, buf, pos, 2)
				if err != nil {
					return bufs, err
				}
				r.rem = int(binary.BigEndian.Uint16(tmpBuf))
			case 127:
				tmpBuf, pos, err = wsGet(ior, buf, pos, 8)
				if err != nil {
					return bufs, err
				}
				r.rem = int(binary.BigEndian.Uint64(tmpBuf))
			}

			// Read masking key
			tmpBuf, pos, err = wsGet(ior, buf, pos, 4)
			if err != nil {
				return bufs, err
			}
			copy(r.mkey[:], tmpBuf)
			r.mkpos = 0

			// Handle control messages in place...
			if wsIsControlFrame(frameType) {
				pos, err = c.wsHandleControlFrame(r, frameType, ior, buf, pos)
				if err != nil {
					return bufs, err
				}
				continue
			}

			// Done with the frame header
			r.fs = false
		}
		if pos < max {
			var b []byte
			var n int

			n = r.rem
			if pos+n > max {
				n = max - pos
			}
			b = buf[pos : pos+n]
			pos += n
			r.rem -= n
			if r.fc && r.rem > 0 {
				r.buf = append(r.buf, b...)
			} else {
				if r.fc && r.buf != nil {
					r.buf = append(r.buf, b...)
					b = r.buf
				}
				r.unmask(b)
				if r.fc {
					// As per https://tools.ietf.org/html/rfc7692#section-7.2.2
					// add 0x00, 0x00, 0xff, 0xff and then a final block so that flate reader
					// does not report unexpected EOF.
					b = append(b, 0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff)
					br := bytes.NewBuffer(b)
					d, _ := decompressorPool.Get().(io.ReadCloser)
					if d == nil {
						d = flate.NewReader(br)
					} else {
						d.(flate.Resetter).Reset(br, nil)
					}
					b, err = ioutil.ReadAll(d)
					decompressorPool.Put(d)
					if err != nil {
						return bufs, err
					}
				}
				bufs = append(bufs, b)
				if r.rem == 0 {
					r.fs, r.fc, r.buf = true, false, nil
				}
			}
		}
	}
	return bufs, nil
}

// Handles the PING, PONG and CLOSE websocket control frames.
//
// Client lock MUST NOT be held on entry.
func (c *client) wsHandleControlFrame(r *wsReadInfo, frameType wsOpCode, nc io.Reader, buf []byte, pos int) (int, error) {
	var payload []byte
	var err error

	statusPos := pos
	if r.rem > 0 {
		payload, pos, err = wsGet(nc, buf, pos, r.rem)
		if err != nil {
			return pos, err
		}
		r.unmask(payload)
		r.rem = 0
	}
	switch frameType {
	case wsCloseMessage:
		status := wsCloseStatusNoStatusReceived
		body := ""
		// If there is a payload, it should contain 2 unsigned bytes
		// that represent the status code and then optional payload.
		if len(payload) >= 2 {
			status = int(binary.BigEndian.Uint16(buf[statusPos : statusPos+2]))
			body = string(buf[statusPos+2 : statusPos+len(payload)])
			if body != "" && !utf8.ValidString(body) {
				// https://tools.ietf.org/html/rfc6455#section-5.5.1
				// If body is present, it must be a valid utf8
				status = wsCloseStatusInvalidPayloadData
				body = "invalid utf8 body in close frame"
			}
		}
		c.wsEnqueueControlMessage(wsCloseMessage, wsFrameCloseMessage(status, body))
		// Return io.EOF so that readLoop will close the connection as ClientClosed
		// after processing pending buffers.
		return pos, io.EOF
	case wsPingMessage:
		c.wsEnqueueControlMessage(wsPongMessage, payload)
	case wsPongMessage:
		// Nothing to do..
	}
	return pos, nil
}

// Unmask the given slice.
func (r *wsReadInfo) unmask(buf []byte) {
	p := int(r.mkpos)
	if len(buf) < 16 {
		for i := 0; i < len(buf); i++ {
			buf[i] ^= r.mkey[p&3]
			p++
		}
		r.mkpos = byte(p & 3)
		return
	}
	var k [8]byte
	for i := 0; i < 8; i++ {
		k[i] = r.mkey[(p+i)&3]
	}
	km := binary.BigEndian.Uint64(k[:])
	n := (len(buf) / 8) * 8
	for i := 0; i < n; i += 8 {
		tmp := binary.BigEndian.Uint64(buf[i : i+8])
		tmp ^= km
		binary.BigEndian.PutUint64(buf[i:], tmp)
	}
	buf = buf[n:]
	for i := 0; i < len(buf); i++ {
		buf[i] ^= r.mkey[p&3]
		p++
	}
	r.mkpos = byte(p & 3)
}

// Returns true if the op code corresponds to a control frame.
func wsIsControlFrame(frameType wsOpCode) bool {
	return frameType >= wsCloseMessage
}

// Creates a frame header for the given op code and possibly compress the given `payload`
//
// Client lock is held on entry.
func (c *client) wsCreateFrameAndPayload(frameType wsOpCode, payload []byte) ([]byte, []byte) {
	compress := c.flags.isSet(wsCompress) && !wsIsControlFrame(frameType)
	if compress {
		// In case server object not avail, use default compression.
		cl := defaultCompressionLevel
		if c.srv != nil {
			cl = c.srv.websocket.compressionLevel
		}
		buf := &bytes.Buffer{}
		cpool := &(compressorPool[cl-minCompressionLevel])
		compressor, _ := cpool.Get().(*flate.Writer)
		if compressor == nil {
			compressor, _ = flate.NewWriter(buf, cl)
		} else {
			compressor.Reset(buf)
		}
		compressor.Write(payload)
		compressor.Flush()
		cpool.Put(compressor)
		rawBytes := buf.Bytes()
		payload = rawBytes[:len(rawBytes)-4]
	}
	return wsFrameMessage(compress, frameType, len(payload)), payload
}

// Create the frame header.
// Encodes the frame type and optional compression flag, and the size of the payload.
func wsFrameMessage(compress bool, frameType wsOpCode, l int) []byte {
	// websocket frame header
	var fh []byte

	b := byte(frameType | wsFinalBit)
	if compress {
		b |= wsRsv1Bit
	}

	switch {
	case l <= 125:
		fh = make([]byte, 2)
		fh[0] = b
		fh[1] = byte(l)
	case l < 65536:
		fh = make([]byte, 2+2)
		fh[0] = b
		fh[1] = 126
		binary.BigEndian.PutUint16(fh[2:], uint16(l))
	default:
		fh = make([]byte, 2+8)
		fh[0] = b
		fh[1] = 127
		binary.BigEndian.PutUint64(fh[2:], uint64(l))
	}
	return fh
}

// Invokes wsEnqueueControlMessageLocked under client lock.
//
// Client lock MUST NOT be held on entry
func (c *client) wsEnqueueControlMessage(controlMsg wsOpCode, payload []byte) {
	c.mu.Lock()
	c.wsEnqueueControlMessageLocked(controlMsg, payload)
	c.mu.Unlock()
}

// Enqueues a websocket control message.
// If the control message is a wsCloseMessage, then marks this client
// has having sent the close message (since only one should be sent).
// This will prevent the generic closeConnection() to enqueue one.
//
// Client lock held on entry.
func (c *client) wsEnqueueControlMessageLocked(controlMsg wsOpCode, payload []byte) {
	// Control messages are never compressed.
	h := wsFrameMessage(false, controlMsg, len(payload))
	c.queueOutbound(h)
	// Note that payload is optional.
	if len(payload) > 0 {
		c.queueOutbound(payload)
	}
	if controlMsg == wsCloseMessage {
		c.flags.set(wsCloseMsgSent)
	}
}

// Enqueues a websocket close message with a status mapped from the given `reason`.
//
// Client lock held on entry
func (c *client) wsEnqueueCloseMessage(reason ClosedState) {
	var status int
	switch reason {
	case ClientClosed:
		status = wsCloseStatusNormalClosure
	case AuthenticationTimeout, AuthenticationViolation, SlowConsumerPendingBytes, SlowConsumerWriteDeadline,
		MaxAccountConnectionsExceeded, MaxConnectionsExceeded, MaxControlLineExceeded, MaxSubscriptionsExceeded,
		MissingAccount, AuthenticationExpired, Revocation:
		status = wsCloseStatusPolicyViolation
	case TLSHandshakeError:
		status = wsCloseStatusTLSHandshake
	case ParseError, ProtocolViolation, BadClientProtocolVersion:
		status = wsCloseStatusProtocolError
	case MaxPayloadExceeded:
		status = wsCloseStatusMessageTooBig
	case ServerShutdown:
		status = wsCloseStatusGoingAway
	case WriteError, ReadError, StaleConnection:
		status = wsCloseStatusAbnormalClosure
	default:
		status = wsCloseStatusInternalSrvError
	}
	body := wsFrameCloseMessage(status, reason.String())
	c.wsEnqueueControlMessageLocked(wsCloseMessage, body)
}

// Create and then enqueue a close message with a protocol error and the
// given message. This is invoked when parsing websocket frames.
//
// Lock MUST NOT be held on entry.
func (c *client) wsHandleProtocolError(message string) error {
	buf := wsFrameCloseMessage(wsCloseStatusProtocolError, message)
	c.wsEnqueueControlMessage(wsCloseMessage, buf)
	return fmt.Errorf(message)
}

// Create a frame for a close message with the given `status` and `body`.
// If the `body` is more than the maximum allows control frame payload size,
// it is truncated and "..." is added at the end (as a hint that message
// is not complete).
func wsFrameCloseMessage(status int, body string) []byte {
	// Since a control message payload is limited in size, we
	// will limit the text and add trailing "..." if truncated.
	if len(body) > wsMaxControlPayloadSize {
		body = body[:wsMaxControlPayloadSize-3]
		body += "..."
	}
	buf := make([]byte, 2+len(body))
	// We need to have a 2 byte unsigned int that represents the error status code
	// https://tools.ietf.org/html/rfc6455#section-5.5.1
	binary.BigEndian.PutUint16(buf[:2], uint16(status))
	copy(buf[2:], []byte(body))
	return buf
}

// Process websocket client handshake. On success, returns the raw net.Conn that
// will be used to create a *client object.
// Invoked from the HTTP server listening on websocket port.
func (s *Server) wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsUpgradeResult, error) {
	opts := s.getOpts()

	// From https://tools.ietf.org/html/rfc6455#section-4.2.1
	// Point 1.
	if r.Method != "GET" {
		return nil, wsReturnHTTPError(w, http.StatusMethodNotAllowed, "request method must be GET")
	}
	// Point 2.
	if r.Host == "" {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "'Host' missing in request")
	}
	// Point 3.
	if !wsHeaderContains(r.Header, "Upgrade", "websocket") {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "invalid value for header 'Uprade'")
	}
	// Point 4.
	if !wsHeaderContains(r.Header, "Connection", "Upgrade") {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "invalid value for header 'Connection'")
	}
	// Point 5.
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "key missing")
	}
	// Point 6.
	if !wsHeaderContains(r.Header, "Sec-Websocket-Version", "13") {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "invalid version")
	}
	// Others are optional
	// Point 7.
	if opts.Websocket.CheckOrigin && !wsCheckOrigin(r, opts.Websocket.Origin) {
		return nil, wsReturnHTTPError(w, http.StatusForbidden, "invalid request origin")
	}
	// Point 8.
	// We don't have protocols, so ignore.
	// Point 9.
	// Extensions, only support for compression at the moment
	compress := opts.Websocket.Compression
	if compress {
		compress = wsClientSupportsCompression(r.Header)
	}

	h := w.(http.Hijacker)
	conn, brw, err := h.Hijack()
	if err != nil {
		return nil, wsReturnHTTPError(w, http.StatusInternalServerError, err.Error())
	}
	if brw.Reader.Buffered() > 0 {
		conn.Close()
		return nil, errors.New("client sent data before handshake is complete")
	}

	var buf [1024]byte
	p := buf[:0]

	// From https://tools.ietf.org/html/rfc6455#section-4.2.2
	p = append(p, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "...)
	p = append(p, wsAcceptKey(key)...)
	p = append(p, _CRLF_...)
	if compress {
		p = append(p, "Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n"...)
	}
	p = append(p, _CRLF_...)

	if _, err = conn.Write(p); err != nil {
		conn.Close()
		return nil, err
	}
	// If there was a deadline set for the handshake, clear it now.
	if opts.Websocket.HandshakeTimeout > 0 {
		conn.SetDeadline(time.Time{})
	}
	return &wsUpgradeResult{conn, compress}, nil
}

// Returns true if the header named `name` contains a token with value `value`.
func wsHeaderContains(header http.Header, name string, value string) bool {
	for _, s := range header[name] {
		tokens := strings.Split(s, ",")
		for _, t := range tokens {
			t = strings.Trim(t, " \t")
			if strings.EqualFold(t, value) {
				return true
			}
		}
	}
	return false
}

// Return true if the client has "permessage-deflate" in its extensions.
func wsClientSupportsCompression(header http.Header) bool {
	for _, extensionList := range header["Sec-Websocket-Extensions"] {
		extensions := strings.Split(extensionList, ",")
		for _, extension := range extensions {
			extension = strings.Trim(extension, " \t")
			params := strings.Split(extension, ";")
			for _, p := range params {
				p = strings.Trim(p, " \t")
				if strings.EqualFold(p, "permessage-deflate") {
					return true
				}
			}
		}
	}
	return false
}

// Send an HTTP error with the given `status`` to the given http response writer `w`.
// Return an error created based on the `reason` string.
func wsReturnHTTPError(w http.ResponseWriter, status int, reason string) error {
	err := fmt.Errorf("websocket handshake error: %s", reason)
	w.Header().Set("Sec-Websocket-Version", "13")
	http.Error(w, http.StatusText(status), status)
	return err
}

// Return true if the client request does not have a Origin header,
// or if it does, it is equal to `expected` value if not empty,
// or to the request's Host.
func wsCheckOrigin(r *http.Request, expected string) bool {
	origin := r.Header["Origin"]
	if len(origin) == 0 {
		return true
	}
	u, err := url.Parse(origin[0])
	if err != nil {
		return false
	}
	if expected == "" {
		expected = u.Host
	}
	return strings.EqualFold(origin[0], expected)
}

// Concatenate the key sent by the client with the GUID, then computes the SHA1 hash
// and returns it as a based64 encoded string.
func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write(wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Validate the websocket related options.
func validateWebsocketOptions(o *Options) error {
	// If no port is defined, we don't care about other options
	if o.Websocket.Port == 0 {
		return nil
	}
	if o.Websocket.CompressionLevel < -2 || o.Websocket.CompressionLevel > 9 {
		return fmt.Errorf("valid range for compression level is [-2, 9], got %v", o.Websocket.CompressionLevel)
	}
	return nil
}

func (s *Server) startWebsocketServer() {
	sopts := s.getOpts()
	o := &sopts.Websocket

	var hl net.Listener
	var proto string
	var err error

	port := o.Port
	if port == -1 {
		port = 0
	}
	hp := net.JoinHostPort(o.Host, strconv.Itoa(port))

	if o.TLSConfig != nil {
		proto = "wss"
		config := o.TLSConfig.Clone()
		config.ClientAuth = tls.NoClientCert
		hl, err = tls.Listen("tcp", hp, config)
	} else {
		proto = "ws"
		hl, err = net.Listen("tcp", hp)
	}
	if err != nil {
		s.Fatalf("Unable to listen for websocket connections: %v", err)
		return
	}
	s.Noticef("Listening for websocket clients on %s://%s:%d", proto, o.Host, port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.wsUpgrade(w, r)
		if err != nil {
			s.Errorf(err.Error())
			return
		}
		wsFlags := wsClient
		if res.compress {
			wsFlags |= wsCompress
		}
		s.createClient(res.conn, wsFlags)
	})
	hs := &http.Server{
		Addr:        hp,
		Handler:     mux,
		ReadTimeout: o.HandshakeTimeout,
		ErrorLog:    log.New(&wsCaptureHTTPServerLog{s}, "", 0),
	}
	s.mu.Lock()
	s.websocket.server = hs
	s.websocket.listener = hl
	s.websocket.tls = proto == "wss"
	if port == -1 {
		s.opts.Websocket.Port = hl.Addr().(*net.TCPAddr).Port
	}
	s.mu.Unlock()

	s.startGoRoutine(func() {
		defer s.grWG.Done()

		if err := hs.Serve(hl); err != http.ErrServerClosed {
			s.Fatalf("websocket listener error: %v", err)
		}
		s.done <- true
	})
}

type wsCaptureHTTPServerLog struct {
	s *Server
}

func (cl *wsCaptureHTTPServerLog) Write(p []byte) (int, error) {
	var buf [128]byte
	var b = buf[:0]

	copy(b, []byte("websocket :"))
	offset := 0
	if bytes.HasPrefix(p, []byte("http:")) {
		offset = 6
	}
	b = append(b, p[offset:]...)
	cl.s.Errorf(string(b))
	return len(p), nil
}
