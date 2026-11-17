// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"bytes"
	"errors"
	"testing"

	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
)

var h264TestAnnexBStartCode = []byte{0x00, 0x00, 0x00, 0x01} //nolint:gochecknoglobals // shared test fixture

const (
	// H264 NAL unit type octet fields, see RFC 6184 section 5.4.
	h264TestNALUTypeMask   = byte(0x1F)
	h264TestFUStartBitmask = byte(0x80)
	h264TestFUEndBitmask   = byte(0x40)
	h264TestFUANALUType    = byte(28)
	h264TestStapANALUType  = byte(24)
	h264TestSPSNALUType    = byte(7)
	h264TestPPSNALUType    = byte(8)

	h264TestPayloadType = uint8(96)
	h264TestSSRC        = uint32(0x1234ABCD)
	h264TestClockRate   = uint32(90000)
	h264TestRTPHeader   = 12
)

var (
	errH264TestSequenceGap    = errors.New("sequence number gap in RTP packet stream")
	errH264TestFrameTruncated = errors.New("access unit ended before fragmented NAL unit was reassembled")
)

// h264AnnexBNALU prefixes a NAL unit with an Annex-B start code.
func h264AnnexBNALU(nalu []byte) []byte {
	out := make([]byte, 0, len(nalu)+4)
	out = append(out, h264TestAnnexBStartCode...)

	return append(out, nalu...)
}

// h264AnnexBFrame joins NAL units into one Annex-B access unit.
func h264AnnexBFrame(nalus ...[]byte) []byte {
	frame := []byte{}
	for _, nalu := range nalus {
		frame = append(frame, h264AnnexBNALU(nalu)...)
	}

	return frame
}

// h264TestNALU builds a NAL unit with the given type/NRI and a deterministic
// payload of the requested size.
func h264TestNALU(naluType, nri byte, size int) []byte {
	nalu := make([]byte, size)
	nalu[0] = (nri << 5) | naluType
	for idx := 1; idx < size; idx++ {
		nalu[idx] = byte(idx * 7) // nolint:gosec // test data never reaches a security boundary
	}

	return nalu
}

// h264SplitAnnexB splits an Annex-B stream back into individual NAL units by
// their 4-byte start codes, so NAL boundaries can be asserted independently of
// the depacketizer output.
func h264SplitAnnexB(stream []byte) [][]byte {
	var nalus [][]byte

	rest := stream
	for len(rest) >= 4 {
		idx := bytes.Index(rest, h264TestAnnexBStartCode)
		if idx < 0 {
			break
		}

		nalStart := idx + len(h264TestAnnexBStartCode)
		next := bytes.Index(rest[nalStart:], h264TestAnnexBStartCode)
		if next < 0 {
			nalus = append(nalus, rest[nalStart:])

			break
		}

		nalus = append(nalus, rest[nalStart:nalStart+next])
		rest = rest[nalStart+next:]
	}

	return nalus
}

// h264RoundTrip drives the full send/receive pipeline: the payloader and
// packetizer produce RTP packets, which are marshaled and parsed again exactly
// as a network receiver would see them.
func h264RoundTrip(t *testing.T, mtu uint16, frames ...[]byte) []*Packet {
	t.Helper()

	packetizer := NewPacketizer(
		mtu,
		h264TestPayloadType,
		h264TestSSRC,
		&codecs.H264Payloader{},
		NewFixedSequencer(1),
		h264TestClockRate,
	)

	var wire [][]byte

	for _, frame := range frames {
		for _, pkt := range packetizer.Packetize(frame, h264TestClockRate/30) {
			raw, err := pkt.Marshal()
			assert.NoError(t, err)
			wire = append(wire, raw)
		}
	}

	received := make([]*Packet, 0, len(wire))

	for _, raw := range wire {
		pkt := &Packet{}
		assert.NoError(t, pkt.Unmarshal(raw))
		received = append(received, pkt)
	}

	return received
}

