// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
)

func TestPacketizer(t *testing.T) {
	multiplepayload := make([]byte, 128)
	// use the G722 payloader here, because it's very simple and all 0s is valid G722 data.
	packetizer := NewPacketizer(100, 98, 0x1234ABCD, &codecs.G722Payloader{}, NewRandomSequencer(), 90000)
	packets := packetizer.Packetize(multiplepayload, 2000)

	expectedLen := 2
	if len(packets) != expectedLen {
		var packetlengths strings.Builder
		for i := range packets {
			fmt.Fprintf(&packetlengths, "Packet %d length %d\n", i, len(packets[i].Payload))
		}
		assert.Failf(
			t, "Packetize failed", "Generated %d packets instead of %d\n%s",
			len(packets), expectedLen, packetlengths.String(),
		)
	}
}

func TestPacketizer_AbsSendTime(t *testing.T) {
	// use the G722 payloader here, because it's very simple and all 0s is valid G722 data.
	pktizer := NewPacketizer(100, 98, 0x1234ABCD, &codecs.G722Payloader{}, NewFixedSequencer(1234), 90000)
	p, ok := pktizer.(*packetizer)
	assert.True(t, ok, "Failed to cast to *packetizer")

	p.Timestamp = 45678
	p.timegen = func() time.Time {
		return time.Date(1985, time.June, 23, 4, 0, 0, 0, time.FixedZone("UTC-5", -5*60*60))
		// (0xa0c65b1000000000>>14) & 0xFFFFFF  = 0x400000
	}
	pktizer.EnableAbsSendTime(1)

	payload := []byte{0x11, 0x12, 0x13, 0x14}
	packets := pktizer.Packetize(payload, 2000)

	expected := &Packet{
		Header: Header{
			Version:          2,
			Padding:          false,
			Extension:        true,
			Marker:           true,
			PayloadType:      98,
			SequenceNumber:   1234,
			Timestamp:        45678,
			SSRC:             0x1234ABCD,
			ExtensionProfile: 0xBEDE,
			Extensions: []Extension{
				{
					id:      1,
					payload: []byte{0x40, 0, 0},
				},
			},
		},
		Payload: []byte{0x11, 0x12, 0x13, 0x14},
	}

	assert.Lenf(t, packets, 1, "Generated %d packets instead of 1", len(packets))
	assert.Equal(t, expected, packets[0], "Packetize failed")
}

func TestPacketizer_Roundtrip(t *testing.T) {
	multiplepayload := make([]byte, 128)
	packetizer := NewPacketizer(100, 98, 0x1234ABCD, &codecs.G722Payloader{}, NewRandomSequencer(), 90000)
	packets := packetizer.Packetize(multiplepayload, 1000)

	rawPkts := make([][]byte, 0, 1400)
	for _, pkt := range packets {
		raw, err := pkt.Marshal()
		assert.NoError(t, err)

		rawPkts = append(rawPkts, raw)
	}

	for ndx, raw := range rawPkts {
		expectedPkt := packets[ndx]
		pkt := &Packet{}

		assert.NoError(t, pkt.Unmarshal(raw))
		assert.Equal(t, len(raw), pkt.MarshalSize())
		assert.Equal(t, expectedPkt.MarshalSize(), pkt.MarshalSize())
		assert.Equal(t, expectedPkt.Version, pkt.Version)
		assert.Equal(t, expectedPkt.Padding, pkt.Padding)
		assert.Equal(t, expectedPkt.Extension, pkt.Extension)
		assert.Equal(t, expectedPkt.Marker, pkt.Marker)
		assert.Equal(t, expectedPkt.PayloadType, pkt.PayloadType)
		assert.Equal(t, expectedPkt.SequenceNumber, pkt.SequenceNumber)
		assert.Equal(t, expectedPkt.Timestamp, pkt.Timestamp)
		assert.Equal(t, expectedPkt.SSRC, pkt.SSRC)
		assert.Equal(t, expectedPkt.CSRC, pkt.CSRC)
		assert.Equal(t, expectedPkt.ExtensionProfile, pkt.ExtensionProfile)
		assert.Equal(t, expectedPkt.Extensions, pkt.Extensions)
		assert.Equal(t, expectedPkt.Payload, pkt.Payload)

		pkt.PaddingSize = 0
		pkt.Header.PaddingSize = 0
		assert.Equal(t, expectedPkt, pkt)
	}
}

