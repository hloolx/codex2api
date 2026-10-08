package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const pendingFirstTokenFlushBytes = 1024 * 1024

var (
	sseDataPrefix = []byte("data: ")
	sseDataSuffix = []byte("\n\n")
)

type streamFlushWriter struct {
	writer        io.Writer
	flusher       http.Flusher
	policy        string
	interval      time.Duration
	lastFlush     time.Time
	buffer        bytes.Buffer
	outputScanner *promptfilter.OutputScanner
	// scanCarry 暂存扫描器已放行但尚未凑满一个完整 SSE 事件的尾巴。扫描器按
	// 字节数扣安全窗,切点会落在事件正中间;若把半个事件写到底层,任何绕过本
	// 写入器直写 ResponseWriter 的字节(请求级保活注释、续想心跳)都会插进
	// JSON 里(issue #696)。这里把底层写入对齐到事件边界,保证底层流永远停在
	// 两个事件之间。
	scanCarry    []byte
	writtenBytes atomic.Int64
	diag         *streamPhaseDiagnostics
}

// Small buffers are reused across token events; an exceptional large event
// must not keep its allocation alive for the rest of a long stream.
func (w *streamFlushWriter) resetBuffer() {
	if w.buffer.Cap() > 256<<10 {
		w.buffer = bytes.Buffer{}
	} else {
		w.buffer.Reset()
	}
}

// streamPhaseDiagnostics 采集流阶段的断流现场判据。
//
// 上游 RST_STREAM（如 "INTERNAL_ERROR; received from peer"）有两类互斥成因，
// 事后只看错误文本无法区分（issue #491）：
//   - 下游背压：SSE 是在读上游的回调里同步写回下游的，客户端读得慢会让
//     body.Read 停摆，HTTP/2 流控窗口不再补充，上游边缘可能因此重置该流。
//     此时 writeBlockedNanos 会显著偏大。
//   - 上游自身：后端被回收/长推理期间安静，边缘直接重置。此时下游写阻塞
//     接近 0，而距上游末帧的间隔偏大。
//
// 两个计数器随断流信息一并落到用量日志，运维在错误明细页即可判读，无需加库表。
type streamPhaseDiagnostics struct {
	writeBlockedNanos   atomic.Int64
	lastUpstreamFrameAt atomic.Int64 // UnixNano
	upstreamFrames      atomic.Int64
}

func newStreamPhaseDiagnostics() *streamPhaseDiagnostics {
	return &streamPhaseDiagnostics{}
}

// annotateStreamBreakDiagnostics 只给断流类失败追加现场判据。客户端主动断开
// （499）与上游显式 response.failed 有各自明确的成因，不需要这两个数字。
func annotateStreamBreakDiagnostics(outcome streamOutcome, diag *streamPhaseDiagnostics) streamOutcome {
	if outcome.logStatusCode != logStatusUpstreamStreamBreak {
		return outcome
	}
	summary := diag.summary()
	if summary == "" {
		return outcome
	}
	outcome.failureMessage = strings.TrimSpace(strings.TrimSpace(outcome.failureMessage) + " " + summary)
	return outcome
}

// markUpstreamFrame 在每个上游 SSE 事件到达时打点。
func (d *streamPhaseDiagnostics) markUpstreamFrame() {
	if d == nil {
		return
	}
	d.lastUpstreamFrameAt.Store(time.Now().UnixNano())
	d.upstreamFrames.Add(1)
}

func (d *streamPhaseDiagnostics) addWriteBlocked(start time.Time) {
	if d == nil || start.IsZero() {
		return
	}
	d.writeBlockedNanos.Add(int64(time.Since(start)))
}

// summary 生成人类可读的现场判据；无任何上游帧时返回空串（连响应头都没读到，
// 与背压无关，加了反而噪声）。
func (d *streamPhaseDiagnostics) summary() string {
	if d == nil {
		return ""
	}
	frames := d.upstreamFrames.Load()
	if frames == 0 {
		return ""
	}
	blockedMs := time.Duration(d.writeBlockedNanos.Load()).Milliseconds()
	sinceLastMs := int64(-1)
	if last := d.lastUpstreamFrameAt.Load(); last > 0 {
		sinceLastMs = time.Since(time.Unix(0, last)).Milliseconds()
	}
	if sinceLastMs < 0 {
		return fmt.Sprintf("[上游帧 %d, 下游写阻塞 %dms]", frames, blockedMs)
	}
	return fmt.Sprintf("[上游帧 %d, 下游写阻塞 %dms, 距上游末帧 %dms]", frames, blockedMs, sinceLastMs)
}

func (h *Handler) newStreamFlushWriter(c *gin.Context, writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	w := newStreamFlushWriter(writer, flusher)
	if h != nil && h.store != nil {
		cfg := h.promptFilterConfigForRequest(c)
		if cfg.Enabled && cfg.Advanced.Output.Enabled {
			w.outputScanner = promptfilter.NewOutputScannerFromNormalizedConfig(cfg)
		}
	}
	return w
}

