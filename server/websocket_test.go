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
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type testReader struct {
	buf []byte
	pos int
	max int
	err error
}

func (tr *testReader) Read(p []byte) (int, error) {
	if tr.err != nil {
		return 0, tr.err
	}
	n := len(tr.buf) - tr.pos
	if n == 0 {
		return 0, nil
	}
	if n > cap(p) {
		n = cap(p)
	}
	if tr.max > 0 && n > tr.max {
		n = tr.max
	}
	copy(p, tr.buf[tr.pos:tr.pos+n])
	tr.pos += n
	return n, nil
}

func TestWSGet(t *testing.T) {
	rb := []byte("012345")

	tr := &testReader{buf: []byte("6789")}

	for _, test := range []struct {
		name   string
		pos    int
		needed int
		newpos int
		trmax  int
		result string
		reterr bool
	}{
		{"fromrb1", 0, 3, 3, 4, "012", false},    // Partial from read buffer
		{"fromrb2", 3, 2, 5, 4, "34", false},     // Partial from read buffer
		{"fromrb3", 5, 1, 6, 4, "5", false},      // Partial from read buffer
		{"fromtr1", 4, 4, 6, 4, "4567", false},   // Partial from read buffer + some of ioReader
		{"fromtr2", 4, 6, 6, 4, "456789", false}, // Partial from read buffer + all of ioReader
		{"fromtr3", 4, 6, 6, 2, "456789", false}, // Partial from read buffer + all of ioReader with several reads
		{"fromtr4", 4, 6, 6, 2, "", true},        // ioReader returns error
	} {
		t.Run(test.name, func(t *testing.T) {
			tr.pos = 0
			tr.max = test.trmax
			if test.reterr {
				tr.err = fmt.Errorf("on purpose")
			}
			res, np, err := wsGet(tr, rb, test.pos, test.needed)
			if test.reterr {
				if err == nil {
					t.Fatalf("Expected error, got none")
				}
				if err.Error() != "on purpose" {
					t.Fatalf("Unexpected error: %v", err)
				}
				if np != 0 || res != nil {
					t.Fatalf("Unexpected returned values: res=%v n=%v", res, np)
				}
				return
			}
			if err != nil {
				t.Fatalf("Error on get: %v", err)
			}
			if np != test.newpos {
				t.Fatalf("Expected pos=%v, got %v", test.newpos, np)
			}
			if string(res) != test.result {
				t.Fatalf("Invalid returned content: %s", res)
			}
		})
	}
}

func TestWSIsControlFrame(t *testing.T) {
	for _, test := range []struct {
		name      string
		code      wsOpCode
		isControl bool
	}{
		{"binary", wsBinaryMessage, false},
		{"text", wsTextMessage, false},
		{"ping", wsPingMessage, true},
		{"pong", wsPongMessage, true},
		{"close", wsCloseMessage, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if res := wsIsControlFrame(test.code); res != test.isControl {
				t.Fatalf("Expected %q isControl to be %v, got %v", test.name, test.isControl, res)
			}
		})
	}
}

func testWSSimpleMask(key, buf []byte) {
	for i := 0; i < len(buf); i++ {
		buf[i] ^= key[i&3]
	}
}

func TestWSUnmask(t *testing.T) {
	key := []byte{1, 2, 3, 4}
	orgBuf := []byte("this is a clear text")

	mask := func() []byte {
		t.Helper()
		buf := append([]byte(nil), orgBuf...)
		testWSSimpleMask(key, buf)
		// First ensure that the content is masked.
		if bytes.Equal(buf, orgBuf) {
			t.Fatalf("Masking did not do anything: %q", buf)
		}
		return buf
	}

	ri := &wsReadInfo{}
	ri.init()
	copy(ri.mkey[:], key)

	buf := mask()
	// Unmask in one call
	ri.unmask(buf)
	if !bytes.Equal(buf, orgBuf) {
		t.Fatalf("Unmask error, expected %q, got %q", orgBuf, buf)
	}

	// Unmask in multiple calls
	buf = mask()
	ri.mkpos = 0
	ri.unmask(buf[:3])
	ri.unmask(buf[3:11])
	ri.unmask(buf[11:])
	if !bytes.Equal(buf, orgBuf) {
		t.Fatalf("Unmask error, expected %q, got %q", orgBuf, buf)
	}
}

func TestWSCreateCloseMessage(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		psize     int
		truncated bool
	}{
		{"fits", wsCloseStatusInternalSrvError, 10, false},
		{"truncated", wsCloseStatusProtocolError, wsMaxControlPayloadSize + 10, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := make([]byte, test.psize)
			for i := 0; i < len(payload); i++ {
				payload[i] = byte('A' + (i % 26))
			}
			res := wsCreateCloseMessage(test.status, string(payload))
			if status := binary.BigEndian.Uint16(res[:2]); int(status) != test.status {
				t.Fatalf("Expected status to be %v, got %v", test.status, status)
			}
			psize := len(res) - 2
			if !test.truncated {
				if int(psize) != test.psize {
					t.Fatalf("Expected size to be %v, got %v", test.psize, psize)
				}
				if !bytes.Equal(res[2:], payload) {
					t.Fatalf("Unexpected result: %q", res[2:])
				}
				return
			}
			if int(psize) != wsMaxControlPayloadSize {
				t.Fatalf("Expected size to be capped to %v, got %v", wsMaxControlPayloadSize, psize)
			}
			if string(res[len(res)-3:]) != "..." {
				t.Fatalf("Expected res to have `...` at the end, got %q", res[2:])
			}
		})
	}
}

func TestWSFrameMessage(t *testing.T) {
	for _, test := range []struct {
		name       string
		frameType  wsOpCode
		compressed bool
		len        int
	}{
		{"uncompressed 10", wsBinaryMessage, false, 10},
		{"uncompressed 600", wsTextMessage, false, 600},
		{"uncompressed 100000", wsTextMessage, false, 100000},
		{"compressed 10", wsBinaryMessage, true, 10},
		{"compressed 600", wsBinaryMessage, true, 600},
		{"compressed 100000", wsTextMessage, true, 100000},
	} {
		t.Run(test.name, func(t *testing.T) {
			res := wsFrameMessage(test.compressed, test.frameType, test.len)
			// The server is always sending the message has a single frame,
			// so the "final" bit should be set.
			expected := byte(test.frameType) | wsFinalBit
			if test.compressed {
				expected |= wsRsv1Bit
			}
			if b := res[0]; b != expected {
				t.Fatalf("Expected first byte to be %v, got %v", expected, b)
			}
			switch {
			case test.len <= 125:
				if len(res) != 2 {
					t.Fatalf("Frame len should be 2, got %v", len(res))
				}
				if res[1] != byte(test.len) {
					t.Fatalf("Expected len to be in second byte and be %v, got %v", test.len, res[1])
				}
			case test.len < 65536:
				// 1+1+2
				if len(res) != 4 {
					t.Fatalf("Frame len should be 4, got %v", len(res))
				}
				if res[1] != 126 {
					t.Fatalf("Second byte value should be 126, got %v", res[1])
				}
				if rl := binary.BigEndian.Uint16(res[2:]); int(rl) != test.len {
					t.Fatalf("Expected len to be %v, got %v", test.len, rl)
				}
			default:
				// 1+1+8
				if len(res) != 10 {
					t.Fatalf("Frame len should be 10, got %v", len(res))
				}
				if res[1] != 127 {
					t.Fatalf("Second byte value should be 127, got %v", res[1])
				}
				if rl := binary.BigEndian.Uint64(res[2:]); int(rl) != test.len {
					t.Fatalf("Expected len to be %v, got %v", test.len, rl)
				}
			}
		})
	}
}

