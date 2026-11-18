// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package codecs

import (
	"testing"

	"github.com/pion/rtp/codecs/av1/obu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Create an AV1 OBU for testing. Returns one without the obu_size_field and another with it included.
func createAV1OBU(obuType obu.Type, payload []byte) ([]byte, []byte) {
	header := obu.Header{Type: obuType}
	withoutSize := createTestPayload(header, payload)
	header.HasSizeField = true
	withSize := createTestPayload(header, payload)

	return withoutSize, withSize
}

func createTestPayload(obuHeader obu.Header, payload []byte) []byte {
	buf := make([]byte, 0)
	buf = append(buf, obuHeader.Marshal()...)
	if obuHeader.HasSizeField {
		buf = append(buf, obu.WriteToLeb128(uint(len(payload)))...)
	}

	buf = append(buf, payload...)

	return buf
}

func TestAV1Depacketizer_invalidPackets(t *testing.T) {
	depacketizer := AV1Depacketizer{}
	_, err := depacketizer.Unmarshal([]byte{})
	assert.ErrorIs(t, err, errShortPacket)

	_, err = depacketizer.Unmarshal([]byte{0b11000000, 0xFF})
	assert.ErrorIs(t, err, obu.ErrFailedToReadLEB128)

	_, err = depacketizer.Unmarshal(append([]byte{0b00000000}, obu.WriteToLeb128(0x99)...))
	assert.ErrorIs(t, err, errShortPacket)

	_, err = depacketizer.Unmarshal(append([]byte{0b00000000}, obu.WriteToLeb128(0x01)...))
	assert.ErrorIs(t, err, errShortPacket)

	_, err = depacketizer.Unmarshal(
		append(
			[]byte{0b00110000},
			append(
				obu.WriteToLeb128(1),
				[]byte{0x01}...,
			)...,
		),
	)
	assert.ErrorIs(t, err, errShortPacket)
}

func TestAV1Depacketizer_singleOBU(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03}
	obuData, expectedOBU := createAV1OBU(4, payload)

	packet := make([]byte, 0)

	packet = append(packet, []byte{0b00000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obuData)))...)
	packet = append(packet, obuData...)

	d := AV1Depacketizer{}
	obu, err := d.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, expectedOBU, obu)
}

func TestAV1Depacketizer_singleOBUWithPadding(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03}
	obuData, expectedOBU := createAV1OBU(4, payload)

	packet := make([]byte, 0)

	packet = append(packet, []byte{0b00000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obuData)))...)
	packet = append(packet, obuData...)
	// padding
	packet = append(packet, []byte{0x00, 0x00, 0x00}...)

	d := AV1Depacketizer{}
	obu, err := d.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, expectedOBU, obu)
}

// AV1 OBUs shouldn't include the obu_size_field when packetized in RTP,
// but we still support it since it's encountered in the wild (Including pion old clients).
func TestAV1Depacketizer_withOBUSize(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03}
	_, obuData := createAV1OBU(4, payload)

	packet := make([]byte, 0)

	packet = append(packet, []byte{0b00000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obuData)))...)
	packet = append(packet, obuData...)

	d := AV1Depacketizer{}
	obu, err := d.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, obuData, obu)
}

