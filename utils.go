package gokhttp_download

import (
	"encoding/json"
	"fmt"
	"github.com/cornelk/hashmap"
	"go.uber.org/atomic"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func supportsRange(resp *http.Response) (uint64, bool) {
	supportsRanges := false
	contentLength := uint64(0)
	if resp != nil {
		if resp.Request.Method == http.MethodHead && resp.StatusCode == http.StatusOK {
			if resp.Header.Get("Accept-Ranges") == "bytes" {
				supportsRanges = true
			}
			if resp.Header.Get("Ranges-Supported") == "bytes" {
				supportsRanges = true
			}
			contentLength = uint64(resp.ContentLength)
		} else if resp.Request.Method == http.MethodGet && resp.StatusCode == http.StatusPartialContent {
			contentRange := resp.Header.Get("Content-Range")
			if contentRange != "" {
				supportsRanges = true
			}
			cLSplit := strings.Split(contentRange, "/")
			if len(cLSplit) == 2 {
				contentLength, _ = strconv.ParseUint(cLSplit[1], 10, 64)
			}
		} else {
			// File inaccessible
		}
	}
	return contentLength, supportsRanges
}

// TaskMap wraps a hashmap.Map with a custom JSON marshaler
type TaskMap[K string, V DownloadTask] struct {
	*hashmap.Map[K, V]
}

// MarshalJSON implements the json.Marshaler interface for CustomMap
func (m *TaskMap[K, V]) MarshalJSON() ([]byte, error) {
	tmpMap := make(map[K]V)
	m.Range(func(k K, v V) bool {
		tmpMap[k] = v
		return true
	})
	return json.Marshal(tmpMap)
}

func (m *TaskMap[K, V]) UnmarshalJSON(data []byte) error {
	tmpMap := make(map[K]V)

	err := json.Unmarshal(data, &tmpMap)
	if err != nil {
		return fmt.Errorf("json.Unmarshal: %w", err)
	}

	for k, v := range tmpMap {
		m.Set(k, v)
	}
	return nil
}

// CustomTime wraps a *atomic.Time with a custom JSON marshaler
type CustomTime struct {
	*atomic.Time
}

func NewCustomTime(t time.Time) *CustomTime {
	return &CustomTime{atomic.NewTime(t)}
}

// MarshalJSON implements the json.Marshaler interface for CustomMap
func (t *CustomTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.Load())
}

func (t *CustomTime) UnmarshalJSON(data []byte) error {
	tmpTime := time.Time{}
	err := json.Unmarshal(data, &tmpTime)
	if err != nil {
		return fmt.Errorf("json.Unmarshal: %w", err)
	}
	t.Store(tmpTime)
	return nil
}
