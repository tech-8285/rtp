// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package rtp

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
)

// canonicalSpatialLayers returns the active layers sorted in the wire's
// canonical order: RTP stream first, spatial ID second.
func canonicalSpatialLayers(vla *VLA) []SpatialLayer {
	ordered := make([]SpatialLayer, 0, len(vla.ActiveSpatialLayer))
	for streamID := 0; streamID < vla.RTPStreamCount; streamID++ {
		for spatialID := range 4 {
			for _, sl := range vla.ActiveSpatialLayer {
				if sl.RTPStreamID == streamID && sl.SpatialID == spatialID {
					ordered = append(ordered, sl)
				}
			}
		}
	}

	return ordered
}

// packStreamMasks encodes the slX_bm fields, two stream masks per byte.
func packStreamMasks(vla *VLA, ordered []SpatialLayer) []byte {
	var slMBs [4]uint8
	for _, sl := range ordered {
		slMBs[sl.RTPStreamID] |= 1 << sl.SpatialID
	}

	maskBytes := make([]byte, 1+(vla.RTPStreamCount-1)/2)
	for streamID := 0; streamID < vla.RTPStreamCount; streamID++ {
		if streamID%2 == 0 {
			maskBytes[streamID/2] |= slMBs[streamID] << 4
		} else {
			maskBytes[streamID/2] |= slMBs[streamID]
		}
	}

	return maskBytes
}

// packTemporalLayerCounts encodes the #tl fields: two bits per active
// spatial layer in canonical order, four layers packed per byte.
func packTemporalLayerCounts(ordered []SpatialLayer) []byte {
	var tlBytes []byte
	tlByte, tlIndex := byte(0), 0
	for _, sl := range ordered {
		if tlIndex >= 4 {
			tlBytes = append(tlBytes, tlByte)
			tlByte, tlIndex = 0, 0
		}
		//nolint:gosec // temporal layer count is in 1..4
		tlByte |= byte(len(sl.TargetBitrates)-1) << (2 * (3 - tlIndex))
		tlIndex++
	}

	return append(tlBytes, tlByte)
}

// appendTargetBitrates appends LEB128 encoded bitrates in canonical order.
func appendTargetBitrates(out []byte, ordered []SpatialLayer) []byte {
	buf := make([]byte, 4)
	for _, sl := range ordered {
		for _, kbps := range sl.TargetBitrates {
			n := writeLeb128To(buf, uint(kbps)) //nolint:gosec
			out = append(out, buf[:n]...)
		}
	}

	return out
}

// appendResolutionAndFramerate appends the optional five-byte-per-layer
// width/height/framerate block in canonical order.
func appendResolutionAndFramerate(out []byte, ordered []SpatialLayer) []byte {
	for _, sl := range ordered {
		res := make([]byte, 5)
		binary.BigEndian.PutUint16(res[0:], uint16(sl.Width-1))  //nolint:gosec
		binary.BigEndian.PutUint16(res[2:], uint16(sl.Height-1)) //nolint:gosec
		res[4] = byte(sl.Framerate)                              //nolint:gosec
		out = append(out, res...)
	}

	return out
}

// buildVLAWithExplicitMasks builds a VLA payload that always carries the
// per-stream slX_bm form (sl_bm == 0), including entries for paused streams.
// It mirrors the wire layout produced by MarshalTo but never collapses streams
// sharing a layer distribution into the compact common-sl_bm form, so the
// decoder can be exercised on payloads where some streams have no active layer.
func buildVLAWithExplicitMasks(t *testing.T, vla *VLA) []byte {
	t.Helper()

	ordered := canonicalSpatialLayers(vla)

	// Header byte: RID, NS (RTPStreamCount-1), sl_bm = 0 (explicit masks follow).
	//nolint:gosec // RID and stream count are small validated values
	header := byte(vla.RTPStreamID<<6) | byte(vla.RTPStreamCount-1)<<4
	out := []byte{header}
	out = append(out, packStreamMasks(vla, ordered)...)
	out = append(out, packTemporalLayerCounts(ordered)...)
	out = appendTargetBitrates(out, ordered)
	if vla.HasResolutionAndFramerate {
		out = appendResolutionAndFramerate(out, ordered)
	}

	return out
}