func TestWSCreateFrameAndPayload(t *testing.T) {
	uncompressed := []byte("this is a buffer with some data that is compresseddddddddddddddddddddd")
	buf := &bytes.Buffer{}
	compressor, _ := flate.NewWriter(buf, 1)
	compressor.Write(uncompressed)
	compressor.Flush()
	compressed := buf.Bytes()
	// The last 4 bytes are dropped
	compressed = compressed[:len(compressed)-4]

	for _, test := range []struct {
		name       string
		frameType  wsOpCode
		compressed bool
	}{
		{"binary", wsBinaryMessage, false},
		{"binary compressed", wsBinaryMessage, true},
		{"text", wsTextMessage, false},
		{"text compressed", wsTextMessage, true},
		{"ping", wsPingMessage, false},
		{"ping compressed", wsPingMessage, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// If compression, stress the fact that we use a pool and
			// want to get to the case where we get from the pool and reset.
			iter := 1
			if test.compressed {
				iter = 10
			}
			for i := 0; i < iter; i++ {
				header, payload := wsCreateFrameAndPayload(test.frameType, test.compressed, 1, uncompressed)
				expectedB0 := byte(test.frameType) | wsFinalBit
				expectedB1 := byte(len(uncompressed))
				expectedPS := len(uncompressed)
				if test.compressed && !wsIsControlFrame(test.frameType) {
					expectedB0 |= wsRsv1Bit
					expectedPS = len(compressed)
					expectedB1 = byte(expectedPS)
				}
				if b := header[0]; b != expectedB0 {
					t.Fatalf("Expected first byte to be %v, got %v", expectedB0, b)
				}
				if b := header[1]; b != expectedB1 {
					t.Fatalf("Expected second byte to be %v, got %v", expectedB1, b)
				}
				if len(payload) != expectedPS {
					t.Fatalf("Expected payload length to be %v, got %v", expectedPS, len(payload))
				}
			}
		})
	}
}

func testWSCreateClientMsg(frameType wsOpCode, frameNum int, final, compressed bool, payload []byte) []byte {
	if compressed {
		buf := &bytes.Buffer{}
		compressor, _ := flate.NewWriter(buf, 1)
		compressor.Write(payload)
		compressor.Flush()
		payload = buf.Bytes()
		// The last 4 bytes are dropped
		payload = payload[:len(payload)-4]
	}
	frame := make([]byte, 14+len(payload))
	if frameNum == 1 {
		frame[0] = byte(frameType)
	}
	if final {
		frame[0] |= wsFinalBit
	}
	if compressed {
		frame[0] |= wsRsv1Bit
	}
	pos := 1
	lenPayload := len(payload)
	switch {
	case lenPayload <= 125:
		frame[pos] = byte(lenPayload) | wsMaskBit
		pos++
	case lenPayload < 65536:
		frame[pos] = 126 | wsMaskBit
		binary.BigEndian.PutUint16(frame[2:], uint16(lenPayload))
		pos += 3
	default:
		frame[1] = 127 | wsMaskBit
		binary.BigEndian.PutUint64(frame[2:], uint64(lenPayload))
		pos += 9
	}
	key := []byte{1, 2, 3, 4}
	copy(frame[pos:], key)
	pos += 4
	copy(frame[pos:], payload)
	testWSSimpleMask(key, frame[pos:])
	pos += lenPayload
	return frame[:pos]
}

func testWSSetupForRead() (*client, *wsReadInfo, *testReader) {
	ri := &wsReadInfo{}
	ri.init()
	tr := &testReader{}
	opts := DefaultOptions()
	opts.MaxPending = MAX_PENDING_SIZE
	s := &Server{opts: opts}
	c := &client{srv: s, flags: wsClient}
	c.initClient()
	return c, ri, tr
}

func TestWSReadUncompressedFrames(t *testing.T) {
	c, ri, tr := testWSSetupForRead()
	// Create 2 WS messages
	pl1 := []byte("first message")
	wsmsg1 := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, pl1)
	pl2 := []byte("second message")
	wsmsg2 := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, pl2)
	// Add both in single buffer
	orgrb := append([]byte(nil), wsmsg1...)
	orgrb = append(orgrb, wsmsg2...)

	rb := append([]byte(nil), orgrb...)
	bufs, err := c.wsRead(ri, tr, rb)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 2 {
		t.Fatalf("Expected 2 buffers, got %v", n)
	}
	if !bytes.Equal(bufs[0], pl1) {
		t.Fatalf("Unexpected content for buffer 1: %s", bufs[0])
	}
	if !bytes.Equal(bufs[1], pl2) {
		t.Fatalf("Unexpected content for buffer 2: %s", bufs[1])
	}

	// Now reset and try with the read buffer not containing full ws frame
	c, ri, tr = testWSSetupForRead()
	rb = append([]byte(nil), orgrb...)
	// Frame is 1+1+4+'first message'. So say we pass with rb of 11 bytes,
	// then we should get "first"
	bufs, err = c.wsRead(ri, tr, rb[:11])
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 1 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
	if string(bufs[0]) != "first" {
		t.Fatalf("Unexpected content: %q", bufs[0])
	}
	// Call again with more data..
	bufs, err = c.wsRead(ri, tr, rb[11:32])
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 2 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
	if string(bufs[0]) != " message" {
		t.Fatalf("Unexpected content: %q", bufs[0])
	}
	if string(bufs[1]) != "second " {
		t.Fatalf("Unexpected content: %q", bufs[1])
	}
	// Call with the rest
	bufs, err = c.wsRead(ri, tr, rb[32:])
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 1 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
	if string(bufs[0]) != "message" {
		t.Fatalf("Unexpected content: %q", bufs[0])
	}
}

func TestWSReadCompressedFrames(t *testing.T) {
	c, ri, tr := testWSSetupForRead()
	uncompressed := []byte("this is the uncompress data")
	wsmsg1 := testWSCreateClientMsg(wsBinaryMessage, 1, true, true, uncompressed)
	rb := append([]byte(nil), wsmsg1...)
	// Call with some but not all of the payload
	bufs, err := c.wsRead(ri, tr, rb[:10])
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 0 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
	// Call with the rest, only then should we get the uncompressed data.
	bufs, err = c.wsRead(ri, tr, rb[10:])
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 1 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
	if !bytes.Equal(bufs[0], uncompressed) {
		t.Fatalf("Unexpected content: %s", bufs[0])
	}
	// Stress the fact that we use a pool and want to make sure
	// that if we get a decompressor from the pool, it is properly reset
	// with the buffer to decompress.
	for i := 0; i < 9; i++ {
		rb = append(rb, wsmsg1...)
	}
	bufs, err = c.wsRead(ri, tr, rb)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 10 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
}

func TestWSReadCompressedFrameCorrupted(t *testing.T) {
	c, ri, tr := testWSSetupForRead()
	uncompressed := []byte("this is the uncompress data")
	wsmsg1 := testWSCreateClientMsg(wsBinaryMessage, 1, true, true, uncompressed)
	copy(wsmsg1[10:], []byte{1, 2, 3, 4})
	rb := append([]byte(nil), wsmsg1...)
	bufs, err := c.wsRead(ri, tr, rb)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Expected error about corrupted data, got %v", err)
	}
	if n := len(bufs); n != 0 {
		t.Fatalf("Expected no buffer, got %v", n)
	}
}

func TestWSReadVariousFrameSizes(t *testing.T) {
	for _, test := range []struct {
		name string
		size int
	}{
		{"tiny", 100},
		{"medium", 1000},
		{"large", 70000},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, ri, tr := testWSSetupForRead()
			uncompressed := make([]byte, test.size)
			for i := 0; i < len(uncompressed); i++ {
				uncompressed[i] = 'A' + byte(i%26)
			}
			wsmsg1 := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, uncompressed)
			rb := append([]byte(nil), wsmsg1...)
			bufs, err := c.wsRead(ri, tr, rb)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if n := len(bufs); n != 1 {
				t.Fatalf("Unexpected buffer returned: %v", n)
			}
			if !bytes.Equal(bufs[0], uncompressed) {
				t.Fatalf("Unexpected content: %s", bufs[0])
			}
		})
	}
}

func TestWSReadFragmentedFrames(t *testing.T) {
	c, ri, tr := testWSSetupForRead()
	payloads := []string{"first", "second", "third"}
	var rb []byte
	for i := 0; i < len(payloads); i++ {
		final := i == len(payloads)-1
		frag := testWSCreateClientMsg(wsBinaryMessage, i+1, final, false, []byte(payloads[i]))
		rb = append(rb, frag...)
	}
	bufs, err := c.wsRead(ri, tr, rb)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 3 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
	for i, expected := range payloads {
		if string(bufs[i]) != expected {
			t.Fatalf("Unexpected content for buf=%v: %s", i, bufs[i])
		}
	}
}

