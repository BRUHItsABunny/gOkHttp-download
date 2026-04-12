package gokhttp_download

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/BRUHItsABunny/gomedia/go-codec"
	mp4 "github.com/BRUHItsABunny/gomedia/go-mp4"
	mpeg2 "github.com/BRUHItsABunny/gomedia/go-mpeg2"
)

// initialTSDelay is the initial timestamp offset added to all PTS/DTS
// when writing to MPEG-TS, matching ffmpeg's default mux delay.
// This gives the decoder time to buffer before playback starts and
// ensures the initial PCR is non-zero for proper player compatibility.
const initialTSDelay = 1400 // milliseconds, matching ffmpeg's default

var mp4CodecToTSStream = map[mp4.MP4_CODEC_TYPE]mpeg2.TS_STREAM_TYPE{
	mp4.MP4_CODEC_H264: mpeg2.TS_STREAM_H264,
	mp4.MP4_CODEC_H265: mpeg2.TS_STREAM_H265,
	mp4.MP4_CODEC_AAC:  mpeg2.TS_STREAM_AAC,
	mp4.MP4_CODEC_MP2:  mpeg2.TS_STREAM_AUDIO_MPEG1,
	mp4.MP4_CODEC_MP3:  mpeg2.TS_STREAM_AUDIO_MPEG2,
}

type fmp4TrackState struct {
	initialized bool
}

type pendingPacket struct {
	streamId uint16
	data     []byte
	pts      uint64
	dts      uint64
	isVideo  bool
}

type FMP4Demuxer struct {
	Muxer            *StreamMuxer
	InitSegment      []byte
	AudioInitSegment []byte
	video            fmp4TrackState
	audio            fmp4TrackState
	seenKeyframe     bool
	origin           uint64
	originSet        bool
	streamsReady     bool

	pending     []pendingPacket
	hasAudio    bool
	hasVideo    bool
	maxAudioDts uint64
	maxVideoDts uint64

	debugLog *os.File
	mu       sync.Mutex
}

func NewFMP4Demuxer(muxer *StreamMuxer) *FMP4Demuxer {
	d := &FMP4Demuxer{Muxer: muxer}
	// Apply initial TS delay for player compatibility
	d.Muxer.Muxer.RebaseTimestamps(initialTSDelay)
	f, err := os.OpenFile("fmp4_debug.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err == nil {
		d.debugLog = f
	}
	return d
}

func (d *FMP4Demuxer) SetInitSegment(data []byte) {
	d.InitSegment = data
	d.tryRegisterStreams()
}

func (d *FMP4Demuxer) SetAudioInitSegment(data []byte) {
	d.AudioInitSegment = data
	d.tryRegisterStreams()
}

func (d *FMP4Demuxer) tryRegisterStreams() {
	if d.streamsReady || d.InitSegment == nil || d.AudioInitSegment == nil {
		return
	}
	for _, initData := range [][]byte{d.InitSegment, d.AudioInitSegment} {
		reader := bytes.NewReader(initData)
		demuxer := mp4.CreateMp4Demuxer(reader)
		infos, err := demuxer.ReadHead()
		if err != nil {
			continue
		}
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
	d.video.initialized = true
	d.audio.initialized = true
	d.streamsReady = true
}

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

func (d *FMP4Demuxer) flushInterleaved() error {
	if len(d.pending) == 0 {
		return nil
	}

	sort.SliceStable(d.pending, func(i, j int) bool {
		return d.pending[i].dts < d.pending[j].dts
	})

	var flushUpTo uint64
	if d.hasAudio && d.hasVideo {
		flushUpTo = d.maxAudioDts
		if d.maxVideoDts < flushUpTo {
			flushUpTo = d.maxVideoDts
		}
	} else {
		if d.AudioInitSegment == nil {
			flushUpTo = d.maxVideoDts
		} else {
			return nil
		}
	}

	writeIdx := 0
	for i, pkt := range d.pending {
		if pkt.dts > flushUpTo {
			break
		}
		if err := d.Muxer.Muxer.Write(pkt.streamId, pkt.data, pkt.pts, pkt.dts); err != nil {
			return fmt.Errorf("tsMuxer.Write: %w", err)
		}
		writeIdx = i + 1
	}

	if writeIdx > 0 {
		d.pending = d.pending[writeIdx:]
		d.hasAudio = false
		d.hasVideo = false
		d.maxAudioDts = 0
		d.maxVideoDts = 0
		for _, pkt := range d.pending {
			if pkt.isVideo {
				d.hasVideo = true
				if pkt.dts > d.maxVideoDts {
					d.maxVideoDts = pkt.dts
				}
			} else {
				d.hasAudio = true
				if pkt.dts > d.maxAudioDts {
					d.maxAudioDts = pkt.dts
				}
			}
		}
	}
	return nil
}

func (d *FMP4Demuxer) FlushAll() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.pending) == 0 {
		return nil
	}

	sort.SliceStable(d.pending, func(i, j int) bool {
		return d.pending[i].dts < d.pending[j].dts
	})

	for _, pkt := range d.pending {
		if err := d.Muxer.Muxer.Write(pkt.streamId, pkt.data, pkt.pts, pkt.dts); err != nil {
			return fmt.Errorf("tsMuxer.Write: %w", err)
		}
	}
	d.pending = d.pending[:0]
	d.hasAudio = false
	d.hasVideo = false
	return nil
}

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

		tsType, ok := mp4CodecToTSStream[pkt.Cid]
		if !ok {
			continue
		}
		streamId, ok := d.Muxer.Streams[tsType]
		if !ok {
			continue
		}

		isVideo := pkt.Cid == mp4.MP4_CODEC_H264 || pkt.Cid == mp4.MP4_CODEC_H265
		if isVideo && !d.seenKeyframe {
			switch pkt.Cid {
			case mp4.MP4_CODEC_H264:
				if !codec.IsH264IDRFrame(pkt.Data) {
					continue
				}
			case mp4.MP4_CODEC_H265:
				if !codec.IsH265IDRFrame(pkt.Data) {
					continue
				}
			}
			d.seenKeyframe = true
		}

		if !d.originSet {
			d.origin = pkt.Dts
			d.originSet = true
		}

		pts := pkt.Pts - d.origin
		dts := pkt.Dts - d.origin

		if d.debugLog != nil {
			streamType := "AUDIO"
			if isVideo {
				streamType = "VIDEO"
			}
			isIDR := isVideo && pkt.Cid == mp4.MP4_CODEC_H264 && codec.IsH264IDRFrame(pkt.Data)
			fmt.Fprintf(d.debugLog, "%s codec=%d rawDts=%d rawPts=%d origin=%d outDts=%d outPts=%d len=%d idr=%v\n",
				streamType, pkt.Cid, pkt.Dts, pkt.Pts, d.origin, dts, pts, len(pkt.Data), isIDR)
		}

		d.pending = append(d.pending, pendingPacket{
			streamId: streamId,
			data:     pkt.Data,
			pts:      pts,
			dts:      dts,
			isVideo:  isVideo,
		})

		if isVideo {
			d.hasVideo = true
			if dts > d.maxVideoDts {
				d.maxVideoDts = dts
			}
		} else {
			d.hasAudio = true
			if dts > d.maxAudioDts {
				d.maxAudioDts = dts
			}
		}
	}

	return d.flushInterleaved()
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
