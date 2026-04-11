package gokhttp_download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gokhttp_requests "github.com/BRUHItsABunny/gOkHttp/requests"
	gokhttp_responses "github.com/BRUHItsABunny/gOkHttp/responses"
	"github.com/cornelk/hashmap"
	"github.com/dustin/go-humanize"
	"github.com/etherlabsio/go-m3u8/m3u8"
	"go.uber.org/atomic"
	"golang.org/x/sync/errgroup"
)

type StreamFormat int

const (
	StreamFormatTS StreamFormat = iota
	StreamFormatFMP4
)

type StreamStats struct {
	DownloadedSegments *atomic.Uint64 `json:"downloadedSegments"`
	DownloadedDuration *atomic.Int64  `json:"downloadedDuration"`
	DownloadedBytes    *atomic.Uint64 `json:"downloadedBytes"`
}

type StreamHLSTask struct {
	// Tracking ref
	Global  *GlobalDownloadTracker    `json:"-"`
	HClient *http.Client              `json:"-"`
	ReqOpts []gokhttp_requests.Option `json:"-"`

	// Muxers (only one is used, based on Format)
	Muxer     *StreamMuxer     `json:"-"`
	FMP4Muxer *StreamFMP4Muxer `json:"-"`
	TaskStats *StreamStats     `json:"taskStats"`

	TaskType     DownloadType                    `json:"taskType"`
	TaskVersion  DownloadVersion                 `json:"taskVersion"`
	FileName     *atomic.String                  `json:"fileName"`
	FileLocation *atomic.String                  `json:"fileLocation"`
	PlayListUrl  *atomic.String                  `json:"playListUrl"`
	BaseUrl      *url.URL                        `json:"-"`
	Opts         []gokhttp_requests.Option       `json:"-"`
	SegmentChan  chan *m3u8.SegmentItem          `json:"-"`
	BufferChan   chan *bytes.Buffer              `json:"-"`
	SegmentCache *hashmap.Map[string, time.Time] `json:"-"`
	SaveSegments bool                            `json:"-"`

	// Format detection (set during getSegments init)
	Format     StreamFormat `json:"format"`
	outputFile *os.File

	// Audio stream support
	HasAudioStream    bool                            `json:"-"`
	AudioPlaylistUrl  *atomic.String                  `json:"-"`
	AudioBaseUrl      *url.URL                        `json:"-"`
	AudioSegmentChan  chan *m3u8.SegmentItem          `json:"-"`
	AudioBufferChan   chan *bytes.Buffer              `json:"-"`
	AudioSegmentCache *hashmap.Map[string, time.Time] `json:"-"`
	audioWg           sync.WaitGroup
}

func (st *StreamHLSTask) Download(ctx context.Context) error {
	errGr, ctx := errgroup.WithContext(ctx)
	errGr.Go(func() error {
		err := st.getSegments(ctx)
		if err != nil {
			return fmt.Errorf("st.getSegments: %w", err)
		}
		return nil
	})
	errGr.Go(func() error {
		err := st.downloadSegments(ctx)
		if err != nil {
			return fmt.Errorf("st.downloadSegments: %w", err)
		}
		return nil
	})
	errGr.Go(func() error {
		err := st.mergeSegments()
		if err != nil {
			return fmt.Errorf("st.mergeSegments: %w", err)
		}
		return nil
	})
	errGr.Go(func() error {
		st.cleanUp()
		return nil
	})

	// Block until done
	st.Global.Add(1)
	st.Global.TotalThreads.Inc()
	err := errGr.Wait()

	// Wait for audio goroutines to finish
	st.audioWg.Wait()

	if err != nil {
		st.Global.Done()
		return fmt.Errorf("errGr.Wait: %w", err)
	}
	st.Global.Done()
	st.Global.TotalThreads.Dec()

	// Close muxer and file
	if st.FMP4Muxer != nil {
		if closeErr := st.FMP4Muxer.Close(); closeErr != nil {
			err = fmt.Errorf("st.FMP4Muxer.Close: %w", closeErr)
		}
	}
	if st.outputFile != nil {
		if closeErr := st.outputFile.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("outputFile.Close: %w", closeErr)
		}
	} else if st.Muxer != nil {
		if closeErr := st.Muxer.F.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("st.Muxer.F.Close: %w", closeErr)
		}
	}

	return err
}