// newAttemptStreamFlushWriter keeps a continuously retried attempt free of
// local output-policy side effects until the upstream terminal is known. The
// winning attempt is scanned exactly once by commitStreamAttempt below.
func (h *Handler) newAttemptStreamFlushWriter(c *gin.Context, attempt *continuousRetryStreamAttempt, writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	if attempt != nil {
		return newStreamFlushWriter(attempt.writerOr(writer), attempt.flusherOr(flusher))
	}
	return h.newStreamFlushWriter(c, writer, flusher)
}

type streamFlushWriterAdapter struct {
	writer *streamFlushWriter
}

func (a streamFlushWriterAdapter) Write(data []byte) (int, error) {
	if a.writer == nil {
		return 0, io.ErrClosedPipe
	}
	if err := a.writer.WriteBytes(data); err != nil {
		return 0, err
	}
	return len(data), nil
}

// commitStreamAttempt applies the request's output policy only after the
// attempt has reached a successful protocol terminal. A filter or downstream
// write failure is local and must never trigger another upstream request.
func (h *Handler) commitStreamAttempt(c *gin.Context, attempt *continuousRetryStreamAttempt) error {
	if attempt == nil {
		return nil
	}
	if attempt.closed || attempt.replay == nil {
		return errContinuousRetryReplayClosed
	}
	flusher, _ := c.Writer.(http.Flusher)
	filtered := h.newStreamFlushWriter(c, c.Writer, flusher)
	if err := attempt.replay.CommitTo(streamFlushWriterAdapter{writer: filtered}, nil); err != nil {
		return err
	}
	return filtered.Finalize()
}

func (w *streamFlushWriter) scanOutput(data []byte) ([]byte, error) {
	if w == nil || w.outputScanner == nil {
		return data, nil
	}
	released, err := w.outputScanner.Push(data)
	if err != nil {
		// 扫描器判定拦截时会丢弃自己的安全窗;尚未落地的尾巴同样不能再出去。
		w.scanCarry = nil
		return nil, err
	}
	return w.alignToEventBoundary(released), nil
}

// alignToEventBoundary 把扫描器放行的字节与之前暂存的尾巴拼接后,只返回到最后
// 一个事件分隔符(空行)为止的部分,余下的半个事件继续暂存。没有完整事件时返回
// nil,调用方视同扫描器仍在扣留。多扣一点只会让输出晚到,绝不会放出扫描器还没
// 看过的字节,所以安全窗语义不变。
func (w *streamFlushWriter) alignToEventBoundary(released []byte) []byte {
	if len(released) == 0 && len(w.scanCarry) == 0 {
		return nil
	}
	w.scanCarry = append(w.scanCarry, released...)
	idx := bytes.LastIndex(w.scanCarry, sseDataSuffix)
	if idx < 0 {
		return nil
	}
	cut := idx + len(sseDataSuffix)
	out := append([]byte(nil), w.scanCarry[:cut]...)
	rest := w.scanCarry[cut:]
	if len(rest) == 0 {
		w.scanCarry = w.scanCarry[:0]
	} else {
		w.scanCarry = append(w.scanCarry[:0], rest...)
	}
	return out
}

func newStreamFlushWriter(writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	settings := CurrentRuntimeSettings()
	return &streamFlushWriter{
		writer:   writer,
		flusher:  flusher,
		policy:   settings.StreamFlushPolicy,
		interval: currentStreamFlushInterval(),
	}
}

func appendSSEData(buf *bytes.Buffer, data []byte) {
	if buf == nil {
		return
	}
	buf.Write(sseDataPrefix)
	buf.Write(data)
	buf.Write(sseDataSuffix)
}

func writeDeferredSSEData(streamWriter *streamFlushWriter, pending *bytes.Buffer, data []byte, shouldDefer bool) (bool, error) {
	if streamWriter == nil {
		return false, nil
	}
	if shouldDefer {
		appendSSEData(pending, data)
		if pending != nil && pending.Len() <= pendingFirstTokenFlushBytes {
			return false, nil
		}
	}
	if pending != nil && pending.Len() > 0 {
		if !shouldDefer {
			appendSSEData(pending, data)
		}
		before := streamWriter.deliveredBytes()
		if err := streamWriter.WriteBytes(pending.Bytes()); err != nil {
			return false, err
		}
		pending.Reset()
		return streamWriter.deliveredBytes() > before, nil
	}
	if shouldDefer {
		return false, nil
	}
	before := streamWriter.deliveredBytes()
	if err := streamWriter.WriteSSEData(data); err != nil {
		return false, err
	}
	return streamWriter.deliveredBytes() > before, nil
}

func (w *streamFlushWriter) deliveredBytes() int64 {
	if w == nil {
		return 0
	}
	return w.writtenBytes.Load()
}

func (w *streamFlushWriter) writeUnderlying(data []byte) error {
	if w == nil || w.writer == nil || len(data) == 0 {
		return nil
	}
	blockStart := w.diagnosticsClock()
	written, err := w.writer.Write(data)
	w.diag.addWriteBlocked(blockStart)
	if written > 0 {
		w.writtenBytes.Add(int64(written))
	}
	return err
}

