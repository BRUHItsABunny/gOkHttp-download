package gokhttp_download

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/BRUHItsABunny/gomedia/go-codec"
	mp4 "github.com/BRUHItsABunny/gomedia/go-mp4"
	mpeg2 "github.com/BRUHItsABunny/gomedia/go-mpeg2"
)

// mp4CodecToTSStream maps MP4 codec types to MPEG-TS stream types.
var mp4CodecToTSStream = map[mp4.MP4_CODEC_TYPE]mpeg2.TS_STREAM_TYPE{
	mp4.MP4_CODEC_H264: mpeg2.TS_STREAM_H264,
	mp4.MP4_CODEC_H265: mpeg2.TS_STREAM_H265,
	mp4.MP4_CODEC_AAC:  mpeg2.TS_STREAM_AAC,
	mp4.MP4_CODEC_MP2:  mpeg2.TS_STREAM_AUDIO_MPEG1,
	mp4.MP4_CODEC_MP3:  mpeg2.TS_STREAM_AUDIO_MPEG2,
}

type fmp4TrackState struct {
	initialized  bool
	seenKeyframe bool // skip video frames until first IDR
}

// FMP4Demuxer demuxes fMP4/M4S segments and feeds frames to a StreamMuxer (TS output).
type FMP4Demuxer struct {
	Muxer            *StreamMuxer
	InitSegment      []byte // video init segment (EXT-X-MAP)
	AudioInitSegment []byte // audio init segment (separate EXT-X-MAP)
	video            fmp4TrackState
	audio            fmp4TrackState
	// Shared DTS base across audio and video for correct A/V sync
	firstDts    uint64
	firstDtsSet bool
	mu          sync.Mutex
}

func NewFMP4Demuxer(muxer *StreamMuxer) *FMP4Demuxer {
	return &FMP4Demuxer{
		Muxer: muxer,
	}
}

func (d *FMP4Demuxer) SetInitSegment(data []byte) {
	d.InitSegment = data
}

func (d *FMP4Demuxer) SetAudioInitSegment(data []byte) {
	d.AudioInitSegment = data
}

// ensureStreams registers any new codec types from the demuxed track info with the TS muxer.
func (d *FMP4Demuxer) ensureStreams(infos []mp4.TrackInfo) {
	for _, info := range infos {
		tsType, ok := mp4CodecToTSStream[info.Cid]
		if !ok {
			continue
		}
		if _, exists := d.Muxer.Streams[tsType]; !exists {
			streamId := d.Muxer.Muxer.AddStream(tsType)
			d.Muxer.Streams[tsType] = streamId
		}
	}
}

// processSegment demuxes an fMP4 segment and writes frames to the TS muxer.
func (d *FMP4Demuxer) processSegment(initData, segmentData []byte, state *fmp4TrackState) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	initReader := bytes.NewReader(initData)
	segReader := bytes.NewReader(segmentData)

	concat, err := mp4.NewConcatReadSeeker([]io.ReadSeeker{initReader, segReader})
	if err != nil {
		return fmt.Errorf("mp4.NewConcatReadSeeker: %w", err)
	}

	demuxer := mp4.CreateMp4Demuxer(concat)
	infos, err := demuxer.ReadHead()
	if err != nil {
		return fmt.Errorf("demuxer.ReadHead: %w", err)
	}

	if !state.initialized {
		d.ensureStreams(infos)
		state.initialized = true
	}

	for {
		pkt, err := demuxer.ReadPacket()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("demuxer.ReadPacket: %w", err)
		}

		// Find the TS stream type for this codec
		tsType, ok := mp4CodecToTSStream[pkt.Cid]
		if !ok {
			continue
		}
		streamId, ok := d.Muxer.Streams[tsType]
		if !ok {
			continue
		}

		// Skip video frames until we see the first IDR (which includes SPS/PPS)
		if !state.seenKeyframe {
			switch pkt.Cid {
			case mp4.MP4_CODEC_H264:
				if !codec.IsH264IDRFrame(pkt.Data) {
					continue
				}
				state.seenKeyframe = true
			case mp4.MP4_CODEC_H265:
				if !codec.IsH265IDRFrame(pkt.Data) {
					continue
				}
				state.seenKeyframe = true
			default:
				// Audio: skip until we've seen a video keyframe
				continue
			}
		}

		// Normalize timestamps using a shared base for correct A/V sync
		if !d.firstDtsSet {
			d.firstDts = pkt.Dts
			d.firstDtsSet = true
		}
		pts := pkt.Pts - d.firstDts
		dts := pkt.Dts - d.firstDts

		// TS muxer expects milliseconds (converts to 90kHz internally)
		if err := d.Muxer.Muxer.Write(streamId, pkt.Data, pts, dts); err != nil {
			return fmt.Errorf("tsMuxer.Write: %w", err)
		}
	}

	return nil
}

func (d *FMP4Demuxer) ProcessSegment(segmentData []byte) error {
	return d.processSegment(d.InitSegment, segmentData, &d.video)
}

func (d *FMP4Demuxer) ProcessAudioSegment(segmentData []byte) error {
	initData := d.AudioInitSegment
	if initData == nil {
		initData = d.InitSegment
	}
	return d.processSegment(initData, segmentData, &d.audio)
}