func findSpatialLayer(layers []SpatialLayer, rtpStreamID, spatialID int) (SpatialLayer, bool) {
	for _, sl := range layers {
		if sl.RTPStreamID == rtpStreamID && sl.SpatialID == spatialID {
			return sl, true
		}
	}

	return SpatialLayer{}, false
}

// TestVLAOrderIndependentAllocation protects the bitrate/temporal-layer
// ownership when spatial layer descriptions are submitted in arbitrary
// order: the compact wire encoding iterates layers in canonical order, so
// shuffled submissions must produce the same bytes and unmarshal back with
// every value still attributed to its original stream and spatial layer.
func TestVLAOrderIndependentAllocation(t *testing.T) {
	canonical := []SpatialLayer{
		{RTPStreamID: 0, SpatialID: 0, TargetBitrates: []int{150}},
		{RTPStreamID: 1, SpatialID: 0, TargetBitrates: []int{240, 400}},
		{RTPStreamID: 2, SpatialID: 0, TargetBitrates: []int{720, 1200}},
	}
	// Same layers, deliberately submitted in a different order.
	shuffled := []SpatialLayer{canonical[2], canonical[0], canonical[1]}

	ordered := &VLA{
		RTPStreamID:        0,
		RTPStreamCount:     3,
		ActiveSpatialLayer: canonical,
	}
	scrambled := &VLA{
		RTPStreamID:        0,
		RTPStreamCount:     3,
		ActiveSpatialLayer: shuffled,
	}

	orderedBytes, err := ordered.Marshal()
	assert.NoError(t, err)
	scrambledBytes, err := scrambled.Marshal()
	assert.NoError(t, err)
	assert.Equal(t, orderedBytes, scrambledBytes,
		"submission order must not change the compact encoding")

	decoded := &VLA{}
	n, err := decoded.Unmarshal(scrambledBytes)
	assert.NoError(t, err)
	assert.Equal(t, len(scrambledBytes), n)
	assert.Equal(t, len(canonical), len(decoded.ActiveSpatialLayer))

	// Temporal layer counts stay with their original spatial layer.
	assertLayer := func(rtpStreamID, spatialID int, wantBitrates []int) {
		got, ok := findSpatialLayer(decoded.ActiveSpatialLayer, rtpStreamID, spatialID)
		assert.True(t, ok, "layer (stream=%d, spatial=%d) must be present", rtpStreamID, spatialID)
		assert.Equal(t, wantBitrates, got.TargetBitrates,
			"bitrates of stream %d spatial %d must not shift", rtpStreamID, spatialID)
		assert.Len(t, got.TargetBitrates, len(wantBitrates),
			"temporal layer count of stream %d spatial %d must stay with its layer", rtpStreamID, spatialID)
	}

	assertLayer(0, 0, []int{150})
	assertLayer(1, 0, []int{240, 400})
	assertLayer(2, 0, []int{720, 1200})

	assert.False(t, decoded.HasResolutionAndFramerate,
		"no resolution block was encoded, so the description must remain absent")
}