func TestWSReadPartialFrameHeaderAtEndOfReadBuffer(t *testing.T) {
	c, ri, tr := testWSSetupForRead()
	msg1 := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, []byte("msg1"))
	msg2 := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, []byte("msg2"))
	rb := append([]byte(nil), msg1...)
	rb = append(rb, msg2...)
	// We will pass the first frame + the first byte of the next frame.
	rbl := rb[:len(msg1)+1]
	// Make the io reader return the rest of the frame
	tr.buf = rb[len(msg1)+1:]
	bufs, err := c.wsRead(ri, tr, rbl)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 1 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
	// We should not have asked to the io reader more than what is needed for reading
	// the frame header. Since we had already the first byte in the read buffer,
	// tr.pos should be 1(size)+4(key)=5
	if tr.pos != 5 {
		t.Fatalf("Expected reader pos to be 5, got %v", tr.pos)
	}
}

func TestWSReadPingFrame(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{"without payload", nil},
		{"with payload", []byte("optional payload")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, ri, tr := testWSSetupForRead()
			ping := testWSCreateClientMsg(wsPingMessage, 1, true, false, test.payload)
			rb := append([]byte(nil), ping...)
			bufs, err := c.wsRead(ri, tr, rb)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if n := len(bufs); n != 0 {
				t.Fatalf("Unexpected buffer returned: %v", n)
			}
			// A PONG should have been queued with the payload of the ping
			c.mu.Lock()
			nb := c.collapsePtoNB()
			c.mu.Unlock()
			if n := len(nb); n == 0 {
				t.Fatalf("Expected buffers, got %v", n)
			}
			if expected := 2 + len(test.payload); expected != len(nb[0]) {
				t.Fatalf("Expected buffer to be %v bytes long, got %v", expected, len(nb[0]))
			}
			b := nb[0][0]
			if b&wsFinalBit == 0 {
				t.Fatalf("Control frame should have been the final flag, it was not set: %v", b)
			}
			if b&byte(wsPongMessage) == 0 {
				t.Fatalf("Should have been a PONG, it wasn't: %v", b)
			}
			if len(test.payload) > 0 {
				if !bytes.Equal(nb[0][2:], test.payload) {
					t.Fatalf("Unexpected content: %s", nb[0][2:])
				}
			}
		})
	}
}

func TestWSReadPongFrame(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{"without payload", nil},
		{"with payload", []byte("optional payload")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, ri, tr := testWSSetupForRead()
			pong := testWSCreateClientMsg(wsPongMessage, 1, true, false, test.payload)
			rb := append([]byte(nil), pong...)
			bufs, err := c.wsRead(ri, tr, rb)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if n := len(bufs); n != 0 {
				t.Fatalf("Unexpected buffer returned: %v", n)
			}
			// Nothing should be sent...
			c.mu.Lock()
			nb := c.collapsePtoNB()
			c.mu.Unlock()
			if n := len(nb); n != 0 {
				t.Fatalf("Expected no buffer, got %v", n)
			}
		})
	}
}

func TestWSReadCloseFrame(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{"without payload", nil},
		{"with payload", []byte("optional payload")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, ri, tr := testWSSetupForRead()
			// a close message has a status in 2 bytes + optional payload
			payload := make([]byte, 2+len(test.payload))
			binary.BigEndian.PutUint16(payload[:2], wsCloseStatusNormalClosure)
			if len(test.payload) > 0 {
				copy(payload[2:], test.payload)
			}
			close := testWSCreateClientMsg(wsCloseMessage, 1, true, false, payload)
			// Have a normal frame prior to close to make sure that wsRead returns
			// the normal frame along with io.EOF to indicate that wsCloseMessage was received.
			msg := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, []byte("msg"))
			rb := append([]byte(nil), msg...)
			rb = append(rb, close...)
			bufs, err := c.wsRead(ri, tr, rb)
			// It is expected that wsRead returns io.EOF on processing a close.
			if err != io.EOF {
				t.Fatalf("Unexpected error: %v", err)
			}
			if n := len(bufs); n != 1 {
				t.Fatalf("Unexpected buffer returned: %v", n)
			}
			if string(bufs[0]) != "msg" {
				t.Fatalf("Unexpected content: %s", bufs[0])
			}
			// A CLOSE should have been queued with the payload of the original close message.
			c.mu.Lock()
			nb := c.collapsePtoNB()
			c.mu.Unlock()
			if n := len(nb); n == 0 {
				t.Fatalf("Expected buffers, got %v", n)
			}
			if expected := 2 + 2 + len(test.payload); expected != len(nb[0]) {
				t.Fatalf("Expected buffer to be %v bytes long, got %v", expected, len(nb[0]))
			}
			b := nb[0][0]
			if b&wsFinalBit == 0 {
				t.Fatalf("Control frame should have been the final flag, it was not set: %v", b)
			}
			if b&byte(wsCloseMessage) == 0 {
				t.Fatalf("Should have been a CLOSE, it wasn't: %v", b)
			}
			if status := binary.BigEndian.Uint16(nb[0][2:4]); status != wsCloseStatusNormalClosure {
				t.Fatalf("Expected status to be %v, got %v", wsCloseStatusNormalClosure, status)
			}
			if len(test.payload) > 0 {
				if !bytes.Equal(nb[0][4:], test.payload) {
					t.Fatalf("Unexpected content: %s", nb[0][4:])
				}
			}
		})
	}
}

func TestWSReadControlFrameBetweebFragmentedFrames(t *testing.T) {
	c, ri, tr := testWSSetupForRead()
	frag1 := testWSCreateClientMsg(wsBinaryMessage, 1, false, false, []byte("first"))
	frag2 := testWSCreateClientMsg(wsBinaryMessage, 2, true, false, []byte("second"))
	ctrl := testWSCreateClientMsg(wsPongMessage, 1, true, false, nil)
	rb := append([]byte(nil), frag1...)
	rb = append(rb, ctrl...)
	rb = append(rb, frag2...)
	bufs, err := c.wsRead(ri, tr, rb)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 2 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
	if string(bufs[0]) != "first" {
		t.Fatalf("Unexpected content: %s", bufs[0])
	}
	if string(bufs[1]) != "second" {
		t.Fatalf("Unexpected content: %s", bufs[1])
	}
}

func TestWSReadGetErrors(t *testing.T) {
	tr := &testReader{err: fmt.Errorf("on purpose")}
	for _, test := range []struct {
		lenPayload int
		rbextra    int
	}{
		{10, 1},
		{10, 3},
		{200, 1},
		{200, 2},
		{200, 5},
		{70000, 1},
		{70000, 5},
		{70000, 13},
	} {
		t.Run("", func(t *testing.T) {
			c, ri, _ := testWSSetupForRead()
			msg := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, []byte("msg"))
			frame := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, make([]byte, test.lenPayload))
			rb := append([]byte(nil), msg...)
			rb = append(rb, frame...)
			bufs, err := c.wsRead(ri, tr, rb[:len(msg)+test.rbextra])
			if err == nil || err.Error() != "on purpose" {
				t.Fatalf("Expected 'on purpose' error, got %v", err)
			}
			if n := len(bufs); n != 1 {
				t.Fatalf("Unexpected buffer returned: %v", n)
			}
			if string(bufs[0]) != "msg" {
				t.Fatalf("Unexpected content: %s", bufs[0])
			}
		})
	}
}

