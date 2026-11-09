package mcphttp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// Match the existing Streamable HTTP SSE line budget. Limit before decoding or
// collecting tool output: Capture runs later and cannot bound parser memory.
const maxRPCFrameBytes = 8 << 20

func readMCPLine(r *bufio.Reader) (string, error) {
	var data []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(data)+len(part) > maxRPCFrameBytes {
			return "", fmt.Errorf("mcp: SSE line exceeds %d bytes", maxRPCFrameBytes)
		}
		data = append(data, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return string(data), err
	}
}

func readRPCJSON(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxRPCFrameBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRPCFrameBytes {
		return nil, fmt.Errorf("mcp: JSON response exceeds %d bytes", maxRPCFrameBytes)
	}
	return data, nil
}