// TestVLAOrderIndependentResolutionAndFramerate pins the stream/layer
// ownership of width, height and framerate against the fixed canonical wire
// layout, including multiple spatial layers per stream. The bitrate and
// resolution sections must both resolve back to the stream/spatial layer
// they were declared for.
func TestVLAOrderIndependentResolutionAndFramerate(t *testing.T) {
	// Canonical order, single spatial layer per stream: known wire bytes.
	canonical := &VLA{
		RTPStreamID:    2,
		RTPStreamCount: 3,
		ActiveSpatialLayer: []SpatialLayer{
			{RTPStreamID: 0, SpatialID: 0, TargetBitrates: []int{150}, Width: 320, Height: 180, Framerate: 30},
			{RTPStreamID: 1, SpatialID: 0, TargetBitrates: []int{240, 400}, Width: 640, Height: 360, Framerate: 25},
			{RTPStreamID: 2, SpatialID: 0, TargetBitrates: []int{720, 1200}, Width: 1280, Height: 720, Framerate: 20},
		},
		HasResolutionAndFramerate: true,
	}
	expected, err := hex.DecodeString("a1149601f0019003d005b009013f00b31e027f01671904ff02cf14")
	assert.NoError(t, err)
	actual, err := canonical.Marshal()
	assert.NoError(t, err)
	assert.Equal(t, expected, actual)

	// Multiple spatial layers, shuffled on submission. Re-encode through the
	// explicit-mask builder (which writes layers in canonical order) and
	// verify every description lands on its own (stream, spatial) pair.
	layers := []SpatialLayer{
		{RTPStreamID: 0, SpatialID: 0, TargetBitrates: []int{100}, Width: 320, Height: 180, Framerate: 30},
		{RTPStreamID: 0, SpatialID: 2, TargetBitrates: []int{100, 200, 300}, Width: 640, Height: 360, Framerate: 60},
		{RTPStreamID: 1, SpatialID: 1, TargetBitrates: []int{150, 250}, Width: 960, Height: 540, Framerate: 24},
		{RTPStreamID: 1, SpatialID: 3, TargetBitrates: []int{400}, Width: 1920, Height: 1080, Framerate: 15},
	}
	shuffledVLA := &VLA{
		RTPStreamID:               1,
		RTPStreamCount:            2,
		ActiveSpatialLayer:        []SpatialLayer{layers[3], layers[1], layers[0], layers[2]},
		HasResolutionAndFramerate: true,
	}
	payload := buildVLAWithExplicitMasks(t, shuffledVLA)

	decoded := &VLA{}
	n, err := decoded.Unmarshal(payload)
	assert.NoError(t, err)
	assert.Equal(t, len(payload), n)
	assert.True(t, decoded.HasResolutionAndFramerate)
	assert.Len(t, decoded.ActiveSpatialLayer, len(layers))

	for _, want := range layers {
		got, ok := findSpatialLayer(decoded.ActiveSpatialLayer, want.RTPStreamID, want.SpatialID)
		assert.True(t, ok, "layer (stream=%d, spatial=%d) missing", want.RTPStreamID, want.SpatialID)
		assert.Equal(t, want.TargetBitrates, got.TargetBitrates)
		assert.Equal(t, want.Width, got.Width, "width must stay with stream %d spatial %d", want.RTPStreamID, want.SpatialID)
		assert.Equal(t, want.Height, got.Height, "height must stay with stream %d spatial %d", want.RTPStreamID, want.SpatialID)
		assert.Equal(t, want.Framerate, got.Framerate, "framerate must stay with stream %d spatial %d", want.RTPStreamID, want.SpatialID)
	}
}

// TestVLAOmittedDescriptionVersusZeroValue keeps the two meanings separate:
// an absent resolution block must not be reconstructed as a description,
// while an explicitly encoded zero value (framerate 0) is real data and
// must survive the round trip.
func TestVLAOmittedDescriptionVersusZeroValue(t *testing.T) {
	t.Run("omitted resolution block stays absent", func(t *testing.T) {
		vla := &VLA{
			RTPStreamID:    0,
			RTPStreamCount: 2,
			ActiveSpatialLayer: []SpatialLayer{
				{RTPStreamID: 0, SpatialID: 0, TargetBitrates: []int{200}},
				{RTPStreamID: 1, SpatialID: 0, TargetBitrates: []int{720, 1200}},
			},
			HasResolutionAndFramerate: false,
		}
		payload, err := vla.Marshal()
		assert.NoError(t, err)

		decoded := &VLA{}
		_, err = decoded.Unmarshal(payload)
		assert.NoError(t, err)
		assert.False(t, decoded.HasResolutionAndFramerate,
			"omitting the description must not be decoded as a present one")
		for _, sl := range decoded.ActiveSpatialLayer {
			assert.Zero(t, sl.Width)
			assert.Zero(t, sl.Height)
			assert.Zero(t, sl.Framerate)
		}
	})

	t.Run("explicit zero framerate is preserved", func(t *testing.T) {
		vla := &VLA{
			RTPStreamID:    0,
			RTPStreamCount: 1,
			ActiveSpatialLayer: []SpatialLayer{
				{RTPStreamID: 0, SpatialID: 0, TargetBitrates: []int{100}, Width: 320, Height: 180, Framerate: 0},
			},
			HasResolutionAndFramerate: true,
		}
		payload, err := vla.Marshal()
		assert.NoError(t, err)

		decoded := &VLA{}
		n, err := decoded.Unmarshal(payload)
		assert.NoError(t, err)
		assert.Equal(t, len(payload), n)
		assert.True(t, decoded.HasResolutionAndFramerate,
			"an encoded description with a zero field is still a description")
		got := decoded.ActiveSpatialLayer[0]
		assert.Equal(t, 320, got.Width)
		assert.Equal(t, 180, got.Height)
		assert.Zero(t, got.Framerate, "zero framerate is an explicit value, not an omission")
	})
}