func (st *StreamHLSTask) Type() DownloadType {
	return st.TaskType
}

func (st *StreamHLSTask) Progress(sb *strings.Builder) error {
	sb.WriteString(fmt.Sprintf("%s\n", Truncate(st.FileLocation.Load(), 128, 0)))
	sb.WriteString(fmt.Sprintf("Downloading stream: %s in data, %s in playtime and %d segments\n", humanize.Bytes(st.TaskStats.DownloadedBytes.Load()), (time.Duration(st.TaskStats.DownloadedDuration.Load()) * time.Millisecond).Round(time.Second).String(), st.TaskStats.DownloadedSegments.Load()))
	return nil
}

func (st *StreamHLSTask) ResetDelta() {
	// nothing? since i don't record per-tick data?
}

func NewStreamHLSTask(global *GlobalDownloadTracker, hClient *http.Client, playlistUrl, fileLocation string, saveSegments bool, opts ...gokhttp_requests.Option) (*StreamHLSTask, error) {
	// Strip known extensions - actual extension set after format detection
	fileLocation = strings.TrimSuffix(fileLocation, ".ts")
	fileLocation = strings.TrimSuffix(fileLocation, ".mp4")

	fileDir := filepath.Dir(fileLocation)
	err := os.MkdirAll(fileDir, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("os.MkdirAll: %w", err)
	}

	parsedUrl, err := url.Parse(playlistUrl)
	if err != nil {
		return nil, fmt.Errorf("url.Parse: %w", err)
	}

	pathSplit := strings.Split(parsedUrl.Path, "/")
	baseUrl := &url.URL{
		Scheme:     parsedUrl.Scheme,
		Opaque:     parsedUrl.Opaque,
		User:       parsedUrl.User,
		Host:       parsedUrl.Host,
		Path:       strings.Join(pathSplit[:len(pathSplit)-1], "/"),
		OmitHost:   parsedUrl.OmitHost,
		ForceQuery: parsedUrl.ForceQuery,
		RawQuery:   parsedUrl.RawQuery,
		Fragment:   parsedUrl.Fragment,
	}

	result := &StreamHLSTask{
		Global:      global,
		HClient:     hClient,
		ReqOpts:     opts,
		TaskType:    DownloadTypeLiveHLS,
		TaskVersion: DownloadVersionV1,
		TaskStats: &StreamStats{
			DownloadedSegments: atomic.NewUint64(0),
			DownloadedDuration: atomic.NewInt64(0),
			DownloadedBytes:    atomic.NewUint64(0),
		},
		FileName:     atomic.NewString(filepath.Base(fileLocation)),
		FileLocation: atomic.NewString(fileLocation),
		SegmentChan:  make(chan *m3u8.SegmentItem),
		BufferChan:   make(chan *bytes.Buffer),
		PlayListUrl:  atomic.NewString(playlistUrl),
		BaseUrl:      baseUrl,
		Opts:         opts,
		SegmentCache: hashmap.New[string, time.Time](),
		SaveSegments: saveSegments,
	}
	global.Tasks.Set(fileLocation, result)
	return result, nil
}

func (st *StreamHLSTask) cleanUp() {
	ticker := time.Tick(time.Second)
	tickerClean := time.Tick(time.Minute)
	for {
		select {
		case <-ticker:
			break
		case <-tickerClean:
			st.SegmentCache.Range(func(key string, val time.Time) bool {
				if time.Minute < time.Now().Sub(val) {
					st.SegmentCache.Del(key)
				}
				return true
			})
			if st.AudioSegmentCache != nil {
				st.AudioSegmentCache.Range(func(key string, val time.Time) bool {
					if time.Minute < time.Now().Sub(val) {
						st.AudioSegmentCache.Del(key)
					}
					return true
				})
			}
			break
		}

		if st.Global.GraceFulStop.Load() {
			break
		}
	}
}

