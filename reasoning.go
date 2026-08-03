package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// The models stream their chain of thought in `delta.reasoning`, which
// measyai.Chunk does not model — it carries only role, content and tool_calls,
// so the SDK's typed frames drop it on the floor.
//
// Rather than hand-rolling a second HTTP client to see it, the SDK is given a
// transport that wraps the response body. Every byte still reaches the SDK
// untouched and is parsed by it as usual; the wrapper only sniffs the lines on
// their way past.

// reasoningSink receives reasoning text as it streams. The agent runs one
// generation at a time, so a single package-level hook is enough.
var reasoningSink func(string)

type reasoningTap struct {
	inner io.ReadCloser
	buf   []byte
}

func (t *reasoningTap) Read(p []byte) (int, error) {
	n, err := t.inner.Read(p)
	if n > 0 && reasoningSink != nil {
		t.buf = append(t.buf, p[:n]...)
		for {
			i := bytes.IndexByte(t.buf, '\n')
			if i < 0 {
				break
			}
			line := t.buf[:i]
			t.buf = t.buf[i+1:]
			if text := reasoningOf(line); text != "" {
				reasoningSink(text)
			}
		}
		// A frame that never terminates must not grow without bound.
		if len(t.buf) > 1<<20 {
			t.buf = t.buf[:0]
		}
	}
	return n, err
}

func (t *reasoningTap) Close() error { return t.inner.Close() }

// reasoningOf pulls the reasoning fragment out of one SSE line, if it has one.
func reasoningOf(line []byte) string {
	data, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !found {
		return ""
	}
	data = bytes.TrimSpace(data)
	// Cheap reject before paying for a JSON decode: most frames have no
	// reasoning at all, and this runs on every line of every stream.
	if !bytes.Contains(data, []byte(`"reasoning"`)) {
		return ""
	}

	var frame struct {
		Choices []struct {
			Delta struct {
				Reasoning string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &frame); err != nil || len(frame.Choices) == 0 {
		return ""
	}
	return frame.Choices[0].Delta.Reasoning
}

// tappingTransport installs the tap on every streamed response.
type tappingTransport struct{ base http.RoundTripper }

func (t tappingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &reasoningTap{inner: resp.Body}
	return resp, nil
}
