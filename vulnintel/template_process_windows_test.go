package vulnintel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestTemplateCancellationReapsWindowsChild(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTemplateProcessHelper$")
	cmd.Env = append(os.Environ(), "GONDAR_TEMPLATE_PROCESS_TEST=parent", "GONDAR_TEMPLATE_PROCESS_READY="+ready)
	cmd.WaitDelay = 3 * time.Second
	configureTemplateCancellation(cmd)
	done := make(chan error, 1)
	go func() { _, err := cmd.CombinedOutput(); done <- err }()
	var pid int
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(20 * time.Millisecond)
	defer poll.Stop()
readyLoop:
	for {
		if raw, err := os.ReadFile(ready); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			if pid > 0 {
				break readyLoop
			}
		}
		select {
		case err := <-done:
			t.Fatalf("helper exited before creating its child: %v", err)
		case <-deadline.C:
			t.Fatal("helper child was not ready")
		case <-poll.C:
		}
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	defer func() {
		if state, _ := windows.WaitForSingleObject(handle, 0); state == uint32(windows.WAIT_TIMEOUT) {
			_ = windows.TerminateProcess(handle, 1) // only this test's known child
		}
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled helper unexpectedly completed")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancellation retained inherited output pipes")
	}
	if state, err := windows.WaitForSingleObject(handle, 0); err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("cancellation left child process alive: state=%d error=%v", state, err)
	}
}

func TestTemplateProcessHelper(t *testing.T) {
	switch os.Getenv("GONDAR_TEMPLATE_PROCESS_TEST") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestTemplateProcessHelper$")
		child.Env = append(os.Environ(), "GONDAR_TEMPLATE_PROCESS_TEST=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Run(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "child":
		if err := os.WriteFile(os.Getenv("GONDAR_TEMPLATE_PROCESS_READY"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
}