func TestWSHandleControlFrameErrors(t *testing.T) {
	c, ri, tr := testWSSetupForRead()
	tr.err = fmt.Errorf("on purpose")

	// a close message has a status in 2 bytes + optional payload
	text := []byte("this is a close message")
	payload := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(payload[:2], wsCloseStatusNormalClosure)
	copy(payload[2:], text)
	ctrl := testWSCreateClientMsg(wsCloseMessage, 1, true, false, payload)

	bufs, err := c.wsRead(ri, tr, ctrl[:len(ctrl)-4])
	if err == nil || err.Error() != "on purpose" {
		t.Fatalf("Expected 'on purpose' error, got %v", err)
	}
	if n := len(bufs); n != 0 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}

	// Alter the content of close message. It is supposed to be valid utf-8.
	c, ri, tr = testWSSetupForRead()
	cp := append([]byte(nil), payload...)
	cp[10] = 0xF1
	ctrl = testWSCreateClientMsg(wsCloseMessage, 1, true, false, cp)
	bufs, err = c.wsRead(ri, tr, ctrl)
	// We should still receive an EOF but the message enqueued to the client
	// should contain wsCloseStatusInvalidPayloadData and the error about invalid utf8
	if err != io.EOF {
		t.Fatalf("Unexpected error: %v", err)
	}
	if n := len(bufs); n != 0 {
		t.Fatalf("Unexpected buffer returned: %v", n)
	}
	c.mu.Lock()
	nb := c.collapsePtoNB()
	c.mu.Unlock()
	if n := len(nb); n == 0 {
		t.Fatalf("Expected buffers, got %v", n)
	}
	b := nb[0][0]
	if b&wsFinalBit == 0 {
		t.Fatalf("Control frame should have been the final flag, it was not set: %v", b)
	}
	if b&byte(wsCloseMessage) == 0 {
		t.Fatalf("Should have been a CLOSE, it wasn't: %v", b)
	}
	if status := binary.BigEndian.Uint16(nb[0][2:4]); status != wsCloseStatusInvalidPayloadData {
		t.Fatalf("Expected status to be %v, got %v", wsCloseStatusInvalidPayloadData, status)
	}
	if !bytes.Contains(nb[0][4:], []byte("utf8")) {
		t.Fatalf("Unexpected content: %s", nb[0][4:])
	}
}

func TestWSReadErrors(t *testing.T) {
	for _, test := range []struct {
		cframe func() []byte
		err    string
		nbufs  int
	}{
		{
			func() []byte {
				msg := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, []byte("hello"))
				msg[1] &= ^byte(wsMaskBit)
				return msg
			},
			"mask bit missing", 1,
		},
		{
			func() []byte {
				return testWSCreateClientMsg(wsPingMessage, 1, true, false, make([]byte, 200))
			},
			"control frame length bigger than maximum allowed", 1,
		},
		{
			func() []byte {
				return testWSCreateClientMsg(wsPingMessage, 1, false, false, []byte("hello"))
			},
			"control frame does not have final bit set", 1,
		},
		{
			func() []byte {
				frag1 := testWSCreateClientMsg(wsBinaryMessage, 1, false, false, []byte("frag1"))
				newMsg := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, []byte("new message"))
				all := append([]byte(nil), frag1...)
				all = append(all, newMsg...)
				return all
			},
			"new message started before final frame for previous message was received", 2,
		},
		{
			func() []byte {
				frag1 := testWSCreateClientMsg(wsBinaryMessage, 1, false, true, []byte("frag1"))
				frag2 := testWSCreateClientMsg(wsBinaryMessage, 2, false, true, []byte("frag2"))
				frag2[0] |= wsRsv1Bit
				all := append([]byte(nil), frag1...)
				all = append(all, frag2...)
				return all
			},
			"invalid continuation frame", 2,
		},
		{
			func() []byte {
				return testWSCreateClientMsg(99, 1, false, false, []byte("hello"))
			},
			"unknown opcode", 1,
		},
	} {
		t.Run(test.err, func(t *testing.T) {
			c, ri, tr := testWSSetupForRead()
			// Add a valid message first
			msg := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, []byte("hello"))
			// Then add the bad frame
			bad := test.cframe()
			// Add them both to a read buffer
			rb := append([]byte(nil), msg...)
			rb = append(rb, bad...)
			bufs, err := c.wsRead(ri, tr, rb)
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Fatalf("Expected error to contain %q, got %q", test.err, err.Error())
			}
			if n := len(bufs); n != test.nbufs {
				t.Fatalf("Unexpected number of buffers: %v", n)
			}
			if string(bufs[0]) != "hello" {
				t.Fatalf("Unexpected content: %s", bufs[0])
			}
		})
	}
}

func TestWSEnqueueCloseMsg(t *testing.T) {
	for _, test := range []struct {
		reason ClosedState
		status int
	}{
		{ClientClosed, wsCloseStatusNormalClosure},
		{AuthenticationTimeout, wsCloseStatusPolicyViolation},
		{AuthenticationViolation, wsCloseStatusPolicyViolation},
		{SlowConsumerPendingBytes, wsCloseStatusPolicyViolation},
		{SlowConsumerWriteDeadline, wsCloseStatusPolicyViolation},
		{MaxAccountConnectionsExceeded, wsCloseStatusPolicyViolation},
		{MaxConnectionsExceeded, wsCloseStatusPolicyViolation},
		{MaxControlLineExceeded, wsCloseStatusPolicyViolation},
		{MaxSubscriptionsExceeded, wsCloseStatusPolicyViolation},
		{MissingAccount, wsCloseStatusPolicyViolation},
		{AuthenticationExpired, wsCloseStatusPolicyViolation},
		{Revocation, wsCloseStatusPolicyViolation},
		{TLSHandshakeError, wsCloseStatusTLSHandshake},
		{ParseError, wsCloseStatusProtocolError},
		{ProtocolViolation, wsCloseStatusProtocolError},
		{BadClientProtocolVersion, wsCloseStatusProtocolError},
		{MaxPayloadExceeded, wsCloseStatusMessageTooBig},
		{ServerShutdown, wsCloseStatusGoingAway},
		{WriteError, wsCloseStatusAbnormalClosure},
		{ReadError, wsCloseStatusAbnormalClosure},
		{StaleConnection, wsCloseStatusAbnormalClosure},
		{ClosedState(254), wsCloseStatusInternalSrvError},
	} {
		t.Run(test.reason.String(), func(t *testing.T) {
			c, _, _ := testWSSetupForRead()
			c.wsEnqueueCloseMessage(test.reason)
			c.mu.Lock()
			nb := c.collapsePtoNB()
			c.mu.Unlock()
			if n := len(nb); n != 1 {
				t.Fatalf("Expected 1 buffer, got %v", n)
			}
			b := nb[0][0]
			if b&wsFinalBit == 0 {
				t.Fatalf("Control frame should have been the final flag, it was not set: %v", b)
			}
			if b&byte(wsCloseMessage) == 0 {
				t.Fatalf("Should have been a CLOSE, it wasn't: %v", b)
			}
			if status := binary.BigEndian.Uint16(nb[0][2:4]); int(status) != test.status {
				t.Fatalf("Expected status to be %v, got %v", test.status, status)
			}
			if string(nb[0][4:]) != test.reason.String() {
				t.Fatalf("Unexpected content: %s", nb[0][4:])
			}
		})
	}
}

type testResponseWriter struct {
	http.ResponseWriter
	buf     bytes.Buffer
	headers http.Header
	err     error
	brw     *bufio.ReadWriter
	conn    *testWSFakeNetConn
}

func (trw *testResponseWriter) Write(p []byte) (int, error) {
	return trw.buf.Write(p)
}

func (trw *testResponseWriter) WriteHeader(status int) {
	trw.buf.WriteString(fmt.Sprintf("%v", status))
}

func (trw *testResponseWriter) Header() http.Header {
	if trw.headers == nil {
		trw.headers = make(http.Header)
	}
	return trw.headers
}

type testWSFakeNetConn struct {
	net.Conn
	wbuf            bytes.Buffer
	err             error
	wsOpened        bool
	isClosed        bool
	deadlineCleared bool
}

func (c *testWSFakeNetConn) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	return c.wbuf.Write(p)
}

func (c *testWSFakeNetConn) SetDeadline(t time.Time) error {
	if t.IsZero() {
		c.deadlineCleared = true
	}
	return nil
}

func (c *testWSFakeNetConn) Close() error {
	c.isClosed = true
	return nil
}

func (trw *testResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if trw.conn == nil {
		trw.conn = &testWSFakeNetConn{}
	}
	trw.conn.wsOpened = true
	if trw.brw == nil {
		trw.brw = bufio.NewReadWriter(bufio.NewReader(trw.conn), bufio.NewWriter(trw.conn))
	}
	return trw.conn, trw.brw, trw.err
}

func testWSOptions() *Options {
	opts := DefaultOptions()
	opts.Websocket.Host = "127.0.0.1"
	opts.Websocket.Port = -1
	return opts
}