// h264SeqDiff returns the forward distance between two 16-bit sequence
// numbers, handling wraparound (1 after the previous number means contiguous).
func h264SeqDiff(next, prev uint16) uint16 {
	return next - prev
}

// h264AccessUnitAssembler models the upper layer (e.g. a track/sample builder)
// that sits on top of the Depacketizer. It owns two responsibilities the RTP
// payload parser deliberately does not: detecting sequence number gaps and
// deciding, via the marker bit, when an access unit is complete.
type h264AccessUnitAssembler struct {
	depacketizer codecs.H264Packet
	haveLastSeq  bool
	lastSeq      uint16
	media        []byte
}

func newH264AccessUnitAssembler() *h264AccessUnitAssembler {
	return &h264AccessUnitAssembler{depacketizer: codecs.H264Packet{}}
}

// Process parses one RTP packet. A parse error means the RTP payload itself is
// malformed; a sequence gap means the stream lost packets even though every
// received payload parsed successfully.
func (a *h264AccessUnitAssembler) Process(pkt *Packet) error {
	if a.haveLastSeq {
		if h264SeqDiff(pkt.SequenceNumber, a.lastSeq) != 1 {
			return errH264TestSequenceGap
		}
	}

	a.haveLastSeq = true
	a.lastSeq = pkt.SequenceNumber

	out, err := a.depacketizer.Unmarshal(pkt.Payload)
	if err != nil {
		return err
	}

	a.media = append(a.media, out...)

	return nil
}

// CompleteFrame returns the reassembled access unit and true only when the
// marker bit declares the sequence finished. A successfully parsed packet on
// its own never completes a frame: fragmented NAL units surface as empty
// parser output until the final fragment arrives.
func (a *h264AccessUnitAssembler) CompleteFrame(marker bool) ([]byte, bool, error) {
	if !marker {
		return nil, false, nil
	}

	// The marker ended the access unit. If no complete NAL unit was emitted,
	// the frame is truncated: the loss happened above the parse layer.
	if len(a.media) == 0 {
		return nil, true, errH264TestFrameTruncated
	}

	frame := a.media
	a.media = nil

	return frame, true, nil
}

// TestH264Integration_ParameterSetAggregation verifies that SPS/PPS parameter
// sets are aggregated into a STAP-A packet together with the first following
// NAL unit, and that depacketization restores every NAL unit with its exact
// content and boundaries in Annex-B order.
func TestH264Integration_ParameterSetAggregation(t *testing.T) {
	sps := h264TestNALU(h264TestSPSNALUType, 3, 15)
	pps := h264TestNALU(h264TestPPSNALUType, 3, 5)
	idr := h264TestNALU(5, 2, 64)

	// SPS and PPS arrive ahead of the IDR slice, mirroring a real encoder
	// emitting parameter sets before the first coded slice.
	packets := h264RoundTrip(t, 1200,
		h264AnnexBFrame(sps),
		h264AnnexBFrame(pps),
		h264AnnexBFrame(idr),
	)

	assert.Len(t, packets, 2, "STAP-A packet and IDR packet must be produced")

	// First RTP packet carries the STAP-A aggregation.
	assert.Equal(t, h264TestStapANALUType, packets[0].Payload[0]&h264TestNALUTypeMask)
	assert.False(t, packets[0].Marker, "only the final packet of a frame sets the marker bit")
	assert.True(t, packets[1].Marker, "the standalone IDR packet ends the frame")

	assembler := newH264AccessUnitAssembler()

	var reassembled []byte
	for idx, pkt := range packets {
		assert.NoError(t, assembler.Process(pkt))

		frame, done, err := assembler.CompleteFrame(pkt.Marker)
		assert.NoError(t, err)

		if done {
			reassembled = frame
			assert.Equal(t, len(packets)-1, idx, "frame must complete on the final packet")
		}
	}

	expected := h264AnnexBFrame(sps, pps, idr)
	assert.Equal(t, expected, reassembled, "reassembled stream must preserve every NAL unit byte-for-byte")

	nalus := h264SplitAnnexB(reassembled)
	assert.Len(t, nalus, 3, "all three NAL units must retain separate boundaries")
	assert.Equal(t, sps, nalus[0])
	assert.Equal(t, pps, nalus[1])
	assert.Equal(t, idr, nalus[2])

	// The same stream in AVCC length-prefixed mode must keep identical content,
	// with 4-byte big-endian lengths in place of start codes.
	avccAssembler := codecs.H264Packet{IsAVC: true}
	avcc := []byte{}
	for _, pkt := range packets {
		out, err := avccAssembler.Unmarshal(pkt.Payload)
		assert.NoError(t, err)
		avcc = append(avcc, out...)
	}

	assert.Len(t, avcc, len(expected))
	declaredSPSLen := uint32(avcc[0])<<24 | uint32(avcc[1])<<16 | uint32(avcc[2])<<8 | uint32(avcc[3])
	assert.Equal(t, uint32(len(sps)), declaredSPSLen) //nolint:gosec // test fixture is far smaller than 2^32
	assert.Equal(t, sps, avcc[4:4+len(sps)])
}

