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
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
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
	left := len(tr.buf) - tr.pos
	if left == 0 {
		return 0, nil
	}
	if left > cap(p) {
		left = cap(p)
	}
	if tr.max > 0 && left > tr.max {
		left = tr.max
	}
	copy(p, tr.buf[tr.pos:tr.pos+left])
	tr.pos += left
	return left, nil
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
		{"close", wsCloseMessage, false},
		{"close compressed", wsCloseMessage, true},
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

func testWSCreateClientMsg(frameType wsOpCode, final, compressed bool, payload []byte) []byte {
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
	frame[0] = byte(frameType)
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

func TestWSRead(t *testing.T) {
	reset := func() *wsReadInfo {
		ri := &wsReadInfo{}
		ri.init()
		return ri
	}
	ri := reset()

	tr := &testReader{}

	s := &Server{opts: DefaultOptions()}
	c := &client{srv: s, flags: wsClient}
	c.initClient()

	// Create 2 WS messages
	pl1 := []byte("first message")
	wsmsg1 := testWSCreateClientMsg(wsBinaryMessage, true, false, pl1)
	pl2 := []byte("second message")
	wsmsg2 := testWSCreateClientMsg(wsBinaryMessage, true, false, pl2)
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
	ri = reset()
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

	// Reset again and now try with compressed data.
	ri = reset()
	uncompressed := []byte("this is the uncompress data")
	wsmsg1 = testWSCreateClientMsg(wsBinaryMessage, true, true, uncompressed)
	rb = append([]byte(nil), wsmsg1...)
	// Call with some but not all of the payload
	bufs, err = c.wsRead(ri, tr, rb[:10])
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

	// Corrupt the compressed data now
	ri = reset()
	copy(wsmsg1[10:], []byte{1, 2, 3, 4})
	rb = append([]byte(nil), wsmsg1...)
	bufs, err = c.wsRead(ri, tr, rb)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Expected error about corrupted data, got %v", err)
	}
	if n := len(bufs); n != 0 {
		t.Fatalf("Expected no buffer, got %v", n)
	}
}
