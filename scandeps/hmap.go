// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package scandeps

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
)

// https://source.chromium.org/chromium/chromium/src/+/main:build/config/ios/write_framework_hmap.py
// https://chromium.googlesource.com/infra/goma/client/+/refs/heads/main/client/cxx/include_processor/include_file_utils.h#20
// https://chromium.googlesource.com/infra/goma/client/+/refs/heads/main/client/cxx/include_processor/include_file_utils.cc#41

type hmapParser struct {
	hmap []byte
	strs []byte
	buf  []byte
	err  error
}

func (p *hmapParser) checkMagic() {
	hmapMagic := [4]byte{'p', 'a', 'm', 'h'}
	if !bytes.HasPrefix(p.buf, hmapMagic[:]) {
		p.buf = nil
		p.err = fmt.Errorf("wrong hmap magic")
		return
	}
	p.buf = p.buf[4:]
}

func (p *hmapParser) checkVersion() {
	if p.err != nil {
		return
	}
	version := p.Uint16("version")
	if version != 1 {
		p.buf = nil
		p.err = fmt.Errorf("unknown hmap version %d", version)
		return
	}
}

func (p *hmapParser) Uint16(fieldName string) uint16 {
	if p.err != nil {
		return 0
	}
	if len(p.buf) < 2 {
		p.buf = nil
		p.err = fmt.Errorf("not enough for uint16 %s", fieldName)
		return 0
	}
	v := binary.LittleEndian.Uint16(p.buf)
	p.buf = p.buf[2:]
	return v
}

func (p *hmapParser) Uint32(fieldName string) uint32 {
	if p.err != nil {
		return 0
	}
	if len(p.buf) < 4 {
		p.buf = nil
		p.err = fmt.Errorf("not enough for uint32 %s", fieldName)
		return 0
	}
	v := binary.LittleEndian.Uint32(p.buf)
	p.buf = p.buf[4:]
	return v
}

func (p *hmapParser) Str(fieldName string) string {
	if p.err != nil {
		return ""
	}
	i := p.Uint32(fieldName)
	if p.err != nil {
		return ""
	}
	if i == 0 {
		return ""
	}
	if int(i) >= len(p.strs) {
		p.err = fmt.Errorf("out of index %s=%d", fieldName, i)
		return ""
	}
	v := p.strs[i:]
	e := bytes.IndexByte(v, 0)
	if e < 0 {
		p.err = fmt.Errorf("unterminated %s=%d", fieldName, i)
		return ""
	}
	return string(v[:e])
}

// ParseHeaderMap parses *.hmap file.
func ParseHeaderMap(ctx context.Context, buf []byte) (map[string]string, error) {
	p := &hmapParser{
		hmap: buf,
		buf:  buf,
	}
	p.checkMagic()
	p.checkVersion()
	p.Uint16("reserved")
	stringOffset := p.Uint32("string_offset")
	p.Uint32("string_count")
	hashCapacity := p.Uint32("hash_capacity")
	p.Uint32("max_value_length")
	if p.err != nil {
		return nil, fmt.Errorf("failed to parse hmap header: %w", p.err)
	}
	if len(p.hmap) < int(stringOffset) {
		return nil, fmt.Errorf("invalid string_offset=%d hmap size=%d", stringOffset, len(p.hmap))
	}
	p.strs = p.hmap[stringOffset:]
	m := make(map[string]string)
	for i := range hashCapacity {
		key := p.Str("key")
		prefix := p.Str("prefix")
		suffix := p.Str("suffix")
		if p.err != nil {
			return nil, fmt.Errorf("failed to get hmap bucket:%d: %w", i, p.err)
		}
		if len(p.buf) == 0 {
			break
		}
		if key == "" {
			continue
		}
		m[key] = prefix + suffix
	}
	return m, nil
}