func testWSCreateValidReq() *http.Request {
	req := &http.Request{
		Method: "GET",
		Host:   "http://host.com",
		Proto:  "HTTP/1.1",
	}
	req.Header = make(http.Header)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-Websocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-Websocket-Version", "13")
	return req
}

func TestWSUpgradeValidationErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func() (*Options, *testResponseWriter, *http.Request)
		err    string
		status int
	}{
		{
			"bad method",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				req := testWSCreateValidReq()
				req.Method = "POST"
				return opts, nil, req
			},
			"must be GET",
			http.StatusMethodNotAllowed,
		},
		{
			"no host",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				req := testWSCreateValidReq()
				req.Host = ""
				return opts, nil, req
			},
			"'Host' missing in request",
			http.StatusBadRequest,
		},
		{
			"invalid upgrade header",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				req := testWSCreateValidReq()
				req.Header.Del("Upgrade")
				return opts, nil, req
			},
			"invalid value for header 'Uprade'",
			http.StatusBadRequest,
		},
		{
			"invalid connection header",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				req := testWSCreateValidReq()
				req.Header.Del("Connection")
				return opts, nil, req
			},
			"invalid value for header 'Connection'",
			http.StatusBadRequest,
		},
		{
			"no key",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				req := testWSCreateValidReq()
				req.Header.Del("Sec-Websocket-Key")
				return opts, nil, req
			},
			"key missing",
			http.StatusBadRequest,
		},
		{
			"empty key",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				req := testWSCreateValidReq()
				req.Header.Set("Sec-Websocket-Key", "")
				return opts, nil, req
			},
			"key missing",
			http.StatusBadRequest,
		},
		{
			"missing version",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				req := testWSCreateValidReq()
				req.Header.Del("Sec-Websocket-Version")
				return opts, nil, req
			},
			"invalid version",
			http.StatusBadRequest,
		},
		{
			"wrong version",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				req := testWSCreateValidReq()
				req.Header.Set("Sec-Websocket-Version", "99")
				return opts, nil, req
			},
			"invalid version",
			http.StatusBadRequest,
		},
		{
			"origin does not match request host",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				opts.Websocket.CheckOrigin = true
				req := testWSCreateValidReq()
				req.Header.Set("Origin", "http://bad.host.com")
				return opts, nil, req
			},
			"invalid request origin",
			http.StatusForbidden,
		},
		{
			"origin does not match option origin",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				opts.Websocket.CheckOrigin = true
				opts.Websocket.Origin = "http://other.host.com"
				req := testWSCreateValidReq()
				req.Header.Set("Origin", "http://host.com")
				return opts, nil, req
			},
			"invalid request origin",
			http.StatusForbidden,
		},
		{
			"origin bad url",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				opts.Websocket.CheckOrigin = true
				opts.Websocket.Origin = "http://other.host.com"
				req := testWSCreateValidReq()
				req.Header.Set("Origin", "http://this is a :: bad url")
				return opts, nil, req
			},
			"invalid request origin",
			http.StatusForbidden,
		},
		{
			"hijack error",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				rw := &testResponseWriter{err: fmt.Errorf("on purpose")}
				req := testWSCreateValidReq()
				return opts, rw, req
			},
			"on purpose",
			http.StatusInternalServerError,
		},
		{
			"hijack buffered data",
			func() (*Options, *testResponseWriter, *http.Request) {
				opts := testWSOptions()
				buf := &bytes.Buffer{}
				buf.WriteString("some data")
				rw := &testResponseWriter{
					conn: &testWSFakeNetConn{},
					brw:  bufio.NewReadWriter(bufio.NewReader(buf), bufio.NewWriter(nil)),
				}
				tmp := [1]byte{}
				io.ReadAtLeast(rw.brw, tmp[:1], 1)
				req := testWSCreateValidReq()
				return opts, rw, req
			},
			"client sent data before handshake is complete",
			http.StatusBadRequest,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts, rw, req := test.setup()
			if rw == nil {
				rw = &testResponseWriter{}
			}
			s := &Server{opts: opts}
			res, err := s.wsUpgrade(rw, req)
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Fatalf("Should get error %q, got %v", test.err, err)
			}
			if res != nil {
				t.Fatalf("Should not have returned a result, got %v", res)
			}
			expected := fmt.Sprintf("%v%s\n", test.status, http.StatusText(test.status))
			if got := rw.buf.String(); got != expected {
				t.Fatalf("Expected %q got %q", expected, got)
			}
			// Check that if the connection was opened, it is now closed.
			if rw.conn != nil && rw.conn.wsOpened && !rw.conn.isClosed {
				t.Fatal("Connection was opened, but has not been closed")
			}
		})
	}
}

func TestWSUpgradeResponseWriteError(t *testing.T) {
	opts := testWSOptions()
	s := &Server{opts: opts}
	expectedErr := errors.New("on purpose")
	rw := &testResponseWriter{
		conn: &testWSFakeNetConn{err: expectedErr},
	}
	req := testWSCreateValidReq()
	res, err := s.wsUpgrade(rw, req)
	if err != expectedErr {
		t.Fatalf("Should get error %q, got %v", expectedErr.Error(), err)
	}
	if res != nil {
		t.Fatalf("Should not have returned a result, got %v", res)
	}
	if !rw.conn.isClosed {
		t.Fatal("Connection should have been closed")
	}
}

func TestWSUpgradeConnDeadline(t *testing.T) {
	opts := testWSOptions()
	opts.Websocket.HandshakeTimeout = time.Second
	s := &Server{opts: opts}
	rw := &testResponseWriter{}
	req := testWSCreateValidReq()
	res, err := s.wsUpgrade(rw, req)
	if res == nil || err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if rw.conn.isClosed {
		t.Fatal("Connection should NOT have been closed")
	}
	if !rw.conn.deadlineCleared {
		t.Fatal("Connection deadline should have been cleared after handshake")
	}
}

func TestWSCompressNegotiation(t *testing.T) {
	// No compression on the server, but client asks
	opts := testWSOptions()
	s := &Server{opts: opts}
	rw := &testResponseWriter{}
	req := testWSCreateValidReq()
	req.Header.Set("Sec-Websocket-Extensions", "permessage-deflate")
	res, err := s.wsUpgrade(rw, req)
	if res == nil || err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// The http response should not contain "permessage-deflate"
	output := rw.conn.wbuf.String()
	if strings.Contains(output, "permessage-deflate") {
		t.Fatalf("Compression disabled in server so response to client should not contain extension, got %s", output)
	}

	// Option in the server and client, so compression should be negotiated.
	s.opts.Websocket.Compression = true
	s.opts.Websocket.CompressionLevel = defaultCompressionLevel
	rw = &testResponseWriter{}
	res, err = s.wsUpgrade(rw, req)
	if res == nil || err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// The http response should not contain "permessage-deflate"
	output = rw.conn.wbuf.String()
	if !strings.Contains(output, "permessage-deflate") {
		t.Fatalf("Compression in server and client request, so response should contain extension, got %s", output)
	}

	// Option in server but not asked by the client, so response should not contain "permessage-deflate"
	rw = &testResponseWriter{}
	req.Header.Del("Sec-Websocket-Extensions")
	res, err = s.wsUpgrade(rw, req)
	if res == nil || err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	// The http response should not contain "permessage-deflate"
	output = rw.conn.wbuf.String()
	if strings.Contains(output, "permessage-deflate") {
		t.Fatalf("Compression in server but not in client, so response to client should not contain extension, got %s", output)
	}
}

func TestWSCheckOriginButClientDoesNotSetIt(t *testing.T) {
	// Spec says that if origin is not set on the client, then server should not check/reject
	opts := testWSOptions()
	opts.Websocket.CheckOrigin = true
	s := &Server{opts: opts}
	rw := &testResponseWriter{}
	req := testWSCreateValidReq()
	res, err := s.wsUpgrade(rw, req)
	if res == nil || err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Add also expected origin, and again, this should not prevent client request to be accepted.
	opts.Websocket.Origin = "this.host.com"
	rw = &testResponseWriter{}
	req = testWSCreateValidReq()
	res, err = s.wsUpgrade(rw, req)
	if res == nil || err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
}