// TestH264Integration_FragmentationRoundtrip verifies FU-A fragmentation of a
// NAL unit larger than the MTU: start/end bits, NRI preservation, exact
// fragment payloads and lossless reassembly with a single reconstructed NAL
// boundary.
func TestH264Integration_FragmentationRoundtrip(t *testing.T) {
	largeNALU := h264TestNALU(1, 2, 200)
	packets := h264RoundTrip(t, 50, h264AnnexBFrame(largeNALU))

	// Effective payload budget is mtu-12 = 38; FU-A adds a 2-byte header, so
	// each fragment carries at most 36 NAL bytes.
	const effectiveMTU = uint16(50 - h264TestRTPHeader)
	const maxFragmentPayload = 50 - h264TestRTPHeader

	assert.Greater(t, len(packets), 1, "the NAL unit must be split into multiple FU-A packets")

	for idx, pkt := range packets {
		assert.LessOrEqual(t, len(pkt.Payload), int(effectiveMTU), "payload must fit the negotiated MTU budget")
		assert.Equal(t, h264TestFUANALUType, pkt.Payload[0]&h264TestNALUTypeMask, "every fragment is a FU-A")
		assert.Equal(t, byte(2)<<5, pkt.Payload[0]&0x60, "NRI must be preserved on every fragment")

		isStart := pkt.Payload[1]&h264TestFUStartBitmask != 0
		isEnd := pkt.Payload[1]&h264TestFUEndBitmask != 0
		assert.Equal(t, largeNALU[0]&h264TestNALUTypeMask, pkt.Payload[1]&h264TestNALUTypeMask,
			"FU header must carry the original NAL unit type")

		switch idx {
		case 0:
			assert.True(t, isStart, "first fragment must set the S bit")
			assert.False(t, isEnd)
		case len(packets) - 1:
			assert.False(t, isStart)
			assert.True(t, isEnd, "last fragment must set the E bit")
			assert.True(t, pkt.Marker, "last fragment carries the RTP marker bit")
		default:
			assert.False(t, isStart, "middle fragment must not set the S bit")
			assert.False(t, isEnd, "middle fragment must not set the E bit")
			assert.False(t, pkt.Marker)
		}

		if idx < len(packets)-1 {
			assert.Len(t, pkt.Payload, maxFragmentPayload, "non-final fragments fill the MTU budget")
		}
	}

	// Reassemble through the public Depacketizer, asserting that intermediate
	// fragments parse successfully but emit nothing. A dedicated instance is
	// required because H264Packet keeps the open FU-A reassembly buffer as
	// internal state, so every packet must be parsed exactly once per instance.
	assembler := newH264AccessUnitAssembler()
	probe := codecs.H264Packet{}

	var reassembled []byte

	for idx, pkt := range packets {
		out, err := probe.Unmarshal(pkt.Payload)
		assert.NoError(t, err)

		if idx < len(packets)-1 {
			assert.Empty(t, out, "a FU-A fragment without the E bit is not a complete NAL unit")
			assert.False(t,
				probe.IsDetectedFinalPacketInSequence(pkt.Marker),
				"marker bit must not report a complete frame for intermediate fragments",
			)
		} else {
			assert.NotEmpty(t, out, "the E bit fragment emits the reassembled NAL unit")
		}

		assert.NoError(t, assembler.Process(pkt))

		frame, done, err := assembler.CompleteFrame(pkt.Marker)
		assert.NoError(t, err)

		if done {
			reassembled = frame
		}
	}

	assert.Equal(t, h264AnnexBFrame(largeNALU), reassembled,
		"concatenated FU payloads must rebuild the exact original NAL unit")

	nalus := h264SplitAnnexB(reassembled)
	assert.Len(t, nalus, 1, "the fragments must merge back into a single NAL boundary")
	assert.Equal(t, largeNALU, nalus[0])
}