// initOutputFile creates the output file and muxer after format detection.
func (st *StreamHLSTask) initOutputFile() error {
	fileLocation := st.FileLocation.Load()
	var err error

	if st.Format == StreamFormatFMP4 {
		fileLocation += ".mp4"
		st.FileLocation.Store(fileLocation)
		st.FileName.Store(filepath.Base(fileLocation))

		f, err := os.OpenFile(fileLocation, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			return fmt.Errorf("os.OpenFile: %w", err)
		}
		st.outputFile = f

		st.FMP4Muxer, err = NewStreamFMP4Muxer(f)
		if err != nil {
			return fmt.Errorf("NewStreamFMP4Muxer: %w", err)
		}
	} else {
		fileLocation += ".ts"
		st.FileLocation.Store(fileLocation)
		st.FileName.Store(filepath.Base(fileLocation))

		f, err := os.OpenFile(fileLocation, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			return fmt.Errorf("os.OpenFile: %w", err)
		}
		st.outputFile = f
		st.Muxer = NewStreamMuxer(f)
	}

	return err
}

// downloadInitSegment fetches the init segment (EXT-X-MAP URI) and returns its bytes.
func (st *StreamHLSTask) downloadInitSegment(ctx context.Context, base *url.URL, mapURI string) ([]byte, error) {
	initURL := buildURLFromBase(base, mapURI)
	req, err := gokhttp_requests.MakeGETRequest(ctx, initURL, st.Opts...)
	if err != nil {
		return nil, fmt.Errorf("requests.MakeGETRequest: %w", err)
	}
	resp, err := st.HClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hClient.Do: %w", err)
	}
	data, err := gokhttp_responses.ResponseBytes(resp)
	if err != nil {
		return nil, fmt.Errorf("responses.ResponseBytes: %w", err)
	}
	return data, nil
}

// findMapItem iterates playlist items looking for a *m3u8.MapItem (EXT-X-MAP).
func findMapItem(playList *m3u8.Playlist) *m3u8.MapItem {
	for _, item := range playList.Items {
		if mapItem, ok := item.(*m3u8.MapItem); ok {
			return mapItem
		}
	}
	return nil
}

// findAudioMediaItem finds the audio MediaItem matching the given group ID.
func findAudioMediaItem(playList *m3u8.Playlist, groupID string) *m3u8.MediaItem {
	for _, item := range playList.Items {
		if mediaItem, ok := item.(*m3u8.MediaItem); ok {
			if mediaItem.Type == "AUDIO" && mediaItem.GroupID == groupID && mediaItem.URI != nil {
				return mediaItem
			}
		}
	}
	return nil
}