func TestWSValidateOptions(t *testing.T) {
	o := DefaultOptions()
	if err := validateWebsocketOptions(o); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	o.Websocket.Port = -1
	badLevels := []int{-10, 20}
	for _, bl := range badLevels {
		t.Run("bad compression level", func(t *testing.T) {
			o.Websocket.CompressionLevel = bl
			if err := validateWebsocketOptions(o); err == nil || !strings.Contains(err.Error(), "valid range") {
				t.Fatalf("Unexpected error: %v", err)
			}
		})
	}
}

type captureFatalLogger struct {
	DummyLogger
	fatalCh chan string
}

func (l *captureFatalLogger) Fatalf(format string, v ...interface{}) {
	select {
	case l.fatalCh <- fmt.Sprintf(format, v...):
	default:
	}
}

func TestWSFailureToStartServer(t *testing.T) {
	// Create a listener to use a port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Error listening: %v", err)
	}
	defer l.Close()

	o := testWSOptions()
	o.Websocket.Port = l.Addr().(*net.TCPAddr).Port
	s, err := NewServer(o)
	if err != nil {
		t.Fatalf("Error creating server: %v", err)
	}
	defer s.Shutdown()
	logger := &captureFatalLogger{fatalCh: make(chan string, 1)}
	s.SetLogger(logger, false, false)

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		s.Start()
		wg.Done()
	}()

	select {
	case e := <-logger.fatalCh:
		if !strings.Contains(e, "Unable to listen") {
			t.Fatalf("Unexpected error: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Should have reported a fatal error")
	}
	s.Shutdown()
	wg.Wait()
}

func TestWSAbnormalFailureOfWebServer(t *testing.T) {
	o := testWSOptions()
	s := RunServer(o)
	defer s.Shutdown()
	logger := &captureFatalLogger{fatalCh: make(chan string, 1)}
	s.SetLogger(logger, false, false)

	// Now close the WS listener to cause a WebServer error
	s.mu.Lock()
	s.websocket.listener.Close()
	s.mu.Unlock()

	select {
	case e := <-logger.fatalCh:
		if !strings.Contains(e, "websocket listener error") {
			t.Fatalf("Unexpected error: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Should have reported a fatal error")
	}
}

func testWSCreateClient(t testing.TB, compress bool, host string, port int) (net.Conn, *bufio.Reader) {
	addr := fmt.Sprintf("%s:%d", host, port)
	wsc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Error creating ws connection: %v", err)
	}
	req := testWSCreateValidReq()
	if compress {
		req.Header.Set("Sec-Websocket-Extensions", "permessage-deflate")
	}
	req.URL, _ = url.Parse("ws://" + addr)
	if err := req.Write(wsc); err != nil {
		t.Fatalf("Error sending request: %v", err)
	}
	br := bufio.NewReader(wsc)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("Error reading response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("Expected response status %v, got %v", http.StatusSwitchingProtocols, resp.StatusCode)
	}
	// Wait for the INFO
	if msg := testWSReadFrame(t, br); !bytes.HasPrefix(msg, []byte("INFO {")) {
		t.Fatalf("Expected INFO, got %s", msg)
	}
	// Send CONNECT
	wsmsg := testWSCreateClientMsg(wsBinaryMessage, 1, true, compress, []byte("CONNECT {\"verbose\":false}\r\n"))
	if _, err := wsc.Write(wsmsg); err != nil {
		t.Fatalf("Error sending message: %v", err)
	}
	return wsc, br
}

func testWSReadFrame(t testing.TB, br *bufio.Reader) []byte {
	fh := [2]byte{}
	if _, err := io.ReadAtLeast(br, fh[:2], 2); err != nil {
		t.Fatalf("Error reading frame: %v", err)
	}
	fc := fh[0]&wsRsv1Bit != 0
	sb := fh[1]
	size := 0
	switch {
	case sb <= 125:
		size = int(sb)
	case sb == 126:
		tmp := [2]byte{}
		if _, err := io.ReadAtLeast(br, tmp[:2], 2); err != nil {
			t.Fatalf("Error reading frame: %v", err)
		}
		size = int(binary.BigEndian.Uint16(tmp[:2]))
	case sb == 127:
		tmp := [8]byte{}
		if _, err := io.ReadAtLeast(br, tmp[:8], 8); err != nil {
			t.Fatalf("Error reading frame: %v", err)
		}
		size = int(binary.BigEndian.Uint64(tmp[:8]))
	}
	buf := make([]byte, size)
	if _, err := io.ReadAtLeast(br, buf, size); err != nil {
		t.Fatalf("Error reading frame: %v", err)
	}
	if !fc {
		return buf
	}
	buf = append(buf, 0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff)
	dbr := bytes.NewBuffer(buf)
	d, _ := decompressorPool.Get().(io.ReadCloser)
	if d == nil {
		d = flate.NewReader(dbr)
	} else {
		d.(flate.Resetter).Reset(dbr, nil)
	}
	uncompressed, err := ioutil.ReadAll(d)
	if err != nil {
		t.Fatalf("Error reading frame: %v", err)
	}
	decompressorPool.Put(d)
	return uncompressed
}

func TestWSPubSub(t *testing.T) {
	for _, test := range []struct {
		name        string
		compression bool
	}{
		{"no compression", false},
		{"compression", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := testWSOptions()
			if test.compression {
				o.Websocket.Compression = true
				o.Websocket.CompressionLevel = defaultCompressionLevel
			}
			s := RunServer(o)
			defer s.Shutdown()

			// Create a regular client to subscribe
			nc := natsConnect(t, s.ClientURL())
			defer nc.Close()
			nsub := natsSubSync(t, nc, "foo")
			checkExpectedSubs(t, 1, s)

			// Now create a WS client and send a message on "foo"
			wsc, br := testWSCreateClient(t, test.compression, o.Websocket.Host, o.Websocket.Port)
			defer wsc.Close()

			// Send a WS message for "PUB foo 2\r\nok\r\n"
			wsmsg := testWSCreateClientMsg(wsBinaryMessage, 1, true, false, []byte("PUB foo 7\r\nfrom ws\r\n"))
			if _, err := wsc.Write(wsmsg); err != nil {
				t.Fatalf("Error sending message: %v", err)
			}

			// Now check that message is received
			msg := natsNexMsg(t, nsub, time.Second)
			if string(msg.Data) != "from ws" {
				t.Fatalf("Expected message to be %q, got %q", "ok", string(msg.Data))
			}

			// Now do reverse, create a subscription on WS client on bar
			wsmsg = testWSCreateClientMsg(wsBinaryMessage, 1, true, false, []byte("SUB bar 1\r\n"))
			if _, err := wsc.Write(wsmsg); err != nil {
				t.Fatalf("Error sending subscription: %v", err)
			}
			// Wait for it to be registered on server
			checkExpectedSubs(t, 2, s)
			// Now publish from NATS connection and verify received on WS client
			natsPub(t, nc, "bar", []byte("from nats"))
			natsFlush(t, nc)

			// Check for the "from nats" message...
			// Set some deadline so we are not stuck forever on failure
			wsc.SetReadDeadline(time.Now().Add(10 * time.Second))
			ok := 0
			for {
				line, _, err := br.ReadLine()
				if err != nil {
					t.Fatalf("Error reading: %v", err)
				}
				// Note that this works even in compression test because those
				// texts are likely not to be compressed, but compression code is
				// still executed.
				if ok == 0 && bytes.Contains(line, []byte("MSG bar 1 9")) {
					ok = 1
					continue
				} else if ok == 1 && bytes.Contains(line, []byte("from nats")) {
					ok = 2
					break
				}
			}
		})
	}
}