// TestH264Integration_MTUBoundary sweeps the exact size where a NAL unit
// switches between a single NAL unit packet and FU-A fragmentation, and
// verifies that no produced RTP packet ever exceeds the configured MTU.
func TestH264Integration_MTUBoundary(t *testing.T) {
	const mtu = uint16(60)

	// A NAL unit exactly matching the payloader budget (mtu-12) stays a single
	// NAL unit packet; one byte more forces FU-A fragmentation.
	exactFit := h264TestNALU(1, 0, int(mtu-h264TestRTPHeader))
	oneOver := h264TestNALU(1, 0, int(mtu-h264TestRTPHeader+1))

	fitPackets := h264RoundTrip(t, mtu, h264AnnexBFrame(exactFit))
	assert.Len(t, fitPackets, 1, "an exactly fitting NAL unit must not be fragmented")
	assert.NotEqual(t, h264TestFUANALUType, fitPackets[0].Payload[0]&h264TestNALUTypeMask,
		"exact-fit NAL unit is sent as a single NAL unit packet")
	assert.True(t, fitPackets[0].Marker)

	overPackets := h264RoundTrip(t, mtu, h264AnnexBFrame(oneOver))
	assert.Len(t, overPackets, 2, "one byte over the budget requires start and end FU-A fragments")
	assert.Equal(t, h264TestFUANALUType, overPackets[0].Payload[0]&h264TestNALUTypeMask)
	assert.True(t, overPackets[0].Payload[1]&h264TestFUStartBitmask != 0)
	assert.True(t, overPackets[1].Payload[1]&h264TestFUEndBitmask != 0)

	// Sweep MTUs across the fragmentation transition with a fixed large NAL
	// unit: every marshaled RTP packet must respect the MTU and every
	// fragmentation must reassemble losslessly. The lowest viable MTU leaves a
	// payloader budget of 3 (the 2-byte FU-A header plus one payload byte);
	// smaller budgets cannot represent a fragment and are covered below.
	nalu := h264TestNALU(5, 1, 300)

	for sweepMTU := uint16(h264TestRTPHeader + 3); sweepMTU <= 200; sweepMTU++ {
		payloader := &codecs.H264Payloader{}
		packetizer := NewPacketizer(
			sweepMTU,
			h264TestPayloadType,
			h264TestSSRC,
			payloader,
			NewFixedSequencer(1),
			h264TestClockRate,
		)

		packets := packetizer.Packetize(h264AnnexBFrame(nalu), h264TestClockRate/30)
		assert.NotEmpty(t, packets, "MTU %d must still produce packets", sweepMTU)

		depacketizer := codecs.H264Packet{}
		rebuilt := []byte{}

		for idx, pkt := range packets {
			raw, err := pkt.Marshal()
			assert.NoErrorf(t, err, "marshal failed at MTU %d", sweepMTU)
			assert.LessOrEqualf(t, len(raw), int(sweepMTU),
				"MTU %d: RTP packet exceeded the configured MTU", sweepMTU)
			assert.Equal(t, idx == len(packets)-1, pkt.Marker)

			out, err := depacketizer.Unmarshal(pkt.Payload)
			assert.NoErrorf(t, err, "depacketize failed at MTU %d", sweepMTU)
			rebuilt = append(rebuilt, out...)
		}

		assert.Equalf(t, h264AnnexBFrame(nalu), rebuilt,
			"MTU %d: reassembly must reproduce the original NAL unit", sweepMTU)
	}

	// An MTU whose payloader budget (mtu-12) cannot fit the 2-byte FU-A header
	// plus one payload byte produces no packets, rather than an oversized one.
	tinyPacketizer := NewPacketizer(
		h264TestRTPHeader+2, // payloader budget becomes 2, FU-A needs 2 header bytes
		h264TestPayloadType,
		h264TestSSRC,
		&codecs.H264Payloader{},
		NewFixedSequencer(1),
		h264TestClockRate,
	)
	assert.Empty(t, tinyPacketizer.Packetize(h264AnnexBFrame(nalu), 1))
}

