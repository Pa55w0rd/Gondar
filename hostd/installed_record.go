package hostd

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gondar/hosttools"
)

func validateRawScript(data []byte) error {
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	switch strings.TrimSpace(string(line)) {
	case "#!/bin/sh", "#!/bin/bash", "#!/usr/bin/env sh", "#!/usr/bin/env bash":
		return nil
	}
	return fmt.Errorf("官方资产不是受支持的 shell 脚本，拒绝安装")
}

func validateRawELF(data []byte) error {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("官方资产不是有效 ELF 可执行文件")
	}
	defer f.Close()
	want := elf.EM_X86_64
	if runtime.GOARCH == "arm64" {
		want = elf.EM_AARCH64
	}
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") || f.Class != elf.ELFCLASS64 || f.Machine != want || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return fmt.Errorf("官方可执行文件不适用于当前平台")
	}
	return nil
}

func saveInstalledRecord(j *Job, tool hosttools.Tool, data []byte) error {
	if j.installPin == nil {
		return nil
	}
	digest := sha256.Sum256(data)
	record := hosttools.InstalledRecord{ToolID: tool.ID, Version: j.installPin.Version, SHA256: hex.EncodeToString(digest[:])}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	name := filepath.Join(j.installBinDir(), tool.Bin) + ".gondar-version.json"
	f, err := os.CreateTemp(j.installBinDir(), ".installed-version-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}