// TestVLAPausedStreamsDoNotGainLayers ensures that streams without an active
// layer cannot recover a fabricated clarity allocation from the encoded
// omissions, even when the surrounding active streams share the same layer
// distribution. Payloads use explicit per-stream masks so the paused stream
// is represented by an empty bitmask.
func TestVLAPausedStreamsDoNotGainLayers(t *testing.T) {
	t.Run("middle stream paused while neighbors share distribution", func(t *testing.T) {
		active := []SpatialLayer{
			{RTPStreamID: 0, SpatialID: 0, TargetBitrates: []int{150}, Width: 320, Height: 180, Framerate: 30},
			{RTPStreamID: 2, SpatialID: 0, TargetBitrates: []int{720, 1200}, Width: 1280, Height: 720, Framerate: 30},
		}
		vla := &VLA{
			RTPStreamID:               1,
			RTPStreamCount:            3,
			ActiveSpatialLayer:        active,
			HasResolutionAndFramerate: true,
		}
		payload := buildVLAWithExplicitMasks(t, vla)

		decoded := &VLA{}
		n, err := decoded.Unmarshal(payload)
		assert.NoError(t, err)
		assert.Equal(t, len(payload), n)
		assert.True(t, decoded.HasResolutionAndFramerate)
		assert.Len(t, decoded.ActiveSpatialLayer, 2,
			"the paused middle stream must not gain a spatial layer")

		// Every decoded layer belongs to one of the active streams; no entry,
		// bitrate or resolution is attributed to stream 1.
		for _, sl := range decoded.ActiveSpatialLayer {
			assert.NotEqual(t, 1, sl.RTPStreamID, "paused stream must not be revived")
		}
		if _, ok := findSpatialLayer(decoded.ActiveSpatialLayer, 1, 0); ok {
			assert.Fail(t, "paused stream 1 recovered a fabricated layer")
		}

		got0, ok := findSpatialLayer(decoded.ActiveSpatialLayer, 0, 0)
		assert.True(t, ok)
		assert.Equal(t, []int{150}, got0.TargetBitrates)
		assert.Equal(t, 320, got0.Width)
		assert.Equal(t, 180, got0.Height)
		assert.Equal(t, 30, got0.Framerate)

		got2, ok := findSpatialLayer(decoded.ActiveSpatialLayer, 2, 0)
		assert.True(t, ok)
		assert.Equal(t, []int{720, 1200}, got2.TargetBitrates)
		assert.Equal(t, 1280, got2.Width)
		assert.Equal(t, 720, got2.Height)
		assert.Equal(t, 30, got2.Framerate)

		// The same allocation without the optional description must also
		// leave the paused stream empty rather than inventing clarity.
		plain := &VLA{
			RTPStreamID:        1,
			RTPStreamCount:     3,
			ActiveSpatialLayer: active,
		}
		plainPayload := buildVLAWithExplicitMasks(t, plain)
		plainDecoded := &VLA{}
		_, err = plainDecoded.Unmarshal(plainPayload)
		assert.NoError(t, err)
		assert.False(t, plainDecoded.HasResolutionAndFramerate)
		assert.Len(t, plainDecoded.ActiveSpatialLayer, 2)
		_, ok = findSpatialLayer(plainDecoded.ActiveSpatialLayer, 1, 0)
		assert.False(t, ok)
	})

	t.Run("outer stream paused", func(t *testing.T) {
		// 2 streams: stream 0 paused, stream 1 has one spatial layer with
		// two temporal layers and a resolution block.
		vla := &VLA{
			RTPStreamID:    1,
			RTPStreamCount: 2,
			ActiveSpatialLayer: []SpatialLayer{
				{RTPStreamID: 1, SpatialID: 0, TargetBitrates: []int{240, 400}, Width: 640, Height: 360, Framerate: 30},
			},
			HasResolutionAndFramerate: true,
		}
		payload := buildVLAWithExplicitMasks(t, vla)

		decoded := &VLA{}
		n, err := decoded.Unmarshal(payload)
		assert.NoError(t, err)
		assert.Equal(t, len(payload), n)
		assert.Len(t, decoded.ActiveSpatialLayer, 1)
		_, ok := findSpatialLayer(decoded.ActiveSpatialLayer, 0, 0)
		assert.False(t, ok, "paused outer stream must stay inactive")
		got, ok := findSpatialLayer(decoded.ActiveSpatialLayer, 1, 0)
		assert.True(t, ok)
		assert.Equal(t, []int{240, 400}, got.TargetBitrates)
		assert.Equal(t, 640, got.Width)
		assert.Equal(t, 360, got.Height)
	})
}