// TestH264Integration_TruncatedAndMissingFragments pins down where each error
// condition is the caller's responsibility: malformed RTP payloads fail at the
// parse layer, while lost fragments of otherwise valid packets are invisible to
// the parser and can only be caught by the upper layer's sequence and marker
// handling.
func TestH264Integration_TruncatedAndMissingFragments(t *testing.T) {
	largeNALU := h264TestNALU(1, 2, 120)
	packets := h264RoundTrip(t, 40, h264AnnexBFrame(largeNALU))
	assert.Greater(t, len(packets), 2, "test needs several fragments")

	t.Run("malformed payloads are parse-layer errors", func(t *testing.T) {
		depacketizer := codecs.H264Packet{}

		// Empty payload cannot carry a NAL unit type octet.
		_, err := depacketizer.Unmarshal(nil)
		assert.Error(t, err)

		// FU-A indicator without the mandatory FU header octet.
		_, err = depacketizer.Unmarshal([]byte{h264TestFUANALUType})
		assert.Error(t, err)

		// STAP-A declaring a NAL unit longer than the remaining buffer.
		stapa := []byte{h264TestStapANALUType, 0x00, 0x20, 0x67, 0x01}
		_, err = depacketizer.Unmarshal(stapa)
		assert.Error(t, err)
	})

	t.Run("a single parsed RTP packet is not a decodable frame", func(t *testing.T) {
		assembler := newH264AccessUnitAssembler()

		// The start fragment parses without error, but emits no complete NAL
		// unit...
		out, err := assembler.depacketizer.Unmarshal(packets[0].Payload)
		assert.NoError(t, err)
		assert.Empty(t, out)

		// ...and neither the parser output nor the marker boundary indicate a
		// complete frame, so the upper layer must not hand anything to a
		// decoder yet.
		frame, done, err := assembler.CompleteFrame(packets[0].Marker)
		assert.NoError(t, err)
		assert.False(t, done)
		assert.Nil(t, frame)
	})

	t.Run("missing final fragment is detected at the access-unit boundary", func(t *testing.T) {
		assembler := newH264AccessUnitAssembler()

		// Feed every fragment except the one carrying the E bit. Every RTP
		// payload remains well-formed, so parsing never returns an error.
		for _, pkt := range packets[:len(packets)-1] {
			assert.NoError(t, assembler.Process(pkt))
			assert.False(t, pkt.Marker)
		}

		// Simulate the frame boundary arriving (e.g. a timestamp change) with
		// the reassembly still open: the upper layer owns this failure.
		frame, done, err := assembler.CompleteFrame(true)
		assert.ErrorIs(t, err, errH264TestFrameTruncated)
		assert.True(t, done)
		assert.Nil(t, frame)
	})

	t.Run("missing middle fragment is detected via sequence numbers", func(t *testing.T) {
		assembler := newH264AccessUnitAssembler()
		assert.NoError(t, assembler.Process(packets[0]))

		// Drop packets[1] and continue with the fragment after it.
		err := assembler.Process(packets[2])
		assert.ErrorIs(t, err, errH264TestSequenceGap,
			"the payload parses fine; only the upper layer can observe the loss")
	})

	t.Run("full fragment stream still reassembles after the loss cases", func(t *testing.T) {
		assembler := newH264AccessUnitAssembler()

		var frame []byte

		for _, pkt := range packets {
			assert.NoError(t, assembler.Process(pkt))

			reassembled, done, err := assembler.CompleteFrame(pkt.Marker)
			assert.NoError(t, err)

			if done {
				frame = reassembled
			}
		}

		assert.Equal(t, h264AnnexBFrame(largeNALU), frame)
	})
}

