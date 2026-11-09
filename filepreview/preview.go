// Package filepreview reads bounded, inert views of evidence. It never extracts
// archives, executes content, or changes the original evidence bytes.
package filepreview

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	MaxBytes        = 64 << 10
	MaxRows         = 60
	MaxColumns      = 32
	MaxEntries      = 80
	maxZipEntries   = 1024
	maxZipDirectory = 2 << 20
	maxZipFile      = 64 << 20
)

type Entry struct {
	Name      string `json:"name"`
	Size      uint64 `json:"size_bytes"`
	Directory bool   `json:"directory"`
}

type Preview struct {
	Format    string     `json:"format"`
	Name      string     `json:"name"`
	Size      int64      `json:"size_bytes"`
	Text      string     `json:"text,omitempty"`
	Rows      [][]string `json:"rows,omitempty"`
	Entries   []Entry    `json:"entries,omitempty"`
	Truncated bool       `json:"truncated"`
	Notice    string     `json:"notice,omitempty"`
}

// Read requires an already-authorized local file. entry optionally names one
// regular ZIP member; its bytes are streamed with the same output limit.
func Read(filename, displayName, entry string) (*Preview, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("仅支持普通文件")
	}
	p := &Preview{Name: displayName, Size: st.Size()}
	ext := strings.ToLower(filepath.Ext(displayName))
	if entry != "" && ext != ".zip" {
		return nil, errors.New("只有 ZIP 支持选择成员")
	}
	switch ext {
	case ".zip":
		return readZip(f, st.Size(), p, entry)
	case ".gz", ".gzip":
		z, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("Gzip 格式错误：%w", err)
		}
		defer z.Close()
		z.Multistream(false)
		name := strings.TrimSuffix(displayName, filepath.Ext(displayName))
		if err := readContent(z, name, p); err != nil {
			return nil, err
		}
		p.Format = "gzip/" + p.Format
		p.Notice = "仅预览首个 Gzip 成员；预览不等于完整性或安全校验"
		return p, nil
	default:
		if err := readContent(f, displayName, p); err != nil {
			return nil, err
		}
		return p, nil
	}
}

func readContent(r io.Reader, name string, p *Preview) error {
	b, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return fmt.Errorf("读取预览失败：%w", err)
	}
	if len(b) > MaxBytes {
		b = b[:MaxBytes]
		p.Truncated = true
	}
	// Do not turn a UTF-8 boundary into a bogus replacement character.
	if p.Truncated {
		for len(b) > 0 && !utf8.Valid(b) && len(b) > MaxBytes-4 {
			b = b[:len(b)-1]
		}
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return errors.New("二进制内容不提供文本预览，请下载原件")
	}
	text := strings.ToValidUTF8(string(b), "�")
	if strings.EqualFold(filepath.Ext(name), ".csv") {
		p.Format = "csv"
		cr := csv.NewReader(strings.NewReader(text))
		cr.FieldsPerRecord = -1
		for i := 0; i <= MaxRows; i++ {
			row, err := cr.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				if p.Truncated {
					p.Notice = "预览达到字节上限，末尾未完整的 CSV 行已省略"
					break
				}
				return fmt.Errorf("CSV 格式错误：%w", err)
			}
			if i == MaxRows {
				p.Truncated = true
				break
			}
			if len(row) > MaxColumns {
				row = row[:MaxColumns]
				p.Truncated = true
			}
			p.Rows = append(p.Rows, row)
		}
		return nil
	}
	p.Format = "text"
	lines := strings.SplitN(text, "\n", MaxRows+1)
	if len(lines) > MaxRows {
		lines = lines[:MaxRows]
		p.Truncated = true
	}
	p.Text = strings.Join(lines, "\n")
	return nil
}

// Inspect the bounded EOCD before archive/zip allocates its directory. ZIP64 and
// very large directories are deliberately left to download/offline tools.
func checkZipDirectory(f *os.File, size int64) error {
	if size > maxZipFile {
		return errors.New("ZIP 超过 64 MiB 预览上限，请下载原件")
	}
	n := int64(22 + 65535)
	if n > size {
		n = size
	}
	b := make([]byte, n)
	if _, err := f.ReadAt(b, size-n); err != nil {
		return err
	}
	for i := len(b) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(b[i:i+4]) != 0x06054b50 {
			continue
		}
		comment := int(binary.LittleEndian.Uint16(b[i+20 : i+22]))
		if i+22+comment != len(b) {
			// archive/zip selects the last EOCD-looking header even when its
			// comment ends before EOF. Do not validate a different earlier one.
			return errors.New("ZIP 尾部长度不一致或存在歧义目录")
		}
		disk := binary.LittleEndian.Uint16(b[i+4 : i+6])
		centralDisk := binary.LittleEndian.Uint16(b[i+6 : i+8])
		diskCount := binary.LittleEndian.Uint16(b[i+8 : i+10])
		count := binary.LittleEndian.Uint16(b[i+10 : i+12])
		dirBytes := binary.LittleEndian.Uint32(b[i+12 : i+16])
		dirOffset := binary.LittleEndian.Uint32(b[i+16 : i+20])
		if disk != 0 || centralDisk != 0 || diskCount != count || count > maxZipEntries || dirBytes > maxZipDirectory || dirOffset == 0xffffffff {
			return errors.New("ZIP 分卷、ZIP64 或目录超过预览界限，请下载原件")
		}
		return checkCentralDirectory(f, size-n+int64(i), int64(dirOffset), int64(dirBytes), int(count))
	}
	return errors.New("ZIP 目录无效")
}

func safeMember(name string) bool {
	return name != "" && !strings.Contains(name, "\\") && !strings.Contains(name, ":") &&
		!strings.HasPrefix(name, "/") && path.Clean(name) == strings.TrimSuffix(name, "/") &&
		name != ".." && !strings.HasPrefix(name, "../")
}

func readZip(f *os.File, size int64, p *Preview, member string) (*Preview, error) {
	if err := checkZipDirectory(f, size); err != nil {
		return nil, err
	}
	z, err := zip.NewReader(f, size)
	if err != nil {
		return nil, err
	}
	if len(z.File) > maxZipEntries {
		return nil, errors.New("ZIP 成员过多")
	}
	p.Format = "zip"
	if member == "" {
		used := 0
		for _, e := range z.File {
			if len(p.Entries) == MaxEntries {
				p.Truncated = true
				break
			}
			entry := Entry{Name: e.Name, Size: e.UncompressedSize64, Directory: e.FileInfo().IsDir()}
			raw, _ := json.Marshal(entry)
			if len(raw) > MaxBytes-1024-used {
				p.Truncated = true
				break
			}
			used += len(raw)
			p.Entries = append(p.Entries, entry)
		}
		p.Notice = "仅列出归档成员；未解压、未执行，选择成员后最多读取 64 KiB"
		return p, nil
	}
	if !safeMember(member) {
		return nil, errors.New("ZIP 成员路径无效")
	}
	for _, e := range z.File {
		if e.Name != member {
			continue
		}
		if !e.Mode().IsRegular() {
			return nil, errors.New("不预览目录、符号链接或特殊文件")
		}
		if strings.EqualFold(path.Ext(member), ".zip") || strings.EqualFold(path.Ext(member), ".gz") {
			return nil, errors.New("嵌套归档不递归预览")
		}
		r, err := e.Open()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		if err := readContent(r, member, p); err != nil {
			return nil, err
		}
		p.Format = "zip/" + p.Format
		p.Name = member
		p.Notice = "归档成员的有界预览，原归档保持不变"
		return p, nil
	}
	return nil, errors.New("ZIP 成员不存在")
}
