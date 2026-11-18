package extension

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	releaseZIPMaxPathBytes      = 1024
	releaseZIPMaxPathDepth      = 32
	releaseZIPDirectoryEndSize  = 22
	releaseZIPCentralHeaderSize = 46
	releaseZIPLocalHeaderSize   = 30
	releaseZIPDirectoryEndSign  = 0x06054b50
	releaseZIPCentralHeaderSign = 0x02014b50
	releaseZIPLocalHeaderSign   = 0x04034b50
)

type releaseZIPDirectoryInfo struct {
	offset int64
	end    int64
	count  int
}

type releaseZIPRecord struct {
	name       string
	offset     int64
	compressed int64
	flags      uint16
	method     uint16
}

type releaseZIPContextReader struct {
	ctx    context.Context
	reader io.ReaderAt
}

func (reader releaseZIPContextReader) ReadAt(buffer []byte, offset int64) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := reader.reader.ReadAt(buffer, offset)
	if canceled := reader.ctx.Err(); canceled != nil {
		return count, canceled
	}
	return count, err
}

func releaseZIPReader(ctx context.Context, reader io.ReaderAt, size int64) (*zip.Reader, error) {
	if ctx == nil || reader == nil {
		return nil, errors.New("release ZIP requires a context and reader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size < releaseZIPDirectoryEndSize || size > catalogPackageLimit {
		return nil, errors.New("release ZIP size is outside its limit")
	}
	checked := releaseZIPContextReader{ctx: ctx, reader: reader}
	directory, err := releaseZIPDirectory(checked, size)
	if err != nil {
		return nil, err
	}
	records := make([]releaseZIPRecord, 0, directory.count)
	extra := make([]byte, (1<<16)-1)
	for offset := directory.offset; offset < directory.end; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(records) >= directory.count {
			return nil, errors.New("release ZIP has more central records than declared")
		}
		var header [releaseZIPCentralHeaderSize]byte
		if directory.end-offset < int64(len(header)) {
			return nil, errors.New("release ZIP central header is truncated")
		}
		if err := releaseZIPReadAt(checked, header[:], offset); err != nil {
			return nil, err
		}
		if binary.LittleEndian.Uint32(header[:4]) != releaseZIPCentralHeaderSign {
			return nil, errors.New("release ZIP has unsupported or malformed central records")
		}
		version := binary.LittleEndian.Uint16(header[6:8])
		flags := binary.LittleEndian.Uint16(header[8:10])
		method := binary.LittleEndian.Uint16(header[10:12])
		compressed := binary.LittleEndian.Uint32(header[20:24])
		uncompressed := binary.LittleEndian.Uint32(header[24:28])
		nameSize := int(binary.LittleEndian.Uint16(header[28:30]))
		extraSize := int(binary.LittleEndian.Uint16(header[30:32]))
		commentSize := int(binary.LittleEndian.Uint16(header[32:34]))
		disk := binary.LittleEndian.Uint16(header[34:36])
		localOffset := binary.LittleEndian.Uint32(header[42:46])
		if compressed == 0xffffffff || uncompressed == 0xffffffff || localOffset == 0xffffffff || disk == 0xffff {
			return nil, errors.New("release ZIP64 entries are not supported")
		}
		if err := releaseZIPFeatures(version, flags, method); err != nil {
			return nil, err
		}
		if disk != 0 {
			return nil, errors.New("release ZIP must use a single disk")
		}
		if nameSize == 0 || nameSize > releaseZIPMaxPathBytes {
			return nil, errors.New("release ZIP path exceeds its length limit")
		}
		recordSize := int64(len(header) + nameSize + extraSize + commentSize)
		if recordSize > directory.end-offset {
			return nil, errors.New("release ZIP central record is truncated")
		}
		name := make([]byte, nameSize)
		if err := releaseZIPReadAt(checked, name, offset+int64(len(header))); err != nil {
			return nil, err
		}
		if !releaseZIPPath(string(name)) {
			return nil, errors.New("release ZIP path is unsafe or exceeds its depth limit")
		}
		if err := releaseZIPReadAt(checked, extra[:extraSize], offset+int64(len(header)+nameSize)); err != nil {
			return nil, err
		}
		if err := releaseZIPExtra(extra[:extraSize]); err != nil {
			return nil, err
		}
		records = append(records, releaseZIPRecord{
			name: string(name), offset: int64(localOffset), compressed: int64(compressed), flags: flags, method: method,
		})
		offset += recordSize
	}
	if len(records) != directory.count {
		return nil, errors.New("release ZIP has fewer central records than declared")
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := releaseZIPLocal(checked, directory.offset, record, extra); err != nil {
			return nil, err
		}
	}
	archive, err := zip.NewReader(checked, size)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(archive.File) != len(records) {
		return nil, errors.New("release ZIP central records changed during validation")
	}
	return archive, nil
}