func TestWSTLSConnection(t *testing.T) {
	o := testWSOptions()
	tc := &TLSConfigOpts{
		CertFile: "./configs/certs/server.pem",
		KeyFile:  "./configs/certs/key.pem",
	}
	o.Websocket.TLSConfig, _ = GenTLSConfig(tc)
	s := RunServer(o)
	defer s.Shutdown()

	addr := fmt.Sprintf("%s:%d", o.Websocket.Host, o.Websocket.Port)
	req := testWSCreateValidReq()
	req.URL, _ = url.Parse("wss://" + addr)

	for _, test := range []struct {
		name   string
		useTLS bool
		status int
	}{
		{"client does use TLS", false, http.StatusBadRequest},
		{"client uses TLS", true, http.StatusSwitchingProtocols},
	} {
		t.Run(test.name, func(t *testing.T) {
			wsc, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("Error creating ws connection: %v", err)
			}
			defer wsc.Close()
			if test.useTLS {
				wsc = tls.Client(wsc, &tls.Config{InsecureSkipVerify: true})
				if err := wsc.(*tls.Conn).Handshake(); err != nil {
					t.Fatalf("Error during handshake: %v", err)
				}
			}
			if err := req.Write(wsc); err != nil {
				t.Fatalf("Error sending request: %v", err)
			}
			br := bufio.NewReader(wsc)
			resp, err := http.ReadResponse(br, req)
			if err != nil {
				t.Fatalf("Error reading response: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != test.status {
				t.Fatalf("Expected status %v, got %v", test.status, resp.StatusCode)
			}
		})
	}
}

func TestWSHandshakeTimeout(t *testing.T) {
	o := testWSOptions()
	o.Websocket.HandshakeTimeout = time.Millisecond
	tc := &TLSConfigOpts{
		CertFile: "./configs/certs/server.pem",
		KeyFile:  "./configs/certs/key.pem",
	}
	o.Websocket.TLSConfig, _ = GenTLSConfig(tc)
	s := RunServer(o)
	defer s.Shutdown()

	logger := &captureErrorLogger{errCh: make(chan string, 1)}
	s.SetLogger(logger, false, false)

	addr := fmt.Sprintf("%s:%d", o.Websocket.Host, o.Websocket.Port)
	wsc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Error creating ws connection: %v", err)
	}
	defer wsc.Close()

	// Delay the handshake
	wsc = tls.Client(wsc, &tls.Config{InsecureSkipVerify: true})
	time.Sleep(20 * time.Millisecond)
	// We expect error since the server should have cut us off
	if err := wsc.(*tls.Conn).Handshake(); err == nil {
		t.Fatal("Expected error during handshake")
	}

	// Check that server logs error
	select {
	case e := <-logger.errCh:
		if !strings.Contains(e, "timeout") {
			t.Fatalf("Unexpected error: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatalf("Should have timed-out")
	}
}

func TestWSServerReportUpgradeFailure(t *testing.T) {
	o := testWSOptions()
	s := RunServer(o)
	defer s.Shutdown()

	logger := &captureErrorLogger{errCh: make(chan string, 1)}
	s.SetLogger(logger, false, false)

	addr := fmt.Sprintf("%s:%d", o.Websocket.Host, o.Websocket.Port)
	req := testWSCreateValidReq()
	req.URL, _ = url.Parse("wss://" + addr)

	wsc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Error creating ws connection: %v", err)
	}
	defer wsc.Close()
	// Remove a required field from the request to have it fail
	req.Header.Del("Connection")
	// Send the request
	if err := req.Write(wsc); err != nil {
		t.Fatalf("Error sending request: %v", err)
	}
	br := bufio.NewReader(wsc)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("Error reading response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected status %v, got %v", http.StatusBadRequest, resp.StatusCode)
	}

	// Check that server logs error
	select {
	case e := <-logger.errCh:
		if !strings.Contains(e, "invalid value for header 'Connection'") {
			t.Fatalf("Unexpected error: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatalf("Should have timed-out")
	}
}

// ==================================================================
// = Benchmark tests
// ==================================================================

const testWSBenchSubject = "a"

var ch = []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@$#%^&*()")

func sizedString(sz int) string {
	b := make([]byte, sz)
	for i := range b {
		b[i] = ch[rand.Intn(len(ch))]
	}
	return string(b)
}

func sizedStringForCompression(sz int) string {
	b := make([]byte, sz)
	c := byte(0)
	s := 0
	for i := range b {
		if s%20 == 0 {
			c = ch[rand.Intn(len(ch))]
		}
		b[i] = c
	}
	return string(b)
}

func testWSFlushConn(b *testing.B, compress bool, c net.Conn, br *bufio.Reader) {
	buf := testWSCreateClientMsg(wsBinaryMessage, 1, true, compress, []byte(pingProto))
	c.Write(buf)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	res := testWSReadFrame(b, br)
	c.SetReadDeadline(time.Time{})
	if !bytes.HasPrefix(res, []byte(pongProto)) {
		b.Fatalf("Failed read of PONG: %s\n", res)
	}
}

func wsBenchPub(b *testing.B, numPubs int, compress bool, payload string) {
	b.StopTimer()
	opts := testWSOptions()
	opts.DisableShortFirstPing = true
	opts.Websocket.Host = "127.0.0.1"
	opts.Websocket.Port = -1
	opts.Websocket.Compression = compress
	opts.Websocket.CompressionLevel = defaultCompressionLevel
	s := RunServer(opts)
	defer s.Shutdown()

	n := b.N
	extra := 0
	pubProto := []byte(fmt.Sprintf("PUB %s %d\r\n%s\r\n", testWSBenchSubject, len(payload), payload))
	singleOpBuf := testWSCreateClientMsg(wsBinaryMessage, 1, true, compress, pubProto)

	// Simulate client that would buffer messages before framing/sending.
	// Figure out how many we can fit in one frame based on b.N and length of pubProto
	const bufSize = 32768
	tmpa := [bufSize]byte{}
	tmp := tmpa[:0]
	pb := 0
	for i := 0; i < b.N; i++ {
		tmp = append(tmp, pubProto...)
		pb++
		if len(tmp) >= bufSize {
			break
		}
	}
	sendBuf := testWSCreateClientMsg(wsBinaryMessage, 1, true, compress, tmp)
	n = b.N / pb
	extra = b.N - (n * pb)

	wg := sync.WaitGroup{}
	wg.Add(numPubs)

	type pub struct {
		c  net.Conn
		br *bufio.Reader
		bw *bufio.Writer
	}
	var pubs []pub
	for i := 0; i < numPubs; i++ {
		wsc, br := testWSCreateClient(b, compress, opts.Websocket.Host, opts.Websocket.Port)
		defer wsc.Close()
		bw := bufio.NewWriterSize(wsc, bufSize)
		pubs = append(pubs, pub{wsc, br, bw})
	}

	// Average the amount of bytes sent by iteration
	avg := len(sendBuf) / pb
	if extra > 0 {
		avg += len(singleOpBuf)
		avg /= 2
	}
	b.SetBytes(int64(numPubs * avg))
	b.StartTimer()

	for i := 0; i < numPubs; i++ {
		p := pubs[i]
		go func(p pub) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				p.bw.Write(sendBuf)
			}
			for i := 0; i < extra; i++ {
				p.bw.Write(singleOpBuf)
			}
			p.bw.Flush()
			testWSFlushConn(b, compress, p.c, p.br)
		}(p)
	}
	wg.Wait()
	b.StopTimer()
}

func Benchmark_WS_Pubx1_CN_____0b(b *testing.B) {
	wsBenchPub(b, 1, false, "")
}

func Benchmark_WS_Pubx1_CY_____0b(b *testing.B) {
	wsBenchPub(b, 1, true, "")
}

func Benchmark_WS_Pubx1_CN___128b(b *testing.B) {
	s := sizedString(128)
	wsBenchPub(b, 1, false, s)
}

func Benchmark_WS_Pubx1_CY___128b(b *testing.B) {
	s := sizedStringForCompression(128)
	wsBenchPub(b, 1, true, s)
}

func Benchmark_WS_Pubx1_CN__1024b(b *testing.B) {
	s := sizedString(1024)
	wsBenchPub(b, 1, false, s)
}

func Benchmark_WS_Pubx1_CY__1024b(b *testing.B) {
	s := sizedStringForCompression(1024)
	wsBenchPub(b, 1, true, s)
}

func Benchmark_WS_Pubx1_CN__4096b(b *testing.B) {
	s := sizedString(4 * 1024)
	wsBenchPub(b, 1, false, s)
}

func Benchmark_WS_Pubx1_CY__4096b(b *testing.B) {
	s := sizedStringForCompression(4 * 1024)
	wsBenchPub(b, 1, true, s)
}