func TestPacketizer_GeneratePadding(t *testing.T) {
	pktizer := NewPacketizer(100, 98, 0x1234ABCD, &codecs.G722Payloader{}, NewFixedSequencer(1234), 90000)

	packets := pktizer.GeneratePadding(5)

	assert.Len(t, packets, 5, "Should generate exactly 5 padding packets")
	for i, pkt := range packets {
		assert.Equal(t, true, pkt.Header.Padding, "Packet %d should have Padding set to true", i)
		assert.Equal(t, byte(255), pkt.Header.PaddingSize, "Packet %d should have PaddingSize set to 255", i)
		assert.Equal(t, byte(0), pkt.PaddingSize, "Packet %d should have PaddingSize set to 0", i)
		assert.Nil(t, pkt.Payload, "Packet %d should have no Payload", i)
	}
}

func TestNewPacketizerWithOptions_DefaultValues(t *testing.T) {
	pack := NewPacketizerWithOptions(100, &codecs.G722Payloader{}, NewRandomSequencer(), 90000)
	p, ok := pack.(*packetizer)
	assert.True(t, ok, "Failed to cast to *packetizer")

	assert.Equal(t, uint16(100), p.MTU)
	assert.Equal(t, uint8(0), p.PayloadType)
	assert.Equal(t, uint32(0), p.SSRC)
	assert.NotZero(t, p.Timestamp)
	assert.Equal(t, uint32(90000), p.ClockRate)
}

func TestNewPacketizerWithOptions_WithOptions(t *testing.T) {
	pack := NewPacketizerWithOptions(
		100,
		&codecs.G722Payloader{},
		NewRandomSequencer(),
		90000,
		WithSSRC(0x1234ABCD),
		WithPayloadType(98),
		WithTimestamp(45678),
	)
	p, ok := pack.(*packetizer)
	assert.True(t, ok, "Failed to cast to *packetizer")

	assert.Equal(t, uint16(100), p.MTU)
	assert.Equal(t, uint8(98), p.PayloadType)
	assert.Equal(t, uint32(0x1234ABCD), p.SSRC)
	assert.Equal(t, uint32(45678), p.Timestamp)
	assert.Equal(t, uint32(90000), p.ClockRate)

	payload := []byte{0x11, 0x12, 0x13, 0x14}
	packets := pack.Packetize(payload, 2000)

	assert.Len(t, packets, 1, "Should generate exactly one packet")
	assert.Equal(t, uint8(98), packets[0].PayloadType)
	assert.Equal(t, uint32(0x1234ABCD), packets[0].SSRC)
	assert.Equal(t, uint32(45678), packets[0].Timestamp)
}

func TestNewPacketizerWithOptions_PartialOptions(t *testing.T) {
	pack := NewPacketizerWithOptions(
		100,
		&codecs.G722Payloader{},
		NewRandomSequencer(),
		90000,
		WithPayloadType(98),
	)
	p, ok := pack.(*packetizer)
	assert.True(t, ok, "Failed to cast to *packetizer")

	assert.Equal(t, uint16(100), p.MTU)
	assert.Equal(t, uint8(98), p.PayloadType)
	assert.Equal(t, uint32(0), p.SSRC)
	assert.NotZero(t, p.Timestamp)
	assert.Equal(t, uint32(90000), p.ClockRate)
}

func TestPacketizer_Empty_Payload(t *testing.T) {
	pktizer := NewPacketizer(100, 98, 0x1234ABCD, &codecs.G722Payloader{}, NewFixedSequencer(1234), 90000)
	const expectedSamples = uint32(4000)

	prevTimestamp := uint32(0)
	for i := range 10 {
		payload := []byte{0x11, 0x12, 0x13, 0x14}
		isEmptyPayload := i%2 == 0
		if isEmptyPayload {
			payload = nil
		}

		packets := pktizer.Packetize(payload, 2000)

		if isEmptyPayload {
			assert.Len(t, packets, 0)
		} else {
			assert.Len(t, packets, 1)

			if prevTimestamp != 0 {
				assert.Equal(t, packets[0].Timestamp-prevTimestamp, expectedSamples)
			}
			prevTimestamp = packets[0].Timestamp
		}
	}
}

