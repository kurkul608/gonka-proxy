package proxy

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"time"
)

// usageTailBytes is how much of the end of a response body is kept to find
// the upstream usage. OpenAI-compatible servers put "usage" at the end of a
// completion object and in the last chunk of a stream.
const usageTailBytes = 16 * 1024

var completionTokensField = []byte(`"completion_tokens"`)

// meteredBody wraps an upstream response body to time the first byte, keep
// the tail for usage parsing and remember a read error.
type meteredBody struct {
	body      io.Reader
	firstByte time.Time
	tail      []byte
	readErr   error
}

func newMeteredBody(body io.Reader) *meteredBody {
	return &meteredBody{body: body}
}

func (m *meteredBody) Read(p []byte) (int, error) {
	n, err := m.body.Read(p)
	if n > 0 {
		if m.firstByte.IsZero() {
			m.firstByte = time.Now()
		}
		m.keep(p[:n])
	}
	if err != nil && !errors.Is(err, io.EOF) && m.readErr == nil {
		m.readErr = err
	}
	return n, err
}

func (m *meteredBody) keep(p []byte) {
	if len(p) >= usageTailBytes {
		m.tail = append(m.tail[:0], p[len(p)-usageTailBytes:]...)
		return
	}
	if overflow := len(m.tail) + len(p) - usageTailBytes; overflow > 0 {
		m.tail = append(m.tail[:0], m.tail[overflow:]...)
	}
	m.tail = append(m.tail, p...)
}

// completionTokens returns the last "completion_tokens" number in the kept
// tail, or 0. A string value inside the content cannot match: its quotes are
// escaped.
func (m *meteredBody) completionTokens() int {
	return lastCompletionTokens(m.tail)
}

func lastCompletionTokens(data []byte) int {
	for end := len(data); end > 0; {
		index := bytes.LastIndex(data[:end], completionTokensField)
		if index < 0 {
			return 0
		}
		end = index
		rest := bytes.TrimLeft(data[index+len(completionTokensField):], " \t\r\n")
		if len(rest) == 0 || rest[0] != ':' {
			continue
		}
		rest = bytes.TrimLeft(rest[1:], " \t\r\n")
		digits := 0
		for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits == 0 || digits == len(rest) {
			continue // not a number, or cut at the end of the data
		}
		value, err := strconv.Atoi(string(rest[:digits]))
		if err != nil || value <= 0 {
			continue
		}
		return value
	}
	return 0
}

// tokensPerSecond is the output rate of a successful attempt. Streams are
// measured from the first byte (generation time); a non-streaming body
// arrives at once, so it is measured from the start of the attempt.
func (m *meteredBody) tokensPerSecond(started, finished time.Time, streaming bool) float64 {
	tokens := m.completionTokens()
	if tokens <= 0 {
		return 0
	}
	from := started
	if streaming && !m.firstByte.IsZero() && finished.Sub(m.firstByte) >= time.Millisecond {
		from = m.firstByte
	}
	elapsed := finished.Sub(from)
	if elapsed <= 0 {
		return 0
	}
	return float64(tokens) / elapsed.Seconds()
}