func TestAV1Depacketizer_validateOBUSize(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		expect  []byte
		err     error
	}{
		{
			name: "invalid OBU size",
			payload: []byte{
				0,    // Aggregation header
				0x02, // Length field
				0x22, // OBU header (has_size_field = 1)
				0xFF, // Invalid LEB128 size
			},
			err: obu.ErrFailedToReadLEB128,
		},
		{
			name: "OBU size larger than payload",
			payload: []byte{
				0,                // Aggregation header
				0x05,             // Length field
				0x22,             // OBU header (has_size_field = 1)
				0x04,             // LEB128 size
				0x03, 0x01, 0x02, // OBU data
			},
			expect: []byte{
				0x22,             // OBU header
				0x03,             // Corrected size
				0x03, 0x01, 0x02, // OBU data
			},
		},
		{
			name: "OBU size smaller than length field",
			payload: []byte{
				0,                // Aggregation header
				0x05,             // Length field
				0x22,             // OBU header (has_size_field = 1)
				0x02,             // LEB128 size
				0x03, 0x01, 0x02, // OBU data
			},
			expect: []byte{
				0x22,             // OBU header
				0x03,             // Corrected size
				0x03, 0x01, 0x02, // OBU data
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := AV1Depacketizer{}
			obu, err := d.Unmarshal(tt.payload)
			if tt.err != nil {
				assert.ErrorIs(t, err, tt.err)

				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expect, obu)
		})
	}
}

func TestAV1Depacketizer_dropBuffer(t *testing.T) {
	depacketizer := &AV1Depacketizer{}
	empty, err := depacketizer.Unmarshal([]byte{0x41, 0x02, 0x00, 0x01})
	assert.NoError(t, err)
	assert.Len(t, empty, 0)

	payload := []byte{0x08, 0x02, 0x03}
	obuData, expectedOBU := createAV1OBU(4, payload)

	packet := make([]byte, 0)

	// N=true, should clear buffer
	packet = append(packet, []byte{0b00001000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obuData)))...)
	packet = append(packet, obuData...)

	obu, err := depacketizer.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, expectedOBU, obu)
}

func TestAV1Depacketizer_singleOBUWithW(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03}
	obuData, expectedOBU := createAV1OBU(4, payload)

	packet := append([]byte{0b00010000}, obuData...)

	d := AV1Depacketizer{}
	obu, err := d.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, expectedOBU, obu)
}

func TestDepacketizer_multipleFullOBUs(t *testing.T) {
	obu1, expectedOBU1 := createAV1OBU(4, []byte{0x01, 0x02, 0x03})
	obu2, expectedOBU2 := createAV1OBU(4, []byte{0x04, 0x05, 0x06})
	obu3, expectedOBU3 := createAV1OBU(4, []byte{0x07, 0x08, 0x09})
	expected := append(append(expectedOBU1, expectedOBU2...), expectedOBU3...)

	packet := make([]byte, 0)

	packet = append(packet, []byte{0b00000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu1)))...)
	packet = append(packet, obu1...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu2)))...)
	packet = append(packet, obu2...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu3)))...)
	packet = append(packet, obu3...)

	d := AV1Depacketizer{}
	obus, err := d.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, expected, obus)
}

func TestAV1Depacketizer_multipleFullOBUsWithW(t *testing.T) {
	obu1, expectedOBU1 := createAV1OBU(4, []byte{0x01, 0x02, 0x03})
	obu2, expectedOBU2 := createAV1OBU(4, []byte{0x04, 0x05, 0x06})
	obu3, expectedOBU3 := createAV1OBU(4, []byte{0x07, 0x08, 0x09})
	expected := append(append(expectedOBU1, expectedOBU2...), expectedOBU3...)

	packet := make([]byte, 0)

	packet = append(packet, []byte{0b00110000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu1)))...)
	packet = append(packet, obu1...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu2)))...)
	packet = append(packet, obu2...)
	// Last MUST NOT be preceded by a length field if W is not 0
	packet = append(packet, obu3...)

	depacketizer := AV1Depacketizer{}
	obus, err := depacketizer.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, expected, obus)
}

