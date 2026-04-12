package gokhttp_download

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"sync"

	mp4 "github.com/BRUHItsABunny/gomedia/go-mp4"
)

type fmp4TrackState struct {
	trackMap    map[int]uint32 // demuxer trackId -> muxer trackId
	initialized bool
	firstDts    map[int]uint64
	firstDtsSet map[int]bool
}

type StreamFMP4Muxer struct {
	F                io.WriteSeeker
	Muxer            *mp4.Movmuxer
	InitSegment      []byte // video init segment (EXT-X-MAP)
	AudioInitSegment []byte // audio init segment (separate EXT-X-MAP)
	video            fmp4TrackState
	audio            fmp4TrackState
	mu               sync.Mutex
}

func newTrackState() fmp4TrackState {
	return fmp4TrackState{
		trackMap:    make(map[int]uint32),
		firstDts:    make(map[int]uint64),
		firstDtsSet: make(map[int]bool),
	}
}

func NewStreamFMP4Muxer(f io.WriteSeeker) (*StreamFMP4Muxer, error) {
	muxer, err := mp4.CreateMp4Muxer(f, mp4.WithMp4Flag(mp4.MP4_FLAG_FRAGMENT|mp4.MP4_FLAG_KEYFRAME))
	if err != nil {
		return nil, fmt.Errorf("mp4.CreateMp4Muxer: %w", err)
	}

	return &StreamFMP4Muxer{
		F:     f,
		Muxer: muxer,
		video: newTrackState(),
		audio: newTrackState(),
	}, nil
}

func (m *StreamFMP4Muxer) SetInitSegment(data []byte) {
	m.InitSegment = data
}

func (m *StreamFMP4Muxer) SetAudioInitSegment(data []byte) {
	m.AudioInitSegment = data
}

// demuxSegment extracts all packets from a segment using the given init data and track state.
// It initializes muxer tracks on first call. Returns packets with normalized timestamps.
func (m *StreamFMP4Muxer) demuxSegment(initData, segmentData []byte, state *fmp4TrackState) ([]*mp4.AVPacket, error) {
	initReader := bytes.NewReader(initData)
	segReader := bytes.NewReader(segmentData)

	concat, err := mp4.NewConcatReadSeeker([]io.ReadSeeker{initReader, segReader})
	if err != nil {
		return nil, fmt.Errorf("mp4.NewConcatReadSeeker: %w", err)
	}

	demuxer := mp4.CreateMp4Demuxer(concat)
	infos, err := demuxer.ReadHead()
	if err != nil {
		return nil, fmt.Errorf("demuxer.ReadHead: %w", err)
	}

	if !state.initialized {
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
			state.trackMap[info.TrackId] = muxTrackId
		}
		state.initialized = true
	}

	var packets []*mp4.AVPacket
	for {
		pkt, err := demuxer.ReadPacket()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("demuxer.ReadPacket: %w", err)
		}
		muxTrackId, ok := state.trackMap[pkt.TrackId]
		if !ok {
			continue
		}

		// Normalize timestamps so output starts at 0
		if !state.firstDtsSet[pkt.TrackId] {
			state.firstDts[pkt.TrackId] = pkt.Dts
			state.firstDtsSet[pkt.TrackId] = true
		}
		pkt.Pts = pkt.Pts - state.firstDts[pkt.TrackId]
		pkt.Dts = pkt.Dts - state.firstDts[pkt.TrackId]
		pkt.TrackId = int(muxTrackId)

		packets = append(packets, pkt)
	}

	return packets, nil
}

// ProcessSegment demuxes a video segment and writes packets to the muxer.
func (m *StreamFMP4Muxer) ProcessSegment(segmentData []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	packets, err := m.demuxSegment(m.InitSegment, segmentData, &m.video)
	if err != nil {
		return err
	}
	return m.writePackets(packets)
}

// ProcessAudioSegment demuxes an audio segment and writes packets to the muxer.
func (m *StreamFMP4Muxer) ProcessAudioSegment(segmentData []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	initData := m.AudioInitSegment
	if initData == nil {
		initData = m.InitSegment
	}
	packets, err := m.demuxSegment(initData, segmentData, &m.audio)
	if err != nil {
		return err
	}
	return m.writePackets(packets)
}

// ProcessAudioVideoSegments demuxes both an audio and video segment, interleaves
// packets by DTS, and writes them to the muxer. Audio is written before video at
// the same DTS so it's included in the fragment before a keyframe triggers a flush.
func (m *StreamFMP4Muxer) ProcessAudioVideoSegments(videoData, audioData []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	audioInitData := m.AudioInitSegment
	if audioInitData == nil {
		audioInitData = m.InitSegment
	}

	videoPackets, err := m.demuxSegment(m.InitSegment, videoData, &m.video)
	if err != nil {
		return fmt.Errorf("video demux: %w", err)
	}
	audioPackets, err := m.demuxSegment(audioInitData, audioData, &m.audio)
	if err != nil {
		return fmt.Errorf("audio demux: %w", err)
	}

	// Merge and sort by DTS, audio before video at same DTS
	all := make([]*mp4.AVPacket, 0, len(videoPackets)+len(audioPackets))
	all = append(all, videoPackets...)
	all = append(all, audioPackets...)
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].Dts < all[j].Dts
	})

	return m.writePackets(all)
}

func (m *StreamFMP4Muxer) writePackets(packets []*mp4.AVPacket) error {
	for _, pkt := range packets {
		if err := m.Muxer.Write(uint32(pkt.TrackId), pkt.Data, pkt.Pts, pkt.Dts); err != nil {
			return fmt.Errorf("muxer.Write: %w", err)
		}
	}
	return nil
}

func (m *StreamFMP4Muxer) Close() error {
	return m.Muxer.WriteTrailer()
}