// TestVLASparseLayersWithOptionalDescription combines non-contiguous spatial
// IDs (sparse gaps) with the optional resolution block: the decoded optional
// layer set must be exactly the input set, gaps left empty and every
// bitrate/temporal/resolution value mapped to its declared layer.
func TestVLASparseLayersWithOptionalDescription(t *testing.T) {
	layers := []SpatialLayer{
		{RTPStreamID: 0, SpatialID: 0, TargetBitrates: []int{100}, Width: 320, Height: 180, Framerate: 30},
		{RTPStreamID: 0, SpatialID: 2, TargetBitrates: []int{100, 200, 300}, Width: 640, Height: 360, Framerate: 60},
		{RTPStreamID: 1, SpatialID: 1, TargetBitrates: []int{150, 250}, Width: 960, Height: 540, Framerate: 24},
		{RTPStreamID: 1, SpatialID: 3, TargetBitrates: []int{400}, Width: 1920, Height: 1080, Framerate: 15},
	}
	vla := &VLA{
		RTPStreamID:               0,
		RTPStreamCount:            2,
		ActiveSpatialLayer:        layers,
		HasResolutionAndFramerate: true,
	}
	payload := buildVLAWithExplicitMasks(t, vla)

	decoded := &VLA{}
	n, err := decoded.Unmarshal(payload)
	assert.NoError(t, err)
	assert.Equal(t, len(payload), n)
	assert.True(t, decoded.HasResolutionAndFramerate)

	// The optional layer set must match the input set exactly: same size,
	// same (stream, spatial) pairs, no layers filling the gaps.
	assert.Len(t, decoded.ActiveSpatialLayer, len(layers))
	gotPairs := map[[2]int]bool{}
	for _, sl := range decoded.ActiveSpatialLayer {
		gotPairs[[2]int{sl.RTPStreamID, sl.SpatialID}] = true
	}
	for _, want := range layers {
		assert.True(t, gotPairs[[2]int{want.RTPStreamID, want.SpatialID}])
		got, ok := findSpatialLayer(decoded.ActiveSpatialLayer, want.RTPStreamID, want.SpatialID)
		assert.True(t, ok)
		assert.Equal(t, want.TargetBitrates, got.TargetBitrates)
		assert.Len(t, got.TargetBitrates, len(want.TargetBitrates))
		assert.Equal(t, want.Width, got.Width)
		assert.Equal(t, want.Height, got.Height)
		assert.Equal(t, want.Framerate, got.Framerate)
	}
	// The sparse gaps (stream 0 spatial 1/3, stream 1 spatial 0/2) stay empty.
	for _, gap := range [][2]int{{0, 1}, {0, 3}, {1, 0}, {1, 2}} {
		assert.False(t, gotPairs[gap], "sparse gap stream=%d spatial=%d must stay empty", gap[0], gap[1])
	}
}