func TestDepacketizer_fragmentedOBUS(t *testing.T) {
	// Not up to spec AV1 stream but it should be depacketized.
	// [ SH MD ] Frag(MD(0,0)) [ FH(0,0) TG(0,0) ] Frag(MD(0,1)) [ FH(0,1) ] [ TG(0,1) ]
	obu1, expectedOBU1 := createAV1OBU(1, []byte{0x01, 0x02, 0x03})
	obu2, expectedOBU2 := createAV1OBU(7, []byte{0x04, 0x05, 0x06})
	obu3, expectedOBU3 := createAV1OBU(7, []byte{0x07, 0x08, 0x09})
	obu3f1 := obu3[:2]
	obu3f2 := obu3[2:]
	obu4, expectedOBU4 := createAV1OBU(3, []byte{0x0A, 0x0B, 0x0C})
	obu5, expectedOBU5 := createAV1OBU(6, []byte{0x0D, 0x0E, 0x0F})
	obu6, expectedOBU6 := createAV1OBU(7, []byte{0x10, 0x11, 0x12})
	obu6f1 := obu6[:2]
	obu6f2 := obu6[2:]
	obu7, expectedOBU7 := createAV1OBU(3, []byte{0x13, 0x14, 0x15})
	obu8, expectedOBU8 := createAV1OBU(6, []byte{0x16, 0x17, 0x18})

	depacketizer := AV1Depacketizer{}

	packet := make([]byte, 0)
	packet = append(packet, []byte{0b01000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu1)))...)
	packet = append(packet, obu1...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu2)))...)
	packet = append(packet, obu2...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu3f1)))...)
	packet = append(packet, obu3f1...)

	obus, err := depacketizer.Unmarshal(packet)
	assert.NoError(t, err)

	expected := make([]byte, 0)
	expected = append(expected, expectedOBU1...)
	expected = append(expected, expectedOBU2...)
	assert.Equal(t, expected, obus)

	packet = make([]byte, 0)
	packet = append(packet, []byte{0b11000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu3f2)))...)
	packet = append(packet, obu3f2...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu4)))...)
	packet = append(packet, obu4...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu5)))...)
	packet = append(packet, obu5...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu6f1)))...)
	packet = append(packet, obu6f1...)

	obus, err = depacketizer.Unmarshal(packet)
	assert.NoError(t, err)

	expected = append(append(expectedOBU3, expectedOBU4...), expectedOBU5...)
	assert.Equal(t, expected, obus)

	packet = make([]byte, 0)
	packet = append(packet, []byte{0b10100000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu6f2)))...)
	packet = append(packet, obu6f2...)
	// W is defined as 2, so the last OBU MUST NOT have a length field
	packet = append(packet, obu7...)

	obus, err = depacketizer.Unmarshal(packet)
	assert.NoError(t, err)

	expected = make([]byte, 0)
	expected = append(expected, expectedOBU6...)
	expected = append(expected, expectedOBU7...)
	assert.Equal(t, expected, obus)

	packet = make([]byte, 0)
	packet = append(packet, []byte{0b00000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obu8)))...)
	packet = append(packet, obu8...)

	obus, err = depacketizer.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, expectedOBU8, obus)
}

func TestAV1Depacketizer_dropLostFragment(t *testing.T) {
	depacketizer := AV1Depacketizer{}

	obus, err := depacketizer.Unmarshal(
		append(
			append([]byte{0b01000000}, obu.WriteToLeb128(3)...),
			[]byte{0x01, 0x02, 0x03}...,
		),
	)
	assert.NoError(t, err)
	assert.Len(t, obus, 0, "Expected empty OBU for fragmented OBU")

	newOBU, expected := createAV1OBU(obu.OBUTileGroup, []byte{0x04, 0x05, 0x06})
	obus, err = depacketizer.Unmarshal(
		append(
			append([]byte{0b00000000}, obu.WriteToLeb128(uint(len(newOBU)))...),
			newOBU...,
		),
	)
	assert.NoError(t, err)
	assert.Equal(t, expected, obus)
}