func (st *StreamHLSTask) getSegments(ctx context.Context) error {
	var (
		req      *http.Request
		resp     *http.Response
		respText string
		playList *m3u8.Playlist
		err      error
	)

	playlistUrl := st.PlayListUrl.Load()

	// Resolve master playlist to media playlist
	for !(playList != nil && !playList.IsMaster()) {
		req, err = gokhttp_requests.MakeGETRequest(ctx, playlistUrl, st.Opts...)
		if err != nil {
			return fmt.Errorf("requests.MakeGETRequest: %w", err)
		}
		resp, err = st.HClient.Do(req)
		if err != nil {
			return fmt.Errorf("hClient.Do: %w", err)
		}
		respText, err = gokhttp_responses.ResponseText(resp)
		if err != nil {
			return fmt.Errorf("responses.ResponseBytes: %w", err)
		}
		playList, err = m3u8.ReadString(respText)
		if err != nil {
			return fmt.Errorf("m3u8.ReadString: %w", err)
		}

		if !playList.IsMaster() {
			break
		}

		// Select highest bandwidth variant
		targetChunkStream := &m3u8.PlaylistItem{Bandwidth: 0}
		for _, chunkStream := range playList.Playlists() {
			if targetChunkStream.Bandwidth < chunkStream.Bandwidth {
				targetChunkStream = chunkStream
			}
		}

		if targetChunkStream.Bandwidth == 0 {
			return errors.New("stream can't have 0 bandwidth")
		}

		// Discover separate audio stream
		if targetChunkStream.Audio != nil {
			audioMedia := findAudioMediaItem(playList, *targetChunkStream.Audio)
			if audioMedia != nil {
				st.HasAudioStream = true
				audioURL := buildURLFromBase(st.BaseUrl, *audioMedia.URI)
				st.AudioPlaylistUrl = atomic.NewString(audioURL)

				// Compute audio base URL
				parsedAudioUrl, parseErr := url.Parse(audioURL)
				if parseErr == nil {
					audioPathSplit := strings.Split(parsedAudioUrl.Path, "/")
					st.AudioBaseUrl = &url.URL{
						Scheme:     parsedAudioUrl.Scheme,
						Opaque:     parsedAudioUrl.Opaque,
						User:       parsedAudioUrl.User,
						Host:       parsedAudioUrl.Host,
						Path:       strings.Join(audioPathSplit[:len(audioPathSplit)-1], "/"),
						OmitHost:   parsedAudioUrl.OmitHost,
						ForceQuery: parsedAudioUrl.ForceQuery,
						RawQuery:   parsedAudioUrl.RawQuery,
						Fragment:   parsedAudioUrl.Fragment,
					}
				}

				st.AudioSegmentChan = make(chan *m3u8.SegmentItem)
				st.AudioBufferChan = make(chan *bytes.Buffer)
				st.AudioSegmentCache = hashmap.New[string, time.Time]()
			}
		}

		playlistUrl = buildURLFromBase(st.BaseUrl, targetChunkStream.URI)
	}

	// Detect format: check for EXT-X-MAP (fMP4) in the media playlist
	mapItem := findMapItem(playList)
	if mapItem != nil {
		st.Format = StreamFormatFMP4
	} else {
		st.Format = StreamFormatTS
	}

	// Create output file and muxer
	if err = st.initOutputFile(); err != nil {
		return fmt.Errorf("initOutputFile: %w", err)
	}

	// For fMP4: download init segment
	if st.Format == StreamFormatFMP4 && mapItem != nil {
		// Compute base URL for media playlist
		parsedMediaUrl, parseErr := url.Parse(playlistUrl)
		if parseErr != nil {
			return fmt.Errorf("url.Parse media playlist: %w", parseErr)
		}
		mediaPathSplit := strings.Split(parsedMediaUrl.Path, "/")
		mediaBaseUrl := &url.URL{
			Scheme:     parsedMediaUrl.Scheme,
			Opaque:     parsedMediaUrl.Opaque,
			User:       parsedMediaUrl.User,
			Host:       parsedMediaUrl.Host,
			Path:       strings.Join(mediaPathSplit[:len(mediaPathSplit)-1], "/"),
			OmitHost:   parsedMediaUrl.OmitHost,
			ForceQuery: parsedMediaUrl.ForceQuery,
			RawQuery:   parsedMediaUrl.RawQuery,
			Fragment:   parsedMediaUrl.Fragment,
		}

		initData, dlErr := st.downloadInitSegment(ctx, mediaBaseUrl, mapItem.URI)
		if dlErr != nil {
			return fmt.Errorf("downloadInitSegment (video): %w", dlErr)
		}
		st.FMP4Muxer.SetInitSegment(initData)

		if st.SaveSegments {
			saveDir := filepath.Dir(st.FileLocation.Load())
			_ = os.WriteFile(filepath.Join(saveDir, "init_video.mp4"), initData, 0666)
		}

		// Download audio init segment if separate audio
		if st.HasAudioStream {
			audioPlaylistUrl := st.AudioPlaylistUrl.Load()
			audioReq, audioErr := gokhttp_requests.MakeGETRequest(ctx, audioPlaylistUrl, st.Opts...)
			if audioErr != nil {
				return fmt.Errorf("audio requests.MakeGETRequest: %w", audioErr)
			}
			audioResp, audioErr := st.HClient.Do(audioReq)
			if audioErr != nil {
				return fmt.Errorf("audio hClient.Do: %w", audioErr)
			}
			audioText, audioErr := gokhttp_responses.ResponseText(audioResp)
			if audioErr != nil {
				return fmt.Errorf("audio responses.ResponseText: %w", audioErr)
			}
			audioPlaylist, audioErr := m3u8.ReadString(audioText)
			if audioErr != nil {
				return fmt.Errorf("audio m3u8.ReadString: %w", audioErr)
			}

			audioMapItem := findMapItem(audioPlaylist)
			if audioMapItem != nil {
				audioInitData, audioInitErr := st.downloadInitSegment(ctx, st.AudioBaseUrl, audioMapItem.URI)
				if audioInitErr != nil {
					return fmt.Errorf("downloadInitSegment (audio): %w", audioInitErr)
				}
				st.FMP4Muxer.SetAudioInitSegment(audioInitData)

				if st.SaveSegments {
					saveDir := filepath.Dir(st.FileLocation.Load())
					_ = os.WriteFile(filepath.Join(saveDir, "init_audio.mp4"), audioInitData, 0666)
				}
			}
		}
	}

	// Launch audio segment polling goroutine
	if st.HasAudioStream {
		st.audioWg.Add(1)
		go func() {
			defer st.audioWg.Done()
			st.getAudioSegments(ctx)
		}()
	}

	// Video segment polling loop
	ticker := time.Tick(time.Second)
	for {
		select {
		case <-ticker:
			req, err = gokhttp_requests.MakeGETRequest(ctx, playlistUrl, st.Opts...)
			if err != nil {
				continue
			}
			resp, err = st.HClient.Do(req)
			if err != nil {
				continue
			}
			respText, err = gokhttp_responses.ResponseText(resp)
			if err != nil {
				continue
			}
			playList, err = m3u8.ReadString(respText)
			if err != nil {
				continue
			}

			for _, chunk := range playList.Segments() {
				if st.Global.GraceFulStop.Load() {
					break
				}

				_, ok := st.SegmentCache.Get(chunk.Segment)
				if !ok {
					st.SegmentChan <- chunk
				}
			}

			// Stream ended: playlist has EXT-X-ENDLIST tag
			if !playList.IsLive() {
				break
			}
		}

		if st.Global.GraceFulStop.Load() {
			break
		}
	}

	return nil
}

