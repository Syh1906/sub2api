package service

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type audioIdleReader struct {
	body     io.Reader
	timer    *time.Timer
	interval time.Duration
}

func (r *audioIdleReader) Read(buf []byte) (int, error) {
	r.timer.Reset(r.interval)
	size, err := r.body.Read(buf)
	if !r.timer.Stop() {
		return 0, fmt.Errorf("upstream audio read timed out")
	}
	return size, err
}

func nativeAudioMedia(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	return err == nil && (strings.HasPrefix(media, "audio/") || media == "application/octet-stream")
}

func (s *OpenAIGatewayService) streamNativeAudio(c *gin.Context, resp *http.Response, result *OpenAIForwardResult, binary bool, start time.Time) (*OpenAIForwardResult, error) {
	contentType := resp.Header.Get("Content-Type")
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil || (binary && !nativeAudioMedia(contentType)) || (!binary && media != "text/event-stream") {
		return nil, fmt.Errorf("unexpected upstream audio stream content type")
	}

	var body io.Reader = resp.Body
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		interval := time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
		timer := time.AfterFunc(interval, func() { _ = resp.Body.Close() })
		defer timer.Stop()
		body = &audioIdleReader{body: resp.Body, timer: timer, interval: interval}
	}
	started, disconnected := false, false
	deliver := func(data []byte) {
		if disconnected {
			return
		}
		if !started {
			started = true
			elapsed := int(time.Since(start).Milliseconds())
			result.FirstTokenMs = &elapsed
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
			c.Writer.Header().Del("Content-Length")
			c.Header("Content-Type", contentType)
			c.Header("Cache-Control", "no-cache")
			c.Header("X-Accel-Buffering", "no")
			c.Status(resp.StatusCode)
		}
		written, writeErr := c.Writer.Write(data)
		disconnected = writeErr != nil || written != len(data)
		if !disconnected {
			c.Writer.Flush()
		}
	}

	if binary {
		buf := make([]byte, 32*1024)
		for {
			size, readErr := body.Read(buf)
			if size > 0 {
				deliver(buf[:size])
			}
			if readErr == nil {
				continue
			}
			if readErr == io.EOF && started {
				break
			}
			if started && !disconnected {
				return nil, fmt.Errorf("%w: truncated upstream audio", http.ErrAbortHandler)
			}
			return nil, fmt.Errorf("incomplete upstream audio: %w", readErr)
		}
	} else {
		terminal := "transcript.text.done"
		if result.UpstreamEndpoint == "/v1/audio/speech" {
			terminal = "speech.audio.done"
		}
		scanner := s.newUpstreamSSEScanner(body)
		var parser openAICompatSSEFrameParser
		var event strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = event.WriteString(line)
			_ = event.WriteByte('\n')
			frame, hasData := parser.AddLine(line)
			if line != "" {
				continue
			}
			payload := gjson.Parse(frame.Data)
			eventType := payload.Get("type").String()
			failed := frame.EventType == "error" || eventType == "error" || payload.Get("error").Exists()
			complete := hasData && gjson.Valid(frame.Data) && payload.IsObject() &&
				(eventType == terminal || (eventType == "" && frame.EventType == terminal)) &&
				(frame.EventType == "" || frame.EventType == "message" || frame.EventType == terminal)
			deliver([]byte(event.String()))
			event.Reset()
			if failed {
				return nil, fmt.Errorf("upstream audio stream reported an error")
			}
			if complete {
				result.Duration = time.Since(start)
				return result, nil
			}
		}
		if started && !disconnected {
			_, _ = c.Writer.WriteString("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"upstream_error\",\"message\":\"Incomplete upstream audio stream\"}}\n\n")
			c.Writer.Flush()
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read upstream audio stream: %w", err)
		}
		return nil, fmt.Errorf("upstream audio stream ended before %s", terminal)
	}
	result.Duration = time.Since(start)
	return result, nil
}