// h264IsFUAFragment reports whether the RTP payload is a non-final FU-A
// fragment, the kind of packet a receiver may lose mid-frame.
func h264IsFUAFragment(pkt *Packet) bool {
	return pkt.Payload[0]&h264TestNALUTypeMask == h264TestFUANALUType && !pkt.Marker
}

// h264AssertWireFraming verifies RTP-level framing independent of the codec:
// exactly the expected number of marker-delimited frames and contiguous
// sequence numbers.
func h264AssertWireFraming(t *testing.T, packets []*Packet, expectedFrames int) {
	t.Helper()

	markerCount := 0
	for idx, pkt := range packets {
		if pkt.Marker {
			markerCount++
		}

		if idx > 0 {
			assert.Equal(t, uint16(1), h264SeqDiff(pkt.SequenceNumber, packets[idx-1].SequenceNumber))
		}
	}

	assert.Equal(t, expectedFrames, markerCount)
}

// TestH264Integration_EndToEndPacketLoss exercises two consecutive access
// units (a parameter-set key frame and a fragmented P frame) end to end,
// dropping a fragment on the wire. The complete first frame must decode while
// the damaged second frame is rejected, proving NAL boundaries stay intact
// across frames and the loss does not corrupt neighboring access units.
func TestH264Integration_EndToEndPacketLoss(t *testing.T) { //nolint:cyclop // table of frame-loss recovery branches
	sps := h264TestNALU(h264TestSPSNALUType, 3, 12)
	pps := h264TestNALU(h264TestPPSNALUType, 3, 6)
	keySlice := h264TestNALU(5, 2, 40)
	largeSlice := h264TestNALU(1, 2, 240)

	packets := h264RoundTrip(t, 80,
		h264AnnexBFrame(sps, pps, keySlice),
		h264AnnexBFrame(largeSlice),
	)

	var keyFrame, pFrame []byte
	frameIndex := 0
	assembler := newH264AccessUnitAssembler()
	skipped := false
	gapDetections := 0

	for idx := 0; idx < len(packets); idx++ {
		pkt := packets[idx]

		// Drop one middle FU-A fragment of the second frame on the floor, as a
		// network receiver would.
		if frameIndex == 1 && idx > 0 && h264IsFUAFragment(pkt) && !skipped {
			skipped = true

			continue
		}

		err := assembler.Process(pkt)
		if errors.Is(err, errH264TestSequenceGap) {
			// Upper layer resets the broken access unit and skips to the next
			// frame boundary instead of feeding corrupt bytes to the decoder.
			gapDetections++
			for !pkt.Marker && idx+1 < len(packets) {
				idx++
				pkt = packets[idx]
			}

			continue
		}

		assert.NoError(t, err)

		frame, done, frameErr := assembler.CompleteFrame(pkt.Marker)
		if frameErr != nil {
			assert.ErrorIs(t, frameErr, errH264TestFrameTruncated)

			continue
		}

		if done {
			if frameIndex == 0 {
				keyFrame = frame
			} else {
				pFrame = frame
			}
			frameIndex++
		}
	}

	assert.True(t, skipped, "the test must actually drop a fragment")
	assert.Equal(t, 1, gapDetections, "the lost fragment must surface as one sequence gap, not a parse error")
	assert.Equal(t, h264AnnexBFrame(sps, pps, keySlice), keyFrame,
		"the undamaged key frame must reassemble with all NAL boundaries intact")
	assert.Nil(t, pFrame, "the frame with a missing fragment must never be presented as decodable")

	h264AssertWireFraming(t, packets, 2)
}