func TestAV1Depacketizer_dropIfLostFragment(t *testing.T) {
	depacketizer := AV1Depacketizer{}

	obus, err := depacketizer.Unmarshal(
		append(
			append([]byte{0b10000000}, obu.WriteToLeb128(3)...),
			[]byte{0x01, 0x02, 0x03}...,
		),
	)
	assert.NoError(t, err)
	assert.Len(t, obus, 0, "Expected empty OBU for fragmented OBU")

	newOBU, expected := createAV1OBU(obu.OBUTileGroup, []byte{0x04, 0x05, 0x06})
	obus, err = depacketizer.Unmarshal(
		append(
			append([]byte{0b00000000}, obu.WriteToLeb128(uint(len(newOBU)))...),
			newOBU...,
		),
	)
	assert.NoError(t, err)
	assert.Equal(t, expected, obus)

	packet := make([]byte, 0)
	packet = append(packet, []byte{0b10000000}...)
	packet = append(packet, obu.WriteToLeb128(3)...)
	packet = append(packet, []byte{0x01, 0x02, 0x03}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(newOBU)))...)
	packet = append(packet, newOBU...)

	obus, err = depacketizer.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, expected, obus)
}

func TestAV1Depacketizer_IsPartitionTail(t *testing.T) {
	depacketizer := &AV1Depacketizer{
		buffer: []byte{1, 2},
	}

	assert.False(t, depacketizer.IsPartitionTail(false, []byte{1, 2}))
	assert.Equal(t, depacketizer.buffer, []byte{1, 2})
	assert.True(t, depacketizer.IsPartitionTail(true, []byte{1, 2}))
}

func TestAV1Depacketizer_IsPartitionHead(t *testing.T) {
	depacketizer := &AV1Depacketizer{}

	assert.False(t, depacketizer.IsPartitionHead(nil))
	assert.False(t, depacketizer.IsPartitionHead([]byte{}))
	assert.False(t, depacketizer.IsPartitionHead([]byte{0b11000000}))
	assert.True(t, depacketizer.IsPartitionHead([]byte{0b00000000}))
}

func TestAV1Depacketizer_ignoreBadOBUs(t *testing.T) {
	shouldIgnore := []obu.Type{
		obu.OBUTemporalDelimiter,
		obu.OBUTileList,
	}

	for _, obuType := range shouldIgnore {
		payload := []byte{0x01, 0x02, 0x03}
		obuData, _ := createAV1OBU(obuType, payload)

		packet := make([]byte, 0)
		packet = append(packet, []byte{0b00000000}...)
		packet = append(packet, obu.WriteToLeb128(uint(len(obuData)))...)
		packet = append(packet, obuData...)

		depacketizer := AV1Depacketizer{}
		obu, err := depacketizer.Unmarshal(packet)
		assert.NoError(t, err)
		assert.Len(t, obu, 0, "Expected empty payload for OBU type %d", obuType)
	}
}

func TestAV1Depacketizer_fragmentedOverMultiple(t *testing.T) {
	fullOBU, expected := createAV1OBU(
		obu.OBUTileGroup,
		[]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
	)
	obuf1 := fullOBU[:2]
	obuf2 := fullOBU[2:5]
	obuf3 := fullOBU[5:7]
	obuf4 := fullOBU[7:]

	depacketizer := AV1Depacketizer{}

	packet := make([]byte, 0)
	packet = append(packet, []byte{0b01000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obuf1)))...)
	packet = append(packet, obuf1...)

	obus, err := depacketizer.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Len(t, obus, 0, "Expected empty OBU for fragmented OBU")

	packet = make([]byte, 0)
	packet = append(packet, []byte{0b11000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obuf2)))...)
	packet = append(packet, obuf2...)

	obus, err = depacketizer.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Len(t, obus, 0, "Expected empty OBU for fragmented OBU")

	packet = make([]byte, 0)
	packet = append(packet, []byte{0b11000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obuf3)))...)
	packet = append(packet, obuf3...)

	obus, err = depacketizer.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Len(t, obus, 0, "Expected empty OBU for fragmented OBU")

	packet = make([]byte, 0)
	packet = append(packet, []byte{0b10000000}...)
	packet = append(packet, obu.WriteToLeb128(uint(len(obuf4)))...)
	packet = append(packet, obuf4...)

	obus, err = depacketizer.Unmarshal(packet)
	assert.NoError(t, err)
	assert.Equal(t, expected, obus)
}