func (st *StreamHLSTask) getAudioSegments(ctx context.Context) {
	playlistUrl := st.AudioPlaylistUrl.Load()
	ticker := time.Tick(time.Second)
	for {
		select {
		case <-ticker:
			req, err := gokhttp_requests.MakeGETRequest(ctx, playlistUrl, st.Opts...)
			if err != nil {
				continue
			}
			resp, err := st.HClient.Do(req)
			if err != nil {
				continue
			}
			respText, err := gokhttp_responses.ResponseText(resp)
			if err != nil {
				continue
			}
			playList, err := m3u8.ReadString(respText)
			if err != nil {
				continue
			}

			for _, chunk := range playList.Segments() {
				if st.Global.GraceFulStop.Load() {
					break
				}

				_, ok := st.AudioSegmentCache.Get(chunk.Segment)
				if !ok {
					st.AudioSegmentChan <- chunk
				}
			}

			if !playList.IsLive() {
				break
			}
		}

		if st.Global.GraceFulStop.Load() {
			break
		}
	}
}

func (st *StreamHLSTask) mergeSegments() error {
	if st.Format == StreamFormatFMP4 {
		return st.mergeFMP4Segments()
	}
	return st.mergeTSSegments()
}

func (st *StreamHLSTask) mergeTSSegments() error {
	if st.HasAudioStream {
		st.audioWg.Add(1)
		go func() {
			defer st.audioWg.Done()
			st.mergeTSAudioSegments()
		}()
	}

	isFirst := true
	ticker := time.Tick(time.Second)
	for {
		select {
		case <-ticker:
			break
		case newBuffer := <-st.BufferChan:
			if isFirst {
				err := st.Muxer.AddStreams(newBuffer)
				if err != nil {
					return fmt.Errorf("controller.Muxer.AddStreams: %w", err)
				}
				isFirst = false
			}
			err := st.Muxer.Demuxer.Input(newBuffer)
			if err != nil {
				return fmt.Errorf("controller.Muxer.Demuxer.Input: %w", err)
			}
			break
		}

		if st.Global.GraceFulStop.Load() {
			break
		}
	}

	return nil
}

func (st *StreamHLSTask) mergeTSAudioSegments() {
	ticker := time.Tick(time.Second)
	for {
		select {
		case <-ticker:
			break
		case newBuffer := <-st.AudioBufferChan:
			st.Muxer.InputSafe(newBuffer)
			break
		}

		if st.Global.GraceFulStop.Load() {
			break
		}
	}
}

func (st *StreamHLSTask) mergeFMP4Segments() error {
	if st.HasAudioStream {
		st.audioWg.Add(1)
		go func() {
			defer st.audioWg.Done()
			st.mergeFMP4AudioSegments()
		}()
	}

	ticker := time.Tick(time.Second)
	for {
		select {
		case <-ticker:
			break
		case newBuffer := <-st.BufferChan:
			err := st.FMP4Muxer.ProcessSegment(newBuffer.Bytes())
			if err != nil {
				return fmt.Errorf("FMP4Muxer.ProcessSegment: %w", err)
			}
			break
		}

		if st.Global.GraceFulStop.Load() {
			break
		}
	}

	return nil
}