func Benchmark_WS_Pubx1_CN__8192b(b *testing.B) {
	s := sizedString(8 * 1024)
	wsBenchPub(b, 1, false, s)
}

func Benchmark_WS_Pubx1_CY__8192b(b *testing.B) {
	s := sizedStringForCompression(8 * 1024)
	wsBenchPub(b, 1, true, s)
}

func Benchmark_WS_Pubx1_CN_32768b(b *testing.B) {
	s := sizedString(32 * 1024)
	wsBenchPub(b, 1, false, s)
}

func Benchmark_WS_Pubx1_CY_32768b(b *testing.B) {
	s := sizedStringForCompression(32 * 1024)
	wsBenchPub(b, 1, true, s)
}

func Benchmark_WS_Pubx5_CN_____0b(b *testing.B) {
	wsBenchPub(b, 5, false, "")
}

func Benchmark_WS_Pubx5_CY_____0b(b *testing.B) {
	wsBenchPub(b, 5, true, "")
}

func Benchmark_WS_Pubx5_CN___128b(b *testing.B) {
	s := sizedString(128)
	wsBenchPub(b, 5, false, s)
}

func Benchmark_WS_Pubx5_CY___128b(b *testing.B) {
	s := sizedStringForCompression(128)
	wsBenchPub(b, 5, true, s)
}

func Benchmark_WS_Pubx5_CN__1024b(b *testing.B) {
	s := sizedString(1024)
	wsBenchPub(b, 5, false, s)
}

func Benchmark_WS_Pubx5_CY__1024b(b *testing.B) {
	s := sizedStringForCompression(1024)
	wsBenchPub(b, 5, true, s)
}

func Benchmark_WS_Pubx5_CN__4096b(b *testing.B) {
	s := sizedString(4 * 1024)
	wsBenchPub(b, 5, false, s)
}

func Benchmark_WS_Pubx5_CY__4096b(b *testing.B) {
	s := sizedStringForCompression(4 * 1024)
	wsBenchPub(b, 5, true, s)
}

func Benchmark_WS_Pubx5_CN__8192b(b *testing.B) {
	s := sizedString(8 * 1024)
	wsBenchPub(b, 5, false, s)
}

func Benchmark_WS_Pubx5_CY__8192b(b *testing.B) {
	s := sizedStringForCompression(8 * 1024)
	wsBenchPub(b, 5, true, s)
}

func Benchmark_WS_Pubx5_CN_32768b(b *testing.B) {
	s := sizedString(32 * 1024)
	wsBenchPub(b, 5, false, s)
}

func Benchmark_WS_Pubx5_CY_32768b(b *testing.B) {
	s := sizedStringForCompression(32 * 1024)
	wsBenchPub(b, 5, true, s)
}

func wsBenchSub(b *testing.B, numSubs int, compress bool, payload string) {
	b.StopTimer()
	opts := testWSOptions()
	opts.DisableShortFirstPing = true
	opts.Websocket.Host = "127.0.0.1"
	opts.Websocket.Port = -1
	opts.Websocket.Compression = compress
	opts.Websocket.CompressionLevel = defaultCompressionLevel
	s := RunServer(opts)
	defer s.Shutdown()

	var subs []*bufio.Reader
	for i := 0; i < numSubs; i++ {
		wsc, br := testWSCreateClient(b, compress, opts.Websocket.Host, opts.Websocket.Port)
		defer wsc.Close()
		subProto := testWSCreateClientMsg(wsBinaryMessage, 1, true, compress,
			[]byte(fmt.Sprintf("SUB %s 1\r\nPING\r\n", testWSBenchSubject)))
		wsc.Write(subProto)
		// Waiting for PONG
		testWSReadFrame(b, br)
		subs = append(subs, br)
	}

	wg := sync.WaitGroup{}
	wg.Add(numSubs)

	// Use regular NATS client to publish messages
	nc := natsConnect(b, s.ClientURL())
	defer nc.Close()

	b.StartTimer()

	for i := 0; i < numSubs; i++ {
		br := subs[i]
		go func(br *bufio.Reader) {
			defer wg.Done()
			for count := 0; count < b.N; {
				msgs := testWSReadFrame(b, br)
				count += bytes.Count(msgs, []byte("MSG "))
			}
		}(br)
	}
	for i := 0; i < b.N; i++ {
		natsPub(b, nc, testWSBenchSubject, []byte(payload))
	}
	wg.Wait()
	b.StopTimer()
}

func Benchmark_WS_Subx1_CN_____0b(b *testing.B) {
	wsBenchSub(b, 1, false, "")
}

func Benchmark_WS_Subx1_CY_____0b(b *testing.B) {
	wsBenchSub(b, 1, true, "")
}

func Benchmark_WS_Subx1_CN___128b(b *testing.B) {
	s := sizedString(128)
	wsBenchSub(b, 1, false, s)
}

func Benchmark_WS_Subx1_CY___128b(b *testing.B) {
	s := sizedStringForCompression(128)
	wsBenchSub(b, 1, true, s)
}

func Benchmark_WS_Subx1_CN__1024b(b *testing.B) {
	s := sizedString(1024)
	wsBenchSub(b, 1, false, s)
}

func Benchmark_WS_Subx1_CY__1024b(b *testing.B) {
	s := sizedStringForCompression(1024)
	wsBenchSub(b, 1, true, s)
}

func Benchmark_WS_Subx1_CN__4096b(b *testing.B) {
	s := sizedString(4096)
	wsBenchSub(b, 1, false, s)
}

func Benchmark_WS_Subx1_CY__4096b(b *testing.B) {
	s := sizedStringForCompression(4096)
	wsBenchSub(b, 1, true, s)
}

func Benchmark_WS_Subx1_CN__8192b(b *testing.B) {
	s := sizedString(8192)
	wsBenchSub(b, 1, false, s)
}

func Benchmark_WS_Subx1_CY__8192b(b *testing.B) {
	s := sizedStringForCompression(8192)
	wsBenchSub(b, 1, true, s)
}

func Benchmark_WS_Subx1_CN_32768b(b *testing.B) {
	s := sizedString(32768)
	wsBenchSub(b, 1, false, s)
}

func Benchmark_WS_Subx1_CY_32768b(b *testing.B) {
	s := sizedStringForCompression(32768)
	wsBenchSub(b, 1, true, s)
}

func Benchmark_WS_Subx5_CN_____0b(b *testing.B) {
	wsBenchSub(b, 5, false, "")
}

func Benchmark_WS_Subx5_CY_____0b(b *testing.B) {
	wsBenchSub(b, 5, true, "")
}

func Benchmark_WS_Subx5_CN___128b(b *testing.B) {
	s := sizedString(128)
	wsBenchSub(b, 5, false, s)
}

func Benchmark_WS_Subx5_CY___128b(b *testing.B) {
	s := sizedStringForCompression(128)
	wsBenchSub(b, 5, true, s)
}

func Benchmark_WS_Subx5_CN__1024b(b *testing.B) {
	s := sizedString(1024)
	wsBenchSub(b, 5, false, s)
}

func Benchmark_WS_Subx5_CY__1024b(b *testing.B) {
	s := sizedStringForCompression(1024)
	wsBenchSub(b, 5, true, s)
}

func Benchmark_WS_Subx5_CN__4096b(b *testing.B) {
	s := sizedString(4096)
	wsBenchSub(b, 5, false, s)
}

func Benchmark_WS_Subx5_CY__4096b(b *testing.B) {
	s := sizedStringForCompression(4096)
	wsBenchSub(b, 5, true, s)
}

func Benchmark_WS_Subx5_CN__8192b(b *testing.B) {
	s := sizedString(8192)
	wsBenchSub(b, 5, false, s)
}

func Benchmark_WS_Subx5_CY__8192b(b *testing.B) {
	s := sizedStringForCompression(8192)
	wsBenchSub(b, 5, true, s)
}

func Benchmark_WS_Subx5_CN_32768b(b *testing.B) {
	s := sizedString(32768)
	wsBenchSub(b, 5, false, s)
}

func Benchmark_WS_Subx5_CY_32768b(b *testing.B) {
	s := sizedStringForCompression(32768)
	wsBenchSub(b, 5, true, s)
}