func TestPacketizer_H264_StagedEncodingParamsDoNotEmitPlayoutTime(t *testing.T) {
	// Drive the RTP packetizer across several calls while the H264 payloader
	// stages SPS/PPS encoding parameters before the first video frame. Every
	// input declares the same fixed sample duration, so the RTP timestamp is a
	// deterministic running total regardless of whether the call emits packets.
	const (
		frameSamples = uint32(3000)
		startTS      = uint32(50000)
	)

	// Minimal Annex B style NAL units: SPS (type 7), PPS (type 8), an IDR
	// slice (type 5) and a non-IDR slice (type 1). SPS/PPS are retained by
	// the payloader, the slices are small enough to be single NAL units here.
	spsNALU := []byte{0x67, 0x00, 0x01}
	ppsNALU := []byte{0x68, 0x02, 0x03}
	idrNALU := []byte{0x65, 0x04, 0x05, 0x06}
	pframeNALU := []byte{0x41, 0x07, 0x08}

	pktizer := NewPacketizerWithOptions(
		1500,
		&codecs.H264Payloader{},
		NewFixedSequencer(100),
		90000,
		WithSSRC(0x1234ABCD),
		WithPayloadType(96),
		WithTimestamp(startTS),
	)
	pktizerImpl, ok := pktizer.(*packetizer)
	assert.True(t, ok, "Failed to cast to *packetizer")

	// An SPS-only input updates the encoding parameters but produces no RTP
	// packet. The media clock still advances by the declared sample duration.
	spsPackets := pktizer.Packetize(spsNALU, frameSamples)
	assert.Len(t, spsPackets, 0, "Staged SPS must not emit a packet")
	assert.Equal(t, startTS+frameSamples, pktizerImpl.Timestamp, "SPS input must still advance media time")

	// A PPS-only input behaves the same way: parameters are staged, no packet
	// is emitted, and the declared duration is accounted for once.
	ppsPackets := pktizer.Packetize(ppsNALU, frameSamples)
	assert.Len(t, ppsPackets, 0, "Staged PPS must not emit a packet")
	assert.Equal(t, startTS+2*frameSamples, pktizerImpl.Timestamp, "PPS input must advance media time")

	// The following video frame carries the previously staged parameters. The
	// STAP-A packet and the slice packet both belong to this single frame, so
	// they share its playout timestamp; the staged parameter durations are
	// already counted and must not be added again.
	idrPackets := pktizer.Packetize(idrNALU, frameSamples)
	assert.Len(t, idrPackets, 2, "IDR frame should be a STAP-A packet and a slice packet")
	for i, pkt := range idrPackets {
		assert.Equal(t, startTS+2*frameSamples, pkt.Timestamp, "IDR packet %d must use the frame timestamp", i)
		assert.Equal(t, uint16(100+i), pkt.SequenceNumber, "IDR packet %d must consume the next sequence number", i)
	}
	assert.False(t, idrPackets[0].Marker, "Only the last packet of the frame may set the marker bit")
	assert.True(t, idrPackets[1].Marker, "The last packet of the frame must set the marker bit")
	assert.Equal(t, startTS+3*frameSamples, pktizerImpl.Timestamp, "The IDR frame must advance media time exactly once")

	// An empty media input owns no transmission sequence number but its
	// declared sample duration is still attributed to the media timeline.
	emptyPackets := pktizer.Packetize(nil, frameSamples)
	assert.Len(t, emptyPackets, 0, "Empty media input must not emit a packet")
	assert.Equal(t, startTS+4*frameSamples, pktizerImpl.Timestamp, "Empty media input must advance media time")

	// An explicit sample skip is another duration with no packets and no
	// sequence numbers, and is likewise accounted for on the media timeline.
	pktizer.SkipSamples(frameSamples)
	assert.Equal(t, startTS+5*frameSamples, pktizerImpl.Timestamp, "Skipped samples must advance media time")

	// The next video frame plays at the accumulated timestamp. Staging the
	// parameters never created an independent playout frame, so there is no
	// extra duration between the IDR frame and this one.
	pframePackets := pktizer.Packetize(pframeNALU, frameSamples)
	assert.Len(t, pframePackets, 1, "P frame should be a single slice packet")
	assert.Equal(t, startTS+5*frameSamples, pframePackets[0].Timestamp, "P frame must play at the accumulated media time")
	assert.Equal(t, uint16(102), pframePackets[0].SequenceNumber, "Empty and skipped inputs must not consume sequence numbers")
	assert.True(t, pframePackets[0].Marker, "The single P frame packet must set the marker bit")
	assert.Equal(t, startTS+6*frameSamples, pktizerImpl.Timestamp, "The P frame must advance media time exactly once")
}