// TestVLAInvalidPayloadsNeverFormCompleteAllocation feeds truncated and
// malformed payloads. The decoder must return an error, must not mark the
// optional resolution/framerate description as present, and any partial
// layer information accumulated before the failure must not be treatable as
// a complete allocation.
func TestVLAInvalidPayloadsNeverFormCompleteAllocation(t *testing.T) {
	base := &VLA{
		RTPStreamID:    0,
		RTPStreamCount: 2,
		ActiveSpatialLayer: []SpatialLayer{
			{RTPStreamID: 0, SpatialID: 0, TargetBitrates: []int{100}, Width: 320, Height: 180, Framerate: 30},
			{RTPStreamID: 1, SpatialID: 1, TargetBitrates: []int{150, 250}, Width: 960, Height: 540, Framerate: 24},
		},
		HasResolutionAndFramerate: true,
	}
	full := buildVLAWithExplicitMasks(t, base)

	t.Run("empty payload", func(t *testing.T) {
		decoded := &VLA{}
		n, err := decoded.Unmarshal(nil)
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrVLATooShort)
		assert.Zero(t, n)
		assert.Empty(t, decoded.ActiveSpatialLayer)
		assert.False(t, decoded.HasResolutionAndFramerate)
	})

	t.Run("truncated before the structural boundary", func(t *testing.T) {
		// A payload ending exactly here is a complete VLA *without* the
		// optional description (len == offset after the bitrate section);
		// every shorter prefix is a truncated, therefore invalid, payload.
		structuralEnd := len(full) - len(base.ActiveSpatialLayer)*5
		for cut := range structuralEnd {
			decoded := &VLA{}
			n, err := decoded.Unmarshal(full[:cut])
			assert.Error(t, err, "cut=%d must be rejected", cut)
			assert.LessOrEqual(t, n, cut)
			assert.False(t, decoded.HasResolutionAndFramerate,
				"cut=%d must not complete an optional description", cut)
			if len(decoded.ActiveSpatialLayer) == len(base.ActiveSpatialLayer) {
				// Even if all layer shells were built before the failure, the
				// returned error means this is not a complete allocation.
				assert.NotNil(t, err)
			}
		}
	})

	t.Run("structural boundary decodes only without the optional description", func(t *testing.T) {
		// Ending exactly at the bitrate-section boundary is valid, but the
		// decoder must not invent the omitted resolution/framerate block.
		structuralEnd := len(full) - len(base.ActiveSpatialLayer)*5
		decoded := &VLA{}
		n, err := decoded.Unmarshal(full[:structuralEnd])
		assert.NoError(t, err)
		assert.Equal(t, structuralEnd, n)
		assert.False(t, decoded.HasResolutionAndFramerate)
		assert.Len(t, decoded.ActiveSpatialLayer, len(base.ActiveSpatialLayer))
	})

	t.Run("partial resolution block is rejected", func(t *testing.T) {
		structuralEnd := len(full) - len(base.ActiveSpatialLayer)*5
		// Any non-zero remainder inside the 5-bytes-per-layer description
		// region leaves the optional block incomplete and must fail without
		// flagging the description as present.
		for cut := structuralEnd + 1; cut < len(full); cut++ {
			decoded := &VLA{}
			n, err := decoded.Unmarshal(full[:cut])
			assert.Error(t, err, "cut=%d must be rejected", cut)
			assert.ErrorIs(t, err, ErrVLATooShort)
			assert.LessOrEqual(t, n, cut)
			assert.False(t, decoded.HasResolutionAndFramerate,
				"cut=%d must not mark a partial description as present", cut)
		}
	})

	t.Run("dangling LEB128 bitrate", func(t *testing.T) {
		// One active layer whose only bitrate is an unterminated LEB128.
		payload := []byte{0x11, 0x00, 0x00, 0x80, 0x80}
		decoded := &VLA{}
		n, err := decoded.Unmarshal(payload)
		assert.Error(t, err)
		assert.Less(t, n, len(payload))
		assert.False(t, decoded.HasResolutionAndFramerate)
	})

	t.Run("resolution block cut short", func(t *testing.T) {
		// Full payload minus one byte: the 5-byte-per-layer description no
		// longer fits, so the optional description must not be marked present.
		decoded := &VLA{}
		_, err := decoded.Unmarshal(full[:len(full)-1])
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrVLATooShort)
		assert.False(t, decoded.HasResolutionAndFramerate)
	})
}