func releaseZIPDirectory(reader io.ReaderAt, size int64) (releaseZIPDirectoryInfo, error) {
	tail := make([]byte, int(min(size, releaseZIPDirectoryEndSize+(1<<16)-1)))
	if err := releaseZIPReadAt(reader, tail, size-int64(len(tail))); err != nil {
		return releaseZIPDirectoryInfo{}, err
	}
	for index := len(tail) - releaseZIPDirectoryEndSize; index >= 0; index-- {
		if binary.LittleEndian.Uint32(tail[index:index+4]) != releaseZIPDirectoryEndSign {
			continue
		}
		end := tail[index : index+releaseZIPDirectoryEndSize]
		commentSize := int(binary.LittleEndian.Uint16(end[20:22]))
		if index+releaseZIPDirectoryEndSize+commentSize != len(tail) {
			return releaseZIPDirectoryInfo{}, errors.New("release ZIP has a truncated comment or trailing data")
		}
		diskCount := binary.LittleEndian.Uint16(end[8:10])
		count := binary.LittleEndian.Uint16(end[10:12])
		length := binary.LittleEndian.Uint32(end[12:16])
		offset := binary.LittleEndian.Uint32(end[16:20])
		if diskCount == 0xffff || count == 0xffff || length == 0xffffffff || offset == 0xffffffff {
			return releaseZIPDirectoryInfo{}, errors.New("release ZIP64 directories are not supported")
		}
		if binary.LittleEndian.Uint16(end[4:6]) != 0 || binary.LittleEndian.Uint16(end[6:8]) != 0 || diskCount != count {
			return releaseZIPDirectoryInfo{}, errors.New("release ZIP must use a single disk with matching record counts")
		}
		if count == 0 || int(count) > catalogFilesLimit {
			return releaseZIPDirectoryInfo{}, errors.New("release ZIP exceeds its entry limit or is empty")
		}
		endOffset := size - int64(len(tail)) + int64(index)
		if int64(offset)+int64(length) != endOffset || int64(length) < int64(count)*releaseZIPCentralHeaderSize {
			return releaseZIPDirectoryInfo{}, errors.New("release ZIP directory bounds are invalid; ZIP64 and extra directory records are unsupported")
		}
		return releaseZIPDirectoryInfo{offset: int64(offset), end: endOffset, count: int(count)}, nil
	}
	return releaseZIPDirectoryInfo{}, errors.New("release ZIP end record is missing")
}

func releaseZIPLocal(reader io.ReaderAt, directoryOffset int64, record releaseZIPRecord, extra []byte) error {
	var header [releaseZIPLocalHeaderSize]byte
	if record.offset > directoryOffset-int64(len(header)) {
		return errors.New("release ZIP local header is outside the file data")
	}
	if err := releaseZIPReadAt(reader, header[:], record.offset); err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(header[:4]) != releaseZIPLocalHeaderSign {
		return errors.New("release ZIP local header is invalid")
	}
	version := binary.LittleEndian.Uint16(header[4:6])
	flags := binary.LittleEndian.Uint16(header[6:8])
	method := binary.LittleEndian.Uint16(header[8:10])
	if binary.LittleEndian.Uint32(header[18:22]) == 0xffffffff || binary.LittleEndian.Uint32(header[22:26]) == 0xffffffff {
		return errors.New("release ZIP64 local headers are not supported")
	}
	if err := releaseZIPFeatures(version, flags, method); err != nil {
		return err
	}
	if flags != record.flags || method != record.method {
		return errors.New("release ZIP local and central features disagree")
	}
	nameSize := int(binary.LittleEndian.Uint16(header[26:28]))
	extraSize := int(binary.LittleEndian.Uint16(header[28:30]))
	bodyOffset := record.offset + int64(len(header)+nameSize+extraSize)
	if nameSize != len(record.name) || bodyOffset+record.compressed > directoryOffset {
		return errors.New("release ZIP local name or data bounds are invalid")
	}
	name := make([]byte, nameSize)
	if err := releaseZIPReadAt(reader, name, record.offset+int64(len(header))); err != nil {
		return err
	}
	if !bytes.Equal(name, []byte(record.name)) {
		return errors.New("release ZIP local and central names disagree")
	}
	if err := releaseZIPReadAt(reader, extra[:extraSize], record.offset+int64(len(header)+nameSize)); err != nil {
		return err
	}
	return releaseZIPExtra(extra[:extraSize])
}

func releaseZIPFeatures(version, flags, method uint16) error {
	if version > 20 {
		return errors.New("release ZIP requires unsupported features such as ZIP64")
	}
	if flags & ^uint16(0x080e) != 0 || method != zip.Store && method != zip.Deflate {
		return errors.New("release ZIP encryption or compression method is unsupported")
	}
	return nil
}

func releaseZIPExtra(extra []byte) error {
	for len(extra) > 0 {
		if len(extra) < 4 {
			return errors.New("release ZIP extra field is truncated")
		}
		tag := binary.LittleEndian.Uint16(extra[:2])
		size := int(binary.LittleEndian.Uint16(extra[2:4]))
		if tag == 1 {
			return errors.New("release ZIP64 extra fields are not supported")
		}
		if size > len(extra)-4 {
			return errors.New("release ZIP extra field size is invalid")
		}
		extra = extra[4+size:]
	}
	return nil
}

func releaseZIPReadAt(reader io.ReaderAt, buffer []byte, offset int64) error {
	if len(buffer) == 0 {
		return nil
	}
	count, err := reader.ReadAt(buffer, offset)
	if err != nil && err != io.EOF {
		return fmt.Errorf("read release ZIP: %w", err)
	}
	if count != len(buffer) {
		return fmt.Errorf("read release ZIP: %w", io.ErrUnexpectedEOF)
	}
	return nil
}

func releaseZIPPath(name string) bool {
	if name == "" || len(name) > releaseZIPMaxPathBytes {
		return false
	}
	name = strings.TrimSuffix(name, "/")
	return strings.Count(name, "/") < releaseZIPMaxPathDepth && catalogRelative(name) && path.Clean(name) == name
}