func TestPacketizer_H264_PaddingFragmentationAndRollover(t *testing.T) {
	// Exercise the transmission sequence (sequence numbers) independently of
	// the media timeline (timestamps) across FU-A fragmentation, pure padding
	// packets and 16/32 bit rollover, with a fixed sequencer and timestamp so
	// the result does not depend on the clock or randomness.
	const (
		mtu          = uint16(21)
		frameSamples = uint32(3000)
	)
	// A variable rather than a constant: the expected timestamps derived from
	// it intentionally wrap at the uint32 boundary, which constant arithmetic
	// would reject even though uint32 addition wraps at runtime.
	startTS := uint32(0xFFFFFFFF - 4)

	// SPS and PPS are deliberately tiny so the STAP-A packet (9 bytes) fits
	// the payloader MTU (mtu-12 = 9), while both slice NAL units are large
	// enough to be fragmented into FU-A packets with a 7 byte fragment size.
	spsNALU := []byte{0x67, 0x00}
	ppsNALU := []byte{0x68, 0x01}
	// 16 bytes total: 15 fragmentable bytes split 7+7+1 into three FU-A packets.
	idrNALU := []byte{
		0x65, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
		0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e,
	}
	// 11 bytes total: 10 fragmentable bytes split 7+3 into two FU-A packets.
	pframeNALU := []byte{0x41, 0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39}

	// Start the sequence number one step below the 16 bit wrap point.
	pktizer := NewPacketizerWithOptions(
		mtu,
		&codecs.H264Payloader{},
		NewFixedSequencer(0xFFFF),
		90000,
		WithSSRC(0x1234ABCD),
		WithPayloadType(96),
		WithTimestamp(startTS),
	)
	pktizerImpl, ok := pktizer.(*packetizer)
	assert.True(t, ok, "Failed to cast to *packetizer")
	seq, ok := pktizerImpl.Sequencer.(*sequencer)
	assert.True(t, ok, "Failed to cast to *sequencer")

	// Staged encoding parameters emit no packets and consume no sequence
	// numbers, but each call still accounts for its declared duration.
	assert.Len(t, pktizer.Packetize(spsNALU, frameSamples), 0, "Staged SPS must not emit a packet")
	assert.Equal(t, startTS+frameSamples, pktizerImpl.Timestamp, "SPS input must advance media time")
	assert.Len(t, pktizer.Packetize(ppsNALU, frameSamples), 0, "Staged PPS must not emit a packet")
	assert.Equal(t, startTS+2*frameSamples, pktizerImpl.Timestamp, "PPS input must advance media time")
	assert.Equal(t, uint64(0), seq.RollOverCount(), "Staged parameters must not consume sequence numbers")

	// The first frame is one STAP-A packet plus three FU-A fragments. All
	// four packets share the frame's playout timestamp, only the final
	// fragment carries the marker bit, and the sequence numbers wrap once.
	idrPackets := pktizer.Packetize(idrNALU, frameSamples)
	assert.Len(t, idrPackets, 4, "IDR frame should be one STAP-A packet and three FU-A fragments")
	expectedIDRSequences := []uint16{0xFFFF, 0x0000, 0x0001, 0x0002}
	for i, pkt := range idrPackets {
		assert.Equal(t, startTS+2*frameSamples, pkt.Timestamp, "IDR packet %d must share the frame timestamp", i)
		assert.Equal(t, expectedIDRSequences[i], pkt.SequenceNumber, "IDR packet %d has an unexpected sequence number", i)
		assert.False(t, pkt.Padding, "Media packets must not be padding")
		assert.Equal(t, i == 3, pkt.Marker, "Only the last IDR fragment may set the marker bit")
	}
	assert.Equal(t, uint64(1), seq.RollOverCount(), "The sequence number must have rolled over once")
	assert.Equal(t, byte(0x78), idrPackets[0].Payload[0], "The first packet must be a STAP-A NAL unit")
	// FU-A indicator/headers: NRI 0x60 with type 28, then S/middle/E headers
	// carrying the original IDR slice type (5).
	assert.Equal(t, []byte{0x7c, 0x85}, idrPackets[1].Payload[:2], "First FU-A fragment must set the start bit")
	assert.Equal(t, []byte{0x7c, 0x05}, idrPackets[2].Payload[:2], "Middle FU-A fragment must clear S and E bits")
	assert.Equal(t, []byte{0x7c, 0x45}, idrPackets[3].Payload[:2], "Last FU-A fragment must set the end bit")
	frame1TS := pktizerImpl.Timestamp
	assert.Equal(t, startTS+3*frameSamples, frame1TS, "The fragmented frame must advance media time exactly once")

	// Padding occupies the transmission sequence but must not move the media
	// clock, must not carry a marker bit and must hold no media payload.
	padding := pktizer.GeneratePadding(2)
	assert.Len(t, padding, 2, "Should generate exactly two padding packets")
	for i, pkt := range padding {
		assert.True(t, pkt.Padding, "Padding packet %d must set the padding bit", i)
		assert.False(t, pkt.Marker, "Padding packet %d must not set the marker bit", i)
		assert.Nil(t, pkt.Payload, "Padding packet %d must not carry media payload", i)
		assert.Equal(t, frame1TS, pkt.Timestamp, "Padding packet %d must reuse the latest media timestamp", i)
		assert.Equal(t, uint16(0x0003+i), pkt.SequenceNumber, "Padding packet %d must consume a sequence number", i)
	}
	assert.Equal(t, frame1TS, pktizerImpl.Timestamp, "Padding must not advance media time")

	// The next frame is fragmented into two packets. Its timestamp is the
	// unchanged media clock (the padding added nothing), and the 32 bit
	// timestamp wraps naturally as a uint32.
	pframePackets := pktizer.Packetize(pframeNALU, frameSamples)
	assert.Len(t, pframePackets, 2, "P frame should be two FU-A fragments")
	expectedPTS := frame1TS
	for i, pkt := range pframePackets {
		assert.Equal(t, expectedPTS, pkt.Timestamp, "P frame packet %d must use the unwrapped media timestamp", i)
		assert.Equal(t, uint16(0x0005+i), pkt.SequenceNumber, "P frame packet %d has an unexpected sequence number", i)
		assert.Equal(t, i == 1, pkt.Marker, "Only the last P frame fragment may set the marker bit")
	}
	assert.Equal(t, []byte{0x5c, 0x81}, pframePackets[0].Payload[:2], "First P frame fragment must set the start bit")
	assert.Equal(t, []byte{0x5c, 0x41}, pframePackets[1].Payload[:2], "Last P frame fragment must set the end bit")
	assert.Equal(t, frame1TS+frameSamples, pktizerImpl.Timestamp, "The P frame must advance media time exactly once")

	// Every emitted packet, including those past the sequence rollover, must
	// survive a marshal/unmarshal round trip without losing header state.
	for _, pkt := range append(append(idrPackets, padding...), pframePackets...) {
		raw, err := pkt.Marshal()
		assert.NoError(t, err)

		var back Packet
		assert.NoError(t, back.Unmarshal(raw))
		assert.Equal(t, pkt.SequenceNumber, back.SequenceNumber)
		assert.Equal(t, pkt.Timestamp, back.Timestamp)
		assert.Equal(t, pkt.Padding, back.Padding)
		assert.Equal(t, pkt.Marker, back.Marker)
	}
}

