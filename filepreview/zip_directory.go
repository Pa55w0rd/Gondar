package filepreview

import (
	"encoding/binary"
	"errors"
	"os"
)

// Go's archive/zip reads central headers until a bad signature, not until the
// EOCD's declared count/size. Validate the actual bounded directory first, and
// reject offsets that would make its base-offset fallback read a hidden prefix.
func checkCentralDirectory(f *os.File, end, offset, length int64, count int) error {
	start := end - length
	base := start - offset
	if length < 0 || length > maxZipDirectory || start < 0 || offset < 0 || base < 0 {
		return errors.New("ZIP 目录偏移或长度无效")
	}
	if end >= 20 {
		var locator [4]byte
		if _, err := f.ReadAt(locator[:], end-20); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(locator[:]) == 0x07064b50 {
			return errors.New("ZIP64 不提供预览")
		}
	}
	if base > 0 {
		var signature [4]byte
		if _, err := f.ReadAt(signature[:], offset); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(signature[:]) == 0x02014b50 {
			return errors.New("ZIP 目录基偏移不一致，可能包含未申报的目录条目")
		}
	}
	b := make([]byte, int(length)) // no more than the independently capped 2 MiB
	if length > 0 {
		if _, err := f.ReadAt(b, start); err != nil {
			return err
		}
	}
	actual := 0
	for pos := 0; pos < len(b); {
		if actual >= maxZipEntries || len(b)-pos < 46 || binary.LittleEndian.Uint32(b[pos:pos+4]) != 0x02014b50 {
			return errors.New("ZIP 实际目录超过界限或包含无效条目")
		}
		h := b[pos : pos+46]
		nameLen := int(binary.LittleEndian.Uint16(h[28:30]))
		extraLen := int(binary.LittleEndian.Uint16(h[30:32]))
		commentLen := int(binary.LittleEndian.Uint16(h[32:34]))
		next := pos + 46 + nameLen + extraLen + commentLen
		if next > len(b) {
			return errors.New("ZIP 实际目录条目长度越界")
		}
		if binary.LittleEndian.Uint16(h[34:36]) != 0 || binary.LittleEndian.Uint32(h[20:24]) == 0xffffffff || binary.LittleEndian.Uint32(h[24:28]) == 0xffffffff || binary.LittleEndian.Uint32(h[42:46]) == 0xffffffff {
			return errors.New("ZIP 分卷或ZIP64成员不提供预览")
		}
		extra := b[pos+46+nameLen : pos+46+nameLen+extraLen]
		for len(extra) > 0 {
			if len(extra) < 4 {
				return errors.New("ZIP 扩展字段不完整")
			}
			tag := binary.LittleEndian.Uint16(extra[:2])
			n := int(binary.LittleEndian.Uint16(extra[2:4]))
			if n > len(extra)-4 {
				return errors.New("ZIP 扩展字段长度越界")
			}
			if tag == 1 {
				return errors.New("ZIP64成员不提供预览")
			}
			extra = extra[4+n:]
		}
		actual++
		pos = next
	}
	if actual != count {
		return errors.New("ZIP 实际目录数量与申报不一致")
	}
	return nil
}