func TestAV1Depacketizer_shortOBUHeader(t *testing.T) {
	d := AV1Depacketizer{}

	payload, err := d.Unmarshal([]byte{0x00, 0x01, 0x04})
	assert.Error(t, err)
	assert.Len(t, payload, 0, "Expected empty payload for short OBU header")
}

func TestAV1Depacketizer_aggregationHeader(t *testing.T) {
	depacketizer := AV1Depacketizer{}
	tests := []struct {
		name    string
		input   []byte
		payload []byte
		Z, Y, N bool
	}{
		{
			name: "Z=0, Y=0, N=0",
			// aggregation header = 0, length field = 1, obu header = 0x30
			input: []byte{0x00, 0x01, 0x30},
			// obu header = 0x32, obu size = 0
			payload: []byte{0x32, 0x00},
		},
		{
			name: "Z=1, Y=0, N=0",
			// aggregation header = z = 1, length field = 1, obu header = 0x20
			input: []byte{0x80, 0x01, 0x20},
			// packet is fragmented, with missing previous packet, so the result is empty
			payload: []byte{},
			Z:       true,
		},
		{
			name: "Z=0, Y=1, N=0",
			// aggregation header = Y = 1, length field = 1, obu header = 0x20
			input: []byte{0x40, 0x01, 0x04},
			// Packet is fragmented with the next packet.
			payload: []byte{},
			Y:       true,
		},
		{
			name: "Z=0, Y=0, N=1",
			// aggregation header = N = 1, length field = 1, obu header = 0x30
			input: []byte{0x08, 0x01, 0x30},
			// obu header = 0x32, obu size = 0
			payload: []byte{0x32, 0x00},
			N:       true,
		},
		{
			name: "Z=1, Y=1, N=1",
			// aggregation header = N, Y, Z = 1, length field = 1, obu header = 0x30
			input: []byte{0xC8, 0x01, 0x30},
			// Packet is fragmented no payload.
			payload: []byte{},
			Z:       true,
			Y:       true,
			N:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := depacketizer.Unmarshal(tt.input)
			assert.NoError(t, err)

			assert.Equal(t, tt.payload, payload)
			assert.Equal(t, tt.Z, depacketizer.Z)
			assert.Equal(t, tt.Y, depacketizer.Y)
			assert.Equal(t, tt.N, depacketizer.N)
		})
	}
}

func FuzzAV1DepacketizerUnmarshal(f *testing.F) {
	f.Add([]byte{0x10, 0x01, 0x00})
	f.Add([]byte{0x20, 0x01, 0x00, 0x01, 0x00})
	f.Add([]byte{0x00, 0x01, 0x00})
	f.Add([]byte{0x80, 0x01, 0x00})
	f.Add([]byte{0x40, 0x01, 0x00})
	f.Add([]byte{0x08, 0x01, 0x00})
	f.Add([]byte{0xC0, 0x01, 0x00})
	f.Add([]byte{0x30, 0x01, 0x00, 0x01, 0x00, 0x00})

	obuData, _ := createAV1OBU(obu.OBUFrameHeader, []byte{0x01, 0x02, 0x03})
	packet := append([]byte{0x00}, obu.WriteToLeb128(uint(len(obuData)))...)
	packet = append(packet, obuData...)
	f.Add(packet)

	obuData2, _ := createAV1OBU(obu.OBUFrame, []byte{0x04, 0x05})
	packet2 := append([]byte{0x10}, obuData2...)
	f.Add(packet2)

	// just check for crashes :)
	f.Fuzz(func(t *testing.T, data []byte) {
		depacketizer := &AV1Depacketizer{}
		_, err := depacketizer.Unmarshal(data)
		_ = err
	})
}