func FuzzPacketizer_Packetize_G722(f *testing.F) {
	// mixed seeds.
	f.Add(uint16(100), uint8(98), uint32(0x1234ABCD), uint32(960), false, []byte{0})
	f.Add(uint16(100), uint8(98), uint32(0x00000001), uint32(0), false, []byte{})
	f.Add(uint16(12), uint8(0), uint32(0), uint32(1), true, []byte{1, 2, 3, 4})
	f.Add(uint16(1500), uint8(120), uint32(0xCAFEBABE), uint32(480), true, make([]byte, 4096))
	f.Add(uint16(1), uint8(34), uint32(7), uint32(160), false, make([]byte, 32))

	f.Fuzz(func(t *testing.T, mtu uint16, pt uint8, ssrc uint32, samples uint32, enableAST bool, payload []byte) {
		if len(payload) > 1<<16 {
			payload = payload[:1<<16]
		}

		packetizer := NewPacketizerWithOptions(
			mtu,
			&codecs.G722Payloader{},
			NewFixedSequencer(0),
			90000,
			WithPayloadType(pt),
			WithSSRC(ssrc),
			WithTimestamp(0xAABBCCDD),
		)

		if enableAST {
			packetizer.EnableAbsSendTime(1)
		}

		packets := packetizer.Packetize(payload, samples)

		if len(payload) == 0 {
			assert.Nil(t, packets)

			return
		}

		eff := int(mtu - 12)
		if eff == 0 {
			assert.Equal(t, 0, len(packets))

			return
		}

		assert.GreaterOrEqual(t, len(packets), 1)

		for i, packet := range packets {
			assert.Equal(t, uint8(2), packet.Version)
			assert.Equal(t, pt, packet.PayloadType)
			assert.Equal(t, ssrc, packet.SSRC)

			if i == len(packets)-1 {
				assert.True(t, packet.Marker)

				if enableAST {
					assert.True(t, packet.Extension)
					raw, err := packet.Marshal()
					assert.NoError(t, err)

					var back Packet
					assert.NoError(t, back.Unmarshal(raw))
				}
			} else {
				assert.False(t, packet.Marker)
			}

			if mtu >= 12 && !enableAST {
				raw, err := packet.Marshal()
				assert.NoError(t, err)
				assert.LessOrEqual(t, len(raw), int(mtu))
			} else {
				raw, err := packet.Marshal()
				assert.NoError(t, err)

				var back Packet
				assert.NoError(t, back.Unmarshal(raw))
			}
		}
	})
}

