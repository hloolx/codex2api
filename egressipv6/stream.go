package egressipv6

import (
	"bytes"
	"io"

	"github.com/tidwall/gjson"
)

// Observe failures after output without withholding or replaying any bytes.
// Parsing memory is bounded even for a single oversized line or event.
type observedSSEBody struct {
	io.ReadCloser
	line, data                    []byte
	oversizedLine, oversizedEvent bool
	terminal, reported            bool
	retry5xx                      bool
	onFailure                     func(string)
}

func (b *observedSSEBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	for _, c := range p[:n] {
		if c != '\n' {
			if len(b.line) < 64*1024 && !b.oversizedLine {
				b.line = append(b.line, c)
			} else {
				b.line = nil
				b.oversizedLine = true
				b.oversizedEvent = true
			}
			continue
		}
		if b.oversizedLine {
			b.oversizedLine = false
			b.line = nil
			continue
		}
		line := bytes.TrimSpace(b.line)
		if len(line) == 0 {
			if !b.oversizedEvent {
				b.event()
			}
			b.data = nil
			b.oversizedEvent = false
		} else if bytes.HasPrefix(line, []byte("data:")) && !b.oversizedEvent {
			part := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(b.data)+len(part)+1 > 64*1024 {
				b.oversizedEvent = true
				b.data = nil
			} else {
				if len(b.data) > 0 {
					b.data = append(b.data, '\n')
				}
				b.data = append(b.data, part...)
			}
		}
		b.line = b.line[:0]
	}
	if err != nil && !b.terminal && !b.reported {
		b.report("stream_disconnected")
	}
	return n, err
}
func (b *observedSSEBody) event() {
	if b.reported {
		return
	}
	typ := gjson.GetBytes(b.data, "type").String()
	if typ == "response.completed" || typ == "response.done" || typ == "response.failed" || typ == "error" || bytes.Equal(b.data, []byte("[DONE]")) {
		b.terminal = true
	}
	if reason := FrameFailure(b.data, b.retry5xx); reason != "" {
		b.report(reason)
	}
}
func (b *observedSSEBody) report(reason string) {
	b.reported = true
	if b.onFailure != nil {
		b.onFailure(reason)
	}
}
