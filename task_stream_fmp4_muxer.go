package gokhttp_download

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	mp4 "github.com/BRUHItsABunny/gomedia/go-mp4"
)

type StreamFMP4Muxer struct {
	F                io.WriteSeeker
	Muxer            *mp4.Movmuxer
	InitSegment      []byte         // video init segment (EXT-X-MAP)
	AudioInitSegment []byte         // audio init segment (separate EXT-X-MAP)
	videoTrackMap    map[int]uint32 // video demuxer trackId -> muxer trackId
	audioTrackMap    map[int]uint32 // audio demuxer trackId -> muxer trackId
	videoInitialized bool
	audioInitialized bool
	mu               sync.Mutex
}

func NewStreamFMP4Muxer(f io.WriteSeeker) (*StreamFMP4Muxer, error) {
	muxer, err := mp4.CreateMp4Muxer(f, mp4.WithMp4Flag(mp4.MP4_FLAG_FRAGMENT|mp4.MP4_FLAG_KEYFRAME))
	if err != nil {
		return nil, fmt.Errorf("mp4.CreateMp4Muxer: %w", err)
	}

	return &StreamFMP4Muxer{
		F:             f,
		Muxer:         muxer,
		videoTrackMap: make(map[int]uint32),
		audioTrackMap: make(map[int]uint32),
	}, nil
}

func (m *StreamFMP4Muxer) SetInitSegment(data []byte) {
	m.InitSegment = data
}

func (m *StreamFMP4Muxer) SetAudioInitSegment(data []byte) {
	m.AudioInitSegment = data
}

func (m *StreamFMP4Muxer) processSegmentWithInit(initData, segmentData []byte, trackMap map[int]uint32, initialized *bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

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

	if !*initialized {
		for _, info := range infos {
			var muxTrackId uint32
			if info.Cid == mp4.MP4_CODEC_H264 || info.Cid == mp4.MP4_CODEC_H265 {
				muxTrackId = m.Muxer.AddVideoTrack(info.Cid,
					mp4.WithVideoWidth(info.Width),
					mp4.WithVideoHeight(info.Height),
				)
			} else {
				muxTrackId = m.Muxer.AddAudioTrack(info.Cid,
					mp4.WithAudioChannelCount(info.ChannelCount),
					mp4.WithAudioSampleRate(info.SampleRate),
					mp4.WithAudioSampleBits(uint8(info.SampleSize)),
				)
			}
			trackMap[info.TrackId] = muxTrackId
		}
		*initialized = true
	}

	for {
		pkt, err := demuxer.ReadPacket()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("demuxer.ReadPacket: %w", err)
		}
		muxTrackId, ok := trackMap[pkt.TrackId]
		if !ok {
			continue
		}
		if err := m.Muxer.Write(muxTrackId, pkt.Data, pkt.Pts, pkt.Dts); err != nil {
			return fmt.Errorf("muxer.Write: %w", err)
		}
	}

	return nil
}

func (m *StreamFMP4Muxer) ProcessSegment(segmentData []byte) error {
	return m.processSegmentWithInit(m.InitSegment, segmentData, m.videoTrackMap, &m.videoInitialized)
}

func (m *StreamFMP4Muxer) ProcessAudioSegment(segmentData []byte) error {
	initData := m.AudioInitSegment
	if initData == nil {
		initData = m.InitSegment
	}
	return m.processSegmentWithInit(initData, segmentData, m.audioTrackMap, &m.audioInitialized)
}

func (m *StreamFMP4Muxer) Close() error {
	return m.Muxer.WriteTrailer()
}