func FuzzPacketizer_SkipSamples_And_Timestamps(f *testing.F) {
	// mixed seeds.
	f.Add(uint16(1200), uint32(0), uint32(480), uint32(960), uint16(100), uint16(200), false)
	f.Add(uint16(200), uint32(160), uint32(160), uint32(160), uint16(10), uint16(20), true)
	f.Add(uint16(20), uint32(32000), uint32(0), uint32(1), uint16(0), uint16(1), false)

	f.Fuzz(func(
		t *testing.T,
		mtu uint16,
		skip uint32,
		samples1 uint32,
		samples2 uint32,
		len1 uint16,
		len2 uint16,
		enableAST bool,
	) {
		p1 := make([]byte, int(len1))
		for i := range p1 {
			p1[i] = byte(i)
		}

		p2 := make([]byte, int(len2))
		for i := range p2 {
			p2[i] = byte(255 - i)
		}

		const startTS = uint32(0x10203040)
		packetizer := NewPacketizerWithOptions(
			mtu,
			&codecs.G722Payloader{},
			NewFixedSequencer(1000),
			90000,
			WithPayloadType(111),
			WithSSRC(0xFEEDBEEF),
			WithTimestamp(startTS),
		)

		if enableAST {
			packetizer.EnableAbsSendTime(1)
		}

		packetizer.SkipSamples(skip)

		pkts1 := packetizer.Packetize(p1, samples1)
		if len(p1) == 0 {
			assert.Nil(t, pkts1)
		} else {
			assert.GreaterOrEqual(t, len(pkts1), 1)
			assert.Equal(t, startTS+skip, pkts1[0].Timestamp)
		}

		pkts2 := packetizer.Packetize(p2, samples2)
		if len(p2) == 0 {
			assert.Nil(t, pkts2)
		} else {
			assert.GreaterOrEqual(t, len(pkts2), 1)
			expectedTS2 := startTS + skip + samples1
			assert.Equal(t, expectedTS2, pkts2[0].Timestamp)
		}

		for _, p := range append(pkts1, pkts2...) {
			raw, err := p.Marshal()
			assert.NoError(t, err)
			var back Packet
			assert.NoError(t, back.Unmarshal(raw))
		}
	})
}

func FuzzPacketizer_GeneratePadding(f *testing.F) {
	// mixed seeds.
	f.Add(uint32(0))
	f.Add(uint32(1))
	f.Add(uint32(5))
	f.Add(uint32(16))

	f.Fuzz(func(t *testing.T, samples uint32) {
		samples %= 64

		packetizer := NewPacketizerWithOptions(
			1200,
			&codecs.G722Payloader{},
			NewFixedSequencer(0),
			90000,
		)

		pads := packetizer.GeneratePadding(samples)
		if samples == 0 {
			assert.Nil(t, pads)

			return
		}

		assert.Len(t, pads, int(samples))
		for _, p := range pads {
			assert.True(t, p.Header.Padding)
			assert.Equal(t, byte(255), p.Header.PaddingSize)
			assert.Nil(t, p.Payload)

			raw, err := p.Marshal()
			assert.NoError(t, err)

			var back Packet
			assert.NoError(t, back.Unmarshal(raw))
			assert.True(t, back.Padding)
		}
	})
}
