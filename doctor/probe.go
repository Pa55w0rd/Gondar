package doctor

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var versionArgs = map[string][]string{
	"go": {"version"}, "node": {"--version"}, "python3": {"--version"},
	"curl": {"--version"}, "git": {"--version"}, "rg": {"--version"},
	"psql": {"--version"}, "docker": {"--version"}, "nmap": {"--version"},
	"nuclei": {"-version"}, "ffuf": {"-V"}, "gobuster": {"version"},
}

type boundedOutput struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remaining := 4096 - len(b.data); remaining > 0 {
		b.data = append(b.data, p[:min(len(p), remaining)]...)
	}
	return len(p), nil
}

func probeVersion(ctx context.Context, bin, filename string) (string, bool) {
	args, ok := versionArgs[bin]
	if !ok {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filename, args...)
	// `go version` must report the installed toolchain instead of implicitly
	// downloading the module's preferred toolchain during a deployment probe.
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	cmd.WaitDelay = 200 * time.Millisecond
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return "", false
	}
	line := strings.TrimSpace(strings.SplitN(string(output.data), "\n", 2)[0])
	// Version output is not a general log transport: accept a short, inert line
	// without URLs, credential markers or terminal controls.
	lower := strings.ToLower(line)
	if len(line) > 256 || strings.ContainsAny(line, "\x00\x1b\r") || strings.Contains(lower, "://") || strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "api_key") || strings.Contains(lower, "secret") {
		return "", false
	}
	return line, line != ""
}