func (st *StreamHLSTask) mergeFMP4AudioSegments() {
	ticker := time.Tick(time.Second)
	for {
		select {
		case <-ticker:
			break
		case newBuffer := <-st.AudioBufferChan:
			err := st.FMP4Muxer.ProcessAudioSegment(newBuffer.Bytes())
			if err != nil {
				continue
			}
			break
		}

		if st.Global.GraceFulStop.Load() {
			break
		}
	}
}

func (st *StreamHLSTask) downloadSegments(ctx context.Context) error {
	if st.HasAudioStream {
		st.audioWg.Add(1)
		go func() {
			defer st.audioWg.Done()
			st.downloadAudioSegments(ctx)
		}()
	}

	ticker := time.Tick(time.Second)
	for {
		select {
		case <-ticker:
			break
		case newChunk := <-st.SegmentChan:
			req, err := gokhttp_requests.MakeGETRequest(ctx, buildURLFromBase(st.BaseUrl, newChunk.Segment), st.Opts...)
			if err != nil {
				continue
			}
			resp, err := st.HClient.Do(req)
			if err != nil {
				continue
			}
			respBytes, err := gokhttp_responses.ResponseBytes(resp)
			if err != nil {
				continue
			}

			st.SegmentCache.Set(newChunk.Segment, time.Now())

			buf := bytes.NewBuffer(respBytes)

			st.TaskStats.DownloadedSegments.Inc()
			st.TaskStats.DownloadedBytes.Add(uint64(buf.Len()))
			st.TaskStats.DownloadedDuration.Add(int64(newChunk.Duration * 1500))
			st.Global.TotalBytes.Add(uint64(buf.Len()))
			st.Global.DownloadedBytes.Add(uint64(buf.Len()))

			st.BufferChan <- buf
			if st.SaveSegments {
				fileLocation := filepath.Dir(st.FileLocation.Load())
				f, err := os.OpenFile(filepath.Join(fileLocation, filepath.Base(newChunk.Segment)), os.O_CREATE|os.O_WRONLY, 0666)
				if err != nil {
					return fmt.Errorf("os.OpenFile: %w", err)
				}
				_, err = f.Write(respBytes)
				if err != nil {
					f.Close()
					return fmt.Errorf("f.Write: %w", err)
				}
				f.Close()
			}
			break
		}

		if st.Global.GraceFulStop.Load() {
			break
		}
	}

	return nil
}

func (st *StreamHLSTask) downloadAudioSegments(ctx context.Context) {
	baseUrl := st.AudioBaseUrl
	if baseUrl == nil {
		baseUrl = st.BaseUrl
	}

	ticker := time.Tick(time.Second)
	for {
		select {
		case <-ticker:
			break
		case newChunk := <-st.AudioSegmentChan:
			req, err := gokhttp_requests.MakeGETRequest(ctx, buildURLFromBase(baseUrl, newChunk.Segment), st.Opts...)
			if err != nil {
				continue
			}
			resp, err := st.HClient.Do(req)
			if err != nil {
				continue
			}
			respBytes, err := gokhttp_responses.ResponseBytes(resp)
			if err != nil {
				continue
			}

			st.AudioSegmentCache.Set(newChunk.Segment, time.Now())

			buf := bytes.NewBuffer(respBytes)

			st.TaskStats.DownloadedSegments.Inc()
			st.TaskStats.DownloadedBytes.Add(uint64(buf.Len()))
			st.TaskStats.DownloadedDuration.Add(int64(newChunk.Duration * 1500))
			st.Global.TotalBytes.Add(uint64(buf.Len()))
			st.Global.DownloadedBytes.Add(uint64(buf.Len()))

			st.AudioBufferChan <- buf
			if st.SaveSegments {
				fileLocation := filepath.Dir(st.FileLocation.Load())
				f, err := os.OpenFile(filepath.Join(fileLocation, "audio_"+filepath.Base(newChunk.Segment)), os.O_CREATE|os.O_WRONLY, 0666)
				if err != nil {
					continue
				}
				_, err = f.Write(respBytes)
				f.Close()
			}
			break
		}

		if st.Global.GraceFulStop.Load() {
			break
		}
	}
}

func buildURLFromBase(baseUrl *url.URL, path string) string {
	ref, err := url.Parse(path)
	if err != nil {
		return path
	}
	return baseUrl.ResolveReference(ref).String()
}