func TestAV1Depacketizer_fragmentAllocationGrowth(t *testing.T) {
	first := make([]byte, 1025)
	first[0], first[1] = 0x50, 0x20
	continuation := make([]byte, 1025)
	continuation[0] = 0xd0
	allocations := testing.AllocsPerRun(1, func() {
		depacketizer := AV1Depacketizer{}
		_, err := depacketizer.Unmarshal(first)
		require.NoError(t, err)
		for range 512 {
			_, err = depacketizer.Unmarshal(continuation)
			require.NoError(t, err)
		}
	})
	assert.Less(t, allocations, float64(64))
}

// requireRestoredAV1OBUs parses a depacketized low-overhead bitstream and
// asserts that every OBU carries an explicit obu_size_field whose value
// exactly matches the OBU payload, so each restored OBU has a well-defined
// length and boundary. It returns the OBUs found, each including its header,
// size field and payload.
func requireRestoredAV1OBUs(t *testing.T, data []byte) [][]byte {
	t.Helper()

	var obus [][]byte
	for offset := 0; offset < len(data); {
		start := offset
		header, err := obu.ParseOBUHeader(data[offset:])
		require.NoError(t, err)
		require.True(t, header.HasSizeField, "restored OBU must carry an explicit obu_size_field")
		offset += header.Size()

		obuSize, n, err := obu.ReadLeb128(data[offset:])
		require.NoError(t, err)
		offset += int(n)

		end := offset + int(obuSize)
		require.LessOrEqual(t, end, len(data), "declared OBU size exceeds the restored stream")
		obus = append(obus, data[start:end])
		offset = end
	}

	return obus
}

func TestAV1Depacketizer_fragmentedOBUExplicitBoundaries(t *testing.T) {
	// A tile group OBU fragmented over three packets, followed by a complete
	// frame header OBU in the last packet. The fragments mix the two length
	// representations allowed by the aggregation header: explicit LEB128
	// length fields and the omitted length form of the last OBU element
	// when W != 0.
	obuA, expectedA := createAV1OBU(obu.OBUTileGroup, []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})
	frag1 := obuA[:3]
	frag2 := obuA[3:6]
	frag3 := obuA[6:]
	obuB, expectedB := createAV1OBU(obu.OBUFrameHeader, []byte{0x09, 0x0A, 0x0B})

	depacketizer := AV1Depacketizer{}

	// First fragment: Y=1, explicit LEB128 length field.
	packet := append([]byte{0b01000000}, obu.WriteToLeb128(uint(len(frag1)))...)
	packet = append(packet, frag1...)
	out, err := depacketizer.Unmarshal(packet)
	require.NoError(t, err)
	assert.Empty(t, out, "incomplete OBU must not be delivered while waiting for fragments")

	// Middle fragment: Z=1, Y=1, W=1. The fragment fills the rest of the
	// packet, so its length is omitted and derived from the packet size.
	packet = append([]byte{0b11010000}, frag2...)
	out, err = depacketizer.Unmarshal(packet)
	require.NoError(t, err)
	assert.Empty(t, out, "incomplete OBU must not be delivered while waiting for fragments")

	// Final fragment followed by a complete OBU: Z=1, W=2. The first OBU
	// element (the final fragment) carries a LEB128 length field, the last
	// element uses the omitted length form.
	packet = append([]byte{0b10100000}, obu.WriteToLeb128(uint(len(frag3)))...)
	packet = append(packet, frag3...)
	packet = append(packet, obuB...)
	out, err = depacketizer.Unmarshal(packet)
	require.NoError(t, err)

	restored := requireRestoredAV1OBUs(t, out)
	require.Len(t, restored, 2)
	assert.Equal(t, expectedA, restored[0], "fragmented OBU must be reassembled with explicit length and boundary")
	assert.Equal(t, expectedB, restored[1], "omitted length form must not be mistaken for a truncated OBU")
}

