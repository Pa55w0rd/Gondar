package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
)

const (
	maxOpenAIStreamFrameBytes = 1 << 20
	maxOpenAIStreamToolBytes  = 8 << 20
	maxOpenAIStreamTools      = 256
	maxOpenAIStreamChoices    = 16
)

// openAIStreamTransport preserves live text/usage, but withholds tool calls
// until a complete response boundary. The SDK consumes sequential tool blocks;
// wire deltas may instead interleave their arguments by choice/tool index.
// This adapter changes neither the request nor the SDK or other wire formats.
type openAIStreamTransport struct{ base http.RoundTripper }

func (t openAIStreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil || response.Body == nil || response.StatusCode < 200 || response.StatusCode >= 300 || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return response, err
	}
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if media != "text/event-stream" {
		return response, nil
	}
	response.Body = newOpenAIStreamBody(req.Context(), response.Body)
	response.ContentLength = -1
	response.Header = response.Header.Clone()
	response.Header.Del("Content-Length")
	response.TransferEncoding = nil
	return response, nil
}

type openAIStreamBody struct {
	reader       *io.PipeReader
	writer       *io.PipeWriter
	upstream     io.ReadCloser
	cancel       context.CancelFunc
	done         chan struct{}
	watchDone    chan struct{}
	stopOnce     sync.Once
	upstreamOnce sync.Once
}

func newOpenAIStreamBody(parent context.Context, upstream io.ReadCloser) *openAIStreamBody {
	ctx, cancel := context.WithCancel(parent)
	reader, writer := io.Pipe()
	body := &openAIStreamBody{reader: reader, writer: writer, upstream: upstream, cancel: cancel, done: make(chan struct{}), watchDone: make(chan struct{})}
	go func() {
		defer close(body.done)
		defer body.closeUpstream()
		err := normalizeOpenAIStream(ctx, upstream, writer)
		_ = writer.CloseWithError(err)
	}()
	go func() {
		defer close(body.watchDone)
		select {
		case <-ctx.Done():
			body.stop(ctx.Err())
		case <-body.done:
		}
	}()
	return body
}

func (b *openAIStreamBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *openAIStreamBody) closeUpstream() {
	b.upstreamOnce.Do(func() { _ = b.upstream.Close() })
}
func (b *openAIStreamBody) stop(err error) {
	b.stopOnce.Do(func() {
		b.cancel()
		b.closeUpstream()
		_ = b.reader.CloseWithError(err)
		_ = b.writer.CloseWithError(err)
	})
}
func (b *openAIStreamBody) Close() error {
	b.stop(io.ErrClosedPipe)
	<-b.done
	<-b.watchDone
	return nil
}

type pendingOpenAITool struct {
	id, name string
	args     strings.Builder
}

type openAIChoiceBuffer struct {
	tools  map[int]*pendingOpenAITool
	finish string
}

type openAIStreamBuffer struct {
	choices              map[int]*openAIChoiceBuffer
	toolBytes, toolCount int
}

func streamFormatError(reason string) error {
	return fmt.Errorf("OpenAI响应流不完整或格式无效：%s", reason)
}

func (b *openAIStreamBuffer) choice(index int) (*openAIChoiceBuffer, error) {
	if index < 0 || index >= maxOpenAIStreamChoices {
		return nil, streamFormatError("候选索引超过客户端边界")
	}
	choice := b.choices[index]
	if choice == nil {
		choice = &openAIChoiceBuffer{tools: map[int]*pendingOpenAITool{}}
		b.choices[index] = choice
	}
	return choice, nil
}

func (b *openAIStreamBuffer) collect(choice *openAIChoiceBuffer, raw json.RawMessage) error {
	var calls []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return streamFormatError("工具片段结构无效")
	}
	for _, call := range calls {
		if choice.finish != "" || call.Index < 0 || call.Index >= maxOpenAIStreamTools {
			return streamFormatError("终结后出现工具片段或工具索引无效")
		}
		tool := choice.tools[call.Index]
		if tool == nil {
			b.toolCount++
			if b.toolCount > maxOpenAIStreamTools {
				return streamFormatError("单回合工具数量超过256")
			}
			tool = &pendingOpenAITool{}
			choice.tools[call.Index] = tool
		}
		// Providers may repeat complete metadata. Actual fragments still append.
		for _, pair := range []struct {
			dst      *string
			fragment string
		}{{&tool.id, call.ID}, {&tool.name, call.Function.Name}} {
			if pair.fragment != "" && *pair.dst != pair.fragment {
				b.toolBytes += len(pair.fragment)
				if b.toolBytes > maxOpenAIStreamToolBytes {
					return streamFormatError("单回合工具参数超过8MiB")
				}
				*pair.dst += pair.fragment
			}
		}
		b.toolBytes += len(call.Function.Arguments)
		if b.toolBytes > maxOpenAIStreamToolBytes {
			return streamFormatError("单回合工具参数超过8MiB")
		}
		tool.args.WriteString(call.Function.Arguments)
	}
	return nil
}

