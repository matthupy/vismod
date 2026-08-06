package frames

import (
	"bytes"
	"strconv"
)

// stderrLimits bound what an ffmpeg run may hold in memory.
//
// The shipped workflows put `showinfo` in the filter graph, which logs one
// verbose line per decoded frame. Buffering all of that meant a long or
// high-fps input held hundreds of MB of chatter — times frames.concurrency
// concurrent extractions — and the process died mid-extraction without
// naming the asset that caused it.
const (
	// stderrTailBytes is kept for error reporting. The error message shows
	// the last 400 bytes; the rest is headroom for context above it.
	stderrTailBytes = 8 << 10
	// stderrLineMax caps a single unterminated line, so output with no
	// newlines cannot grow the partial-line buffer without bound.
	stderrLineMax = 64 << 10
)

// stderrScanner is the io.Writer attached to ffmpeg's stderr. It extracts
// what the pipeline actually needs as the bytes arrive and keeps only a
// rolling tail of the rest.
//
// Two consumers read this stream and they need different things: the error
// path wants the LAST few lines, while collect needs the showinfo pts_time
// of EVERY frame, in order. A plain tail buffer would serve the first and
// silently break the second — timestamps for early frames would vanish and
// each frame would fall back to its ordinal, which looks like working code
// and produces wrong timeline data. So timestamps are parsed on the fly
// (bounded by frame count, itself bounded by the extraction budget) and
// only the tail of the raw text is retained.
type stderrScanner struct {
	timestamps []float64
	tail       []byte // rolling last stderrTailBytes
	partial    []byte // bytes since the last newline
}

func (s *stderrScanner) Write(p []byte) (int, error) {
	n := len(p)
	s.appendTail(p)

	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendPartial(p)
			return n, nil
		}
		line := p[:i]
		if len(s.partial) > 0 {
			s.appendPartial(line)
			line = s.partial
		}
		s.scanLine(line)
		s.partial = s.partial[:0]
		p = p[i+1:]
	}
}

func (s *stderrScanner) scanLine(line []byte) {
	m := showinfoRe.FindSubmatch(line)
	if m == nil {
		return
	}
	if v, err := strconv.ParseFloat(string(m[1]), 64); err == nil {
		s.timestamps = append(s.timestamps, v)
	}
}

func (s *stderrScanner) appendPartial(b []byte) {
	if len(s.partial) >= stderrLineMax {
		return // a single pathological line: keep the head, drop the rest
	}
	if room := stderrLineMax - len(s.partial); len(b) > room {
		b = b[:room]
	}
	s.partial = append(s.partial, b...)
}

// appendTail keeps the last stderrTailBytes of everything written.
func (s *stderrScanner) appendTail(p []byte) {
	if len(p) >= stderrTailBytes {
		s.tail = append(s.tail[:0], p[len(p)-stderrTailBytes:]...)
		return
	}
	if over := len(s.tail) + len(p) - stderrTailBytes; over > 0 {
		s.tail = append(s.tail[:0], s.tail[over:]...)
	}
	s.tail = append(s.tail, p...)
}

// Flush scans any trailing unterminated line. ffmpeg's last line may not
// end in a newline, and it is the one most likely to explain a failure.
func (s *stderrScanner) Flush() {
	if len(s.partial) > 0 {
		s.scanLine(s.partial)
		s.partial = s.partial[:0]
	}
}

// Tail returns the retained tail as text.
func (s *stderrScanner) Tail() string { return string(s.tail) }

// Timestamps returns the pts_time of each frame, in emission order.
func (s *stderrScanner) Timestamps() []float64 { return s.timestamps }

// scanStderrText parses a whole stderr string. Only used by tests that
// already hold the text; production streams through Write.
func scanStderrText(text string) *stderrScanner {
	s := &stderrScanner{}
	_, _ = s.Write([]byte(text))
	s.Flush()
	return s
}