func TestAV1Depacketizer_newSequenceDropsPendingFragment(t *testing.T) {
	depacketizer := AV1Depacketizer{}

	// Begin a fragmented OBU from the first coded video sequence.
	staleFrag := []byte{0x22, 0xAA, 0xBB}
	packet := append([]byte{0b01000000}, obu.WriteToLeb128(uint(len(staleFrag)))...)
	packet = append(packet, staleFrag...)
	out, err := depacketizer.Unmarshal(packet)
	require.NoError(t, err)
	assert.Empty(t, out)

	// A packet with N=1 starts a new coded video sequence. The incomplete OBU
	// from the previous sequence must be discarded, not prepended to the new
	// sequence's OBUs.
	seqHeader, expectedSeqHeader := createAV1OBU(obu.OBUSequenceHeader, []byte{0x01, 0x02})
	packet = append([]byte{0b00011000}, seqHeader...) // N=1, W=1, omitted length
	out, err = depacketizer.Unmarshal(packet)
	require.NoError(t, err)
	assert.Equal(t, expectedSeqHeader, out, "pending fragment from the previous sequence must not leak into the new one")

	// A late continuation fragment (Z=1) belonging to the aborted sequence
	// has no buffered start and must be dropped.
	orphan := []byte{0xCC, 0xDD}
	packet = append([]byte{0b10010000}, orphan...) // Z=1, W=1
	out, err = depacketizer.Unmarshal(packet)
	require.NoError(t, err)
	assert.Empty(t, out, "continuation without a buffered start must be dropped")

	// The new sequence continues to depacketize normally.
	frame, expectedFrame := createAV1OBU(obu.OBUFrame, []byte{0x03, 0x04, 0x05})
	packet = append([]byte{0b00010000}, frame...) // W=1, omitted length
	out, err = depacketizer.Unmarshal(packet)
	require.NoError(t, err)
	assert.Equal(t, expectedFrame, out)
}

func TestAV1Depacketizer_waitingFragmentVsCorruptedPayload(t *testing.T) {
	t.Run("waiting for continuation is not an error", func(t *testing.T) {
		depacketizer := AV1Depacketizer{}
		frag := []byte{0x22, 0x01, 0x02}
		packet := append([]byte{0b01000000}, obu.WriteToLeb128(uint(len(frag)))...)
		packet = append(packet, frag...)

		out, err := depacketizer.Unmarshal(packet)
		require.NoError(t, err)
		assert.Empty(t, out, "a fragmented OBU waits for its continuation instead of failing")
		assert.True(t, depacketizer.Y)
	})

	corrupted := []struct {
		name   string
		packet []byte
		err    error
	}{
		{
			name:   "length field exceeds payload",
			packet: []byte{0b00000000, 0x05, 0x22, 0x01},
			err:    errShortPacket,
		},
		{
			name:   "W count larger than OBU elements",
			packet: []byte{0b00110000, 0x01, 0x30},
			err:    errShortPacket,
		},
		{
			name:   "invalid LEB128 length field",
			packet: []byte{0b00000000, 0xFF, 0xFF},
			err:    obu.ErrFailedToReadLEB128,
		},
	}

	for _, tt := range corrupted {
		t.Run(tt.name, func(t *testing.T) {
			depacketizer := AV1Depacketizer{}
			out, err := depacketizer.Unmarshal(tt.packet)
			require.ErrorIs(t, err, tt.err)
			assert.Empty(t, out, "a corrupted payload must not be delivered as a complete video unit")

			// The depacketizer recovers: the next intact OBU is delivered normally.
			intact, expected := createAV1OBU(obu.OBUFrame, []byte{0x01, 0x02, 0x03})
			packet := append([]byte{0b00000000}, obu.WriteToLeb128(uint(len(intact)))...)
			packet = append(packet, intact...)
			out, err = depacketizer.Unmarshal(packet)
			require.NoError(t, err)
			assert.Equal(t, expected, out)
		})
	}
}