// event separates data from SSE framing without decoding UTF-8 per read chunk.
// The inner recorder retains original bytes; only the provider-facing projection
// is normalized. Usage-only events and ordinary text keep streaming immediately.
func (b *openAIStreamBuffer) event(ctx context.Context, raw []byte, dst io.Writer) (done bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var data []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	value := strings.TrimSpace(strings.Join(data, "\n"))
	if value == "" {
		_, err := dst.Write(raw)
		return false, err
	}
	if value == "[DONE]" {
		if err := b.flush(ctx, dst, true); err != nil {
			return false, err
		}
		_, err := dst.Write(raw)
		return true, err
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(value), &envelope) != nil {
		return false, streamFormatError("SSE数据不是完整JSON")
	}
	var choices []map[string]json.RawMessage
	if rawChoices, ok := envelope["choices"]; ok {
		if json.Unmarshal(rawChoices, &choices) != nil {
			return false, streamFormatError("choices结构无效")
		}
	}
	if len(choices) == 0 {
		_, err := dst.Write(raw)
		return false, err
	}
	var visible []map[string]json.RawMessage
	changed := false
	for _, item := range choices {
		var index int
		if value := item["index"]; len(value) > 0 && json.Unmarshal(value, &index) != nil {
			return false, streamFormatError("候选索引无效")
		}
		choice, err := b.choice(index)
		if err != nil {
			return false, err
		}
		var delta map[string]json.RawMessage
		if value := item["delta"]; len(value) > 0 && json.Unmarshal(value, &delta) != nil {
			return false, streamFormatError("delta结构无效")
		}
		if calls, ok := delta["tool_calls"]; ok {
			if err := b.collect(choice, calls); err != nil {
				return false, err
			}
			delete(delta, "tool_calls")
			changed = true
		}
		var finish string
		if value := item["finish_reason"]; len(value) > 0 && !bytes.Equal(value, []byte("null")) {
			if json.Unmarshal(value, &finish) != nil {
				return false, streamFormatError("finish_reason结构无效")
			}
			if finish != "" {
				if choice.finish != "" && choice.finish != finish {
					return false, streamFormatError("终结原因互相冲突")
				}
				choice.finish = finish
				item["finish_reason"] = json.RawMessage(`null`)
				changed = true
			}
		}
		// This SDK requests and consumes one primary choice. Never execute a
		// tool or concatenate prose from an alternative candidate as choice 0.
		if index != 0 {
			changed = true
			continue
		}
		if delta != nil {
			item["delta"], _ = json.Marshal(delta)
		}
		visible = append(visible, item)
	}
	if !changed {
		_, err := dst.Write(raw)
		return false, err
	}
	envelope["choices"], _ = json.Marshal(visible)
	projected, err := json.Marshal(envelope)
	if err != nil {
		return false, err
	}
	_, err = fmt.Fprintf(dst, "data: %s\n\n", projected)
	return false, err
}

func (b *openAIStreamBuffer) flush(ctx context.Context, dst io.Writer, explicitDone bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	primary := b.choices[0]
	if primary == nil {
		return nil
	}
	if primary.finish == "" && (!explicitDone || len(primary.tools) > 0) {
		return streamFormatError("缺少完整finish_reason，工具未交付")
	}
	indices := make([]int, 0, len(primary.tools))
	ids := map[string]bool{}
	for index, tool := range primary.tools {
		args := strings.TrimSpace(tool.args.String())
		if primary.finish == "length" || primary.finish == "content_filter" || tool.id == "" || tool.name == "" || len(args) == 0 || args[0] != '{' || !json.Valid([]byte(args)) || ids[tool.id] {
			return streamFormatError("工具身份或JSON参数不完整，未执行")
		}
		ids[tool.id] = true
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		tool := primary.tools[index]
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "id": tool.id, "type": "function", "function": map[string]any{"name": tool.name, "arguments": tool.args.String()},
		}}}, "finish_reason": nil}}})
		if _, err := fmt.Fprintf(dst, "data: %s\n\n", frame); err != nil {
			return err
		}
	}
	if primary.finish != "" {
		frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": primary.finish}}})
		if _, err := fmt.Fprintf(dst, "data: %s\n\n", frame); err != nil {
			return err
		}
	}
	return nil
}

func normalizeOpenAIStream(ctx context.Context, src io.Reader, dst io.Writer) error {
	buffer := &openAIStreamBuffer{choices: map[int]*openAIChoiceBuffer{}}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), maxOpenAIStreamFrameBytes+1)
	var frame bytes.Buffer
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Bytes()
		if frame.Len()+len(line)+1 > maxOpenAIStreamFrameBytes {
			return streamFormatError("单个SSE帧超过1MiB")
		}
		frame.Write(line)
		frame.WriteByte('\n')
		if len(line) == 0 {
			done, err := buffer.event(ctx, frame.Bytes(), dst)
			frame.Reset()
			if err != nil || done {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if frame.Len() > 0 {
		return streamFormatError("SSE帧在分隔符前结束")
	}
	return buffer.flush(ctx, dst, false)
}