func (w *streamFlushWriter) writeUnderlyingString(data string) error {
	if w == nil || w.writer == nil || data == "" {
		return nil
	}
	blockStart := w.diagnosticsClock()
	written, err := io.WriteString(w.writer, data)
	w.diag.addWriteBlocked(blockStart)
	if written > 0 {
		w.writtenBytes.Add(int64(written))
	}
	return err
}

// diagnosticsClock 仅在挂了诊断器时取时间戳，未开启时零开销。
func (w *streamFlushWriter) diagnosticsClock() time.Time {
	if w == nil || w.diag == nil {
		return time.Time{}
	}
	return time.Now()
}

func (w *streamFlushWriter) WriteString(data string) error {
	if w == nil || w.writer == nil {
		return nil
	}
	filtered, err := w.scanOutput([]byte(data))
	if err != nil || len(filtered) == 0 {
		return err
	}
	data = string(filtered)
	if w.policy != StreamFlushPolicyCoalesce {
		if err := w.writeUnderlyingString(data); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if _, err := w.buffer.WriteString(data); err != nil {
		return err
	}
	if w.lastFlush.IsZero() || time.Since(w.lastFlush) >= w.interval {
		return w.Flush()
	}
	return nil
}

func (w *streamFlushWriter) WriteBytes(data []byte) error {
	if w == nil || w.writer == nil || len(data) == 0 {
		return nil
	}
	var err error
	data, err = w.scanOutput(data)
	if err != nil || len(data) == 0 {
		return err
	}
	if w.policy != StreamFlushPolicyCoalesce {
		if err := w.writeUnderlying(data); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if _, err := w.buffer.Write(data); err != nil {
		return err
	}
	if w.lastFlush.IsZero() || time.Since(w.lastFlush) >= w.interval {
		return w.Flush()
	}
	return nil
}

func (w *streamFlushWriter) WriteSSEData(data []byte) error {
	if w == nil || w.writer == nil {
		return nil
	}
	framed := make([]byte, 0, len(sseDataPrefix)+len(data)+len(sseDataSuffix))
	framed = append(framed, sseDataPrefix...)
	framed = append(framed, data...)
	framed = append(framed, sseDataSuffix...)
	var err error
	framed, err = w.scanOutput(framed)
	if err != nil || len(framed) == 0 {
		return err
	}
	if w.policy != StreamFlushPolicyCoalesce {
		if err := w.writeUnderlying(framed); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	w.buffer.Write(framed)
	if w.lastFlush.IsZero() || time.Since(w.lastFlush) >= w.interval {
		return w.Flush()
	}
	return nil
}

// WriteSSEComment 写一条 SSE 注释(如 ": keepalive\n\n")并立即冲刷传输。
// 注释不是模型输出,不进扫描器:先排空合并缓冲(里面只有完整事件)再直写底层。
// 输出过滤开启时,scanOutput 已把底层写入对齐到事件边界(见 scanCarry),
// 底层流不会停在半个事件里,直写是安全的;若改走扫描器,注释会和安全窗一起
// 被扣到下一个终态帧,上游长时间静默时保活就完全失效。
func (w *streamFlushWriter) WriteSSEComment(comment string) error {
	if w == nil || w.writer == nil || comment == "" {
		return nil
	}
	if w.buffer.Len() > 0 {
		if err := w.writeUnderlying(w.buffer.Bytes()); err != nil {
			return err
		}
		w.resetBuffer()
	}
	if err := w.writeUnderlyingString(comment); err != nil {
		return err
	}
	w.flushTransport()
	return nil
}

func (w *streamFlushWriter) Flush() error {
	if w == nil {
		return nil
	}
	if w.buffer.Len() > 0 {
		if err := w.writeUnderlying(w.buffer.Bytes()); err != nil {
			return err
		}
		w.resetBuffer()
	}
	if w.outputScanner != nil {
		pending, err := w.outputScanner.Flush()
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			if err := w.writeUnderlying(pending); err != nil {
				return err
			}
		}
	}
	w.flushTransport()
	return nil
}

// Finalize releases the retained safety window at a real semantic end-of-stream.
// A transport Flush must not call this because an unsafe phrase may span chunks.
func (w *streamFlushWriter) Finalize() error {
	if w == nil {
		return nil
	}
	if w.buffer.Len() > 0 {
		if err := w.writeUnderlying(w.buffer.Bytes()); err != nil {
			return err
		}
		w.resetBuffer()
	}
	if w.outputScanner != nil {
		pending, err := w.outputScanner.Finalize()
		if err != nil {
			w.scanCarry = nil
			return err
		}
		// 流真正结束:先补齐暂存的半个事件,再写扫描器释放的安全窗。
		if len(w.scanCarry) > 0 {
			if err := w.writeUnderlying(w.scanCarry); err != nil {
				return err
			}
			w.scanCarry = nil
		}
		if len(pending) > 0 {
			if err := w.writeUnderlying(pending); err != nil {
				return err
			}
		}
	}
	w.flushTransport()
	return nil
}

func (w *streamFlushWriter) flushTransport() {
	if w == nil || w.flusher == nil {
		return
	}
	w.flusher.Flush()
	w.lastFlush = time.Now()
}
