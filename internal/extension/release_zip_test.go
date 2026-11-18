package extension

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func releaseZIPTestArchive(t *testing.T, names ...string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if strings.HasSuffix(name, "/") {
			header.SetMode(os.ModeDir | 0o700)
		} else {
			header.SetMode(0o700)
		}
		output, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(name, "/") {
			if _, err := io.WriteString(output, "contents:"+name); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func releaseZIPTestEnd(t *testing.T, archive []byte) []byte {
	t.Helper()
	index := bytes.LastIndex(archive, []byte{'P', 'K', 5, 6})
	if index < 0 || len(archive)-index < releaseZIPDirectoryEndSize {
		t.Fatal("test archive has no end record")
	}
	return archive[index:]
}

type releaseZIPTestReader struct {
	reader     io.ReaderAt
	reads      int
	bytes      int
	cancel     context.CancelFunc
	cancelAt   int
	failAt     int
	failure    error
	tailSize   int
	tailReads  int
	cancelTail bool
}

func (reader *releaseZIPTestReader) ReadAt(buffer []byte, offset int64) (int, error) {
	reader.reads++
	if reader.failAt != 0 && reader.reads == reader.failAt {
		return 0, reader.failure
	}
	if offset == 0 && len(buffer) == reader.tailSize {
		reader.tailReads++
	}
	count, err := reader.reader.ReadAt(buffer, offset)
	reader.bytes += count
	if reader.cancelAt != 0 && reader.reads == reader.cancelAt || reader.cancelTail && reader.tailReads == 2 {
		reader.cancel()
	}
	return count, err
}

func TestReleaseZIPReaderAcceptsNormalPackagesAndImplicitDirectories(t *testing.T) {
	for _, names := range [][]string{
		{"magpie-plugin.json", "bin/worker"},
		{"bin/", "bin/worker", "assets/config/settings.json"},
		{"sdk/index.js", "sdk/日本語.txt"},
	} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			archive := releaseZIPTestArchive(t, names...)
			reader, err := releaseZIPReader(t.Context(), bytes.NewReader(archive), int64(len(archive)))
			if err != nil {
				t.Fatal(err)
			}
			if len(reader.File) != len(names) {
				t.Fatalf("wrong file count: %d", len(reader.File))
			}
			for index, file := range reader.File {
				if file.Name != names[index] {
					t.Fatalf("changed file name: %q", file.Name)
				}
				input, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(input)
				if err := errors.Join(readErr, input.Close()); err != nil {
					t.Fatal(err)
				}
				want := "contents:" + names[index]
				if strings.HasSuffix(names[index], "/") {
					want = ""
				}
				if string(body) != want {
					t.Fatalf("changed contents: %q", body)
				}
			}
		})
	}
}

func TestReleaseZIPReaderChecksActualCentralCountBeforeStandardParser(t *testing.T) {
	for _, count := range []int{2, catalogFilesLimit + 1, (1 << 16) + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var header [releaseZIPCentralHeaderSize]byte
			binary.LittleEndian.PutUint32(header[:4], releaseZIPCentralHeaderSign)
			binary.LittleEndian.PutUint16(header[6:8], 20)
			binary.LittleEndian.PutUint16(header[28:30], 1)
			var archive bytes.Buffer
			for range count {
				archive.Write(header[:])
				archive.WriteByte('a')
			}
			var end [releaseZIPDirectoryEndSize]byte
			binary.LittleEndian.PutUint32(end[:4], releaseZIPDirectoryEndSign)
			binary.LittleEndian.PutUint16(end[8:10], 1)
			binary.LittleEndian.PutUint16(end[10:12], 1)
			binary.LittleEndian.PutUint32(end[12:16], uint32(archive.Len()))
			archive.Write(end[:])
			if count == (1<<16)+1 {
				standard, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
				if err != nil {
					t.Fatalf("standard parser refused the modulo count fixture: %v", err)
				}
				if len(standard.File) != count {
					t.Fatalf("wrong modulo count fixture: %d files", len(standard.File))
				}
			}
			source := &releaseZIPTestReader{reader: bytes.NewReader(archive.Bytes())}
			if reader, err := releaseZIPReader(t.Context(), source, int64(archive.Len())); err == nil || reader != nil {
				t.Fatal("undeclared central records were accepted")
			}
			if source.bytes > (128 << 10) {
				t.Fatalf("malformed directory was consumed beyond its preflight bound: %d bytes", source.bytes)
			}
		})
	}
}

func TestReleaseZIPReaderAcceptsExactEntryLimitAndRefusesLargerClaims(t *testing.T) {
	names := make([]string, catalogFilesLimit)
	for index := range names {
		names[index] = fmt.Sprintf("files/%04d", index)
	}
	archive := releaseZIPTestArchive(t, names...)
	reader, err := releaseZIPReader(t.Context(), bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) != catalogFilesLimit {
		t.Fatalf("exact entry limit was refused: %v", err)
	}
	oversized := bytes.Clone(archive)
	end := releaseZIPTestEnd(t, oversized)
	binary.LittleEndian.PutUint16(end[8:10], catalogFilesLimit+1)
	binary.LittleEndian.PutUint16(end[10:12], catalogFilesLimit+1)
	source := &releaseZIPTestReader{reader: bytes.NewReader(oversized)}
	if _, err := releaseZIPReader(t.Context(), source, int64(len(oversized))); err == nil {
		t.Fatal("oversized claimed count was accepted")
	}
	if source.reads != 1 {
		t.Fatalf("oversized claim reached central parsing: %d reads", source.reads)
	}
}

func TestReleaseZIPReaderRejectsMalformedDirectoryBoundsAndCounts(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"disk", func(archive []byte) { binary.LittleEndian.PutUint16(releaseZIPTestEnd(t, archive)[4:6], 1) }},
		{"directory disk", func(archive []byte) { binary.LittleEndian.PutUint16(releaseZIPTestEnd(t, archive)[6:8], 1) }},
		{"disk count", func(archive []byte) { binary.LittleEndian.PutUint16(releaseZIPTestEnd(t, archive)[8:10], 1) }},
		{"too few", func(archive []byte) {
			end := releaseZIPTestEnd(t, archive)
			binary.LittleEndian.PutUint16(end[8:10], 1)
			binary.LittleEndian.PutUint16(end[10:12], 1)
		}},
		{"too many", func(archive []byte) {
			end := releaseZIPTestEnd(t, archive)
			binary.LittleEndian.PutUint16(end[8:10], 3)
			binary.LittleEndian.PutUint16(end[10:12], 3)
		}},
		{"hidden directory suffix", func(archive []byte) {
			end := releaseZIPTestEnd(t, archive)
			binary.LittleEndian.PutUint32(end[12:16], binary.LittleEndian.Uint32(end[12:16])-1)
		}},
		{"invalid directory offset", func(archive []byte) {
			end := releaseZIPTestEnd(t, archive)
			binary.LittleEndian.PutUint32(end[16:20], uint32(len(archive)))
		}},
		{"central signature", func(archive []byte) {
			offset := binary.LittleEndian.Uint32(releaseZIPTestEnd(t, archive)[16:20])
			archive[offset] = 0
		}},
		{"truncated central name", func(archive []byte) {
			offset := binary.LittleEndian.Uint32(releaseZIPTestEnd(t, archive)[16:20])
			binary.LittleEndian.PutUint16(archive[offset+28:offset+30], releaseZIPMaxPathBytes)
		}},
		{"local signature", func(archive []byte) { archive[0] = 0 }},
		{"local name mismatch", func(archive []byte) { archive[releaseZIPLocalHeaderSize] = 'x' }},
		{"central disk", func(archive []byte) {
			offset := binary.LittleEndian.Uint32(releaseZIPTestEnd(t, archive)[16:20])
			binary.LittleEndian.PutUint16(archive[offset+34:offset+36], 1)
		}},
		{"encryption", func(archive []byte) {
			offset := binary.LittleEndian.Uint32(releaseZIPTestEnd(t, archive)[16:20])
			binary.LittleEndian.PutUint16(archive[offset+8:offset+10], 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := releaseZIPTestArchive(t, "first", "second")
			test.mutate(archive)
			if reader, err := releaseZIPReader(t.Context(), bytes.NewReader(archive), int64(len(archive))); err == nil || reader != nil {
				t.Fatal("malformed ZIP was accepted")
			}
		})
	}
}

func TestReleaseZIPReaderRejectsZIP64AndMalformedExtras(t *testing.T) {
	for _, field := range []string{"disk records", "records", "directory size", "directory offset", "entry size", "entry offset", "entry version", "local version"} {
		t.Run(field, func(t *testing.T) {
			archive := releaseZIPTestArchive(t, "file")
			end := releaseZIPTestEnd(t, archive)
			offset := binary.LittleEndian.Uint32(end[16:20])
			switch field {
			case "disk records":
				binary.LittleEndian.PutUint16(end[8:10], 0xffff)
			case "records":
				binary.LittleEndian.PutUint16(end[10:12], 0xffff)
			case "directory size":
				binary.LittleEndian.PutUint32(end[12:16], 0xffffffff)
			case "directory offset":
				binary.LittleEndian.PutUint32(end[16:20], 0xffffffff)
			case "entry size":
				binary.LittleEndian.PutUint32(archive[offset+24:offset+28], 0xffffffff)
			case "entry offset":
				binary.LittleEndian.PutUint32(archive[offset+42:offset+46], 0xffffffff)
			case "entry version":
				binary.LittleEndian.PutUint16(archive[offset+6:offset+8], 45)
			case "local version":
				binary.LittleEndian.PutUint16(archive[4:6], 45)
			}
			if _, err := releaseZIPReader(t.Context(), bytes.NewReader(archive), int64(len(archive))); err == nil || !strings.Contains(err.Error(), "ZIP64") {
				t.Fatalf("ZIP64 feature was not clearly rejected: %v", err)
			}
		})
	}
	for _, extra := range [][]byte{{1, 0, 0, 0}, {2}, {2, 0, 4, 0, 1}} {
		t.Run(fmt.Sprintf("extra-%x", extra), func(t *testing.T) {
			var archive bytes.Buffer
			writer := zip.NewWriter(&archive)
			if _, err := writer.CreateHeader(&zip.FileHeader{Name: "file", Method: zip.Store, Extra: extra}); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := releaseZIPReader(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len())); err == nil {
				t.Fatal("ZIP64 or malformed extra field was accepted")
			}
		})
	}
}

func TestReleaseZIPReaderRejectsRealZIP64EndRecords(t *testing.T) {
	for _, sentinels := range []bool{false, true} {
		t.Run(fmt.Sprintf("sentinels=%t", sentinels), func(t *testing.T) {
			original := releaseZIPTestArchive(t, "file")
			end := bytes.Clone(releaseZIPTestEnd(t, original))
			endOffset := len(original) - len(end)
			var extended [56]byte
			binary.LittleEndian.PutUint32(extended[:4], 0x06064b50)
			binary.LittleEndian.PutUint64(extended[4:12], 44)
			binary.LittleEndian.PutUint16(extended[12:14], 45)
			binary.LittleEndian.PutUint16(extended[14:16], 45)
			binary.LittleEndian.PutUint64(extended[24:32], 1)
			binary.LittleEndian.PutUint64(extended[32:40], 1)
			binary.LittleEndian.PutUint64(extended[40:48], uint64(binary.LittleEndian.Uint32(end[12:16])))
			binary.LittleEndian.PutUint64(extended[48:56], uint64(binary.LittleEndian.Uint32(end[16:20])))
			var locator [20]byte
			binary.LittleEndian.PutUint32(locator[:4], 0x07064b50)
			binary.LittleEndian.PutUint64(locator[8:16], uint64(endOffset))
			binary.LittleEndian.PutUint32(locator[16:20], 1)
			if sentinels {
				binary.LittleEndian.PutUint16(end[8:10], 0xffff)
				binary.LittleEndian.PutUint16(end[10:12], 0xffff)
				binary.LittleEndian.PutUint32(end[12:16], 0xffffffff)
				binary.LittleEndian.PutUint32(end[16:20], 0xffffffff)
			}
			archive := append(bytes.Clone(original[:endOffset]), extended[:]...)
			archive = append(archive, locator[:]...)
			archive = append(archive, end...)
			standard, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
			if err != nil || len(standard.File) != 1 {
				t.Fatalf("invalid ZIP64 fixture: %v", err)
			}
			if _, err := releaseZIPReader(t.Context(), bytes.NewReader(archive), int64(len(archive))); err == nil || !strings.Contains(err.Error(), "ZIP64") {
				t.Fatalf("real ZIP64 end records were not refused: %v", err)
			}
		})
	}
}

func TestReleaseZIPReaderAcceptsCommentsAndOrdinaryExtras(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	if err := writer.SetComment("ordinary archive comment"); err != nil {
		t.Fatal(err)
	}
	extra := []byte{0xfe, 0xca, 3, 0, 1, 2, 3}
	output, err := writer.CreateHeader(&zip.FileHeader{Name: "file", Method: zip.Store, Extra: extra})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(output, "contents"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := releaseZIPReader(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil || len(reader.File) != 1 {
		t.Fatalf("ordinary ZIP metadata was refused: %v", err)
	}
	input, err := reader.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(input)
	if err := errors.Join(readErr, input.Close()); err != nil || string(body) != "contents" {
		t.Fatalf("ordinary extra field changed body access: %q %v", body, err)
	}
	if reader.Comment != "ordinary archive comment" {
		t.Fatalf("archive comment was lost: %q", reader.Comment)
	}
}

func TestReleaseZIPReaderRejectsAmbiguousEndRecordInComment(t *testing.T) {
	archive := releaseZIPTestArchive(t, "file")
	end := releaseZIPTestEnd(t, archive)
	fake := bytes.Clone(end)
	binary.LittleEndian.PutUint32(fake[12:16], binary.LittleEndian.Uint32(fake[12:16])+releaseZIPDirectoryEndSize)
	binary.LittleEndian.PutUint16(end[20:22], uint16(len(fake)+1))
	archive = append(archive, fake...)
	archive = append(archive, 'x')
	if _, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive))); err != nil {
		t.Fatalf("standard reader did not select the ambiguous end record: %v", err)
	}
	if _, err := releaseZIPReader(t.Context(), bytes.NewReader(archive), int64(len(archive))); err == nil {
		t.Fatal("preflight and standard parser could select different end records")
	}
}

func TestReleaseZIPPathBoundariesAndUnsafeNames(t *testing.T) {
	depth := func(count int) string { return strings.TrimSuffix(strings.Repeat("dir/", count), "/") }
	for _, name := range []string{
		"magpie-plugin.json", "bin/worker", "bin/", "assets/日本語.json",
		strings.Repeat("a", releaseZIPMaxPathBytes), depth(releaseZIPMaxPathDepth), depth(releaseZIPMaxPathDepth) + "/",
	} {
		if !releaseZIPPath(name) {
			t.Errorf("normal or boundary path was refused: %q", name)
		}
	}
	for _, name := range []string{
		"", ".", "..", "../outside", "/outside", `C:outside`, `dir\outside`,
		"dir/../outside", "dir//file", "dir//", "dir/\x00file", "dir/\nfile",
		strings.Repeat("a", releaseZIPMaxPathBytes+1), depth(releaseZIPMaxPathDepth + 1),
	} {
		if releaseZIPPath(name) {
			t.Errorf("unsafe or unbounded path was accepted: %q", name)
		}
	}
	for _, name := range []string{strings.Repeat("a", releaseZIPMaxPathBytes+1), depth(releaseZIPMaxPathDepth + 1)} {
		archive := releaseZIPTestArchive(t, name)
		source := &releaseZIPTestReader{reader: bytes.NewReader(archive), tailSize: len(archive)}
		if _, err := releaseZIPReader(t.Context(), source, int64(len(archive))); err == nil || source.tailReads != 1 {
			t.Fatalf("unbounded path reached the standard parser: error=%v tail reads=%d", err, source.tailReads)
		}
	}
}

func TestReleaseZIPReaderCancellationAndReadErrors(t *testing.T) {
	archive := releaseZIPTestArchive(t, "file")
	for _, cancelAt := range []int{0, 1, 2, 4} {
		t.Run(fmt.Sprintf("cancel-%d", cancelAt), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			source := &releaseZIPTestReader{reader: bytes.NewReader(archive), cancel: cancel, cancelAt: cancelAt}
			if cancelAt == 0 {
				cancel()
			}
			if _, err := releaseZIPReader(ctx, source, int64(len(archive))); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was lost: %v", err)
			}
			if cancelAt == 0 && source.reads != 0 {
				t.Fatal("canceled preflight read the archive")
			}
		})
	}
	t.Run("standard parser", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		source := &releaseZIPTestReader{reader: bytes.NewReader(archive), tailSize: len(archive), cancel: cancel, cancelTail: true}
		if _, err := releaseZIPReader(ctx, source, int64(len(archive))); !errors.Is(err, context.Canceled) || source.tailReads != 2 {
			t.Fatalf("standard parser bypassed cancellation: tail reads=%d error=%v", source.tailReads, err)
		}
	})
	t.Run("returned reader", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reader, err := releaseZIPReader(ctx, bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		input, err := reader.File[0].Open()
		if err == nil {
			_, err = io.ReadAll(input)
			input.Close()
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("returned reader detached from its context: %v", err)
		}
	})
	failure := errors.New("fixture read failed")
	source := &releaseZIPTestReader{reader: bytes.NewReader(archive), failAt: 2, failure: failure}
	if _, err := releaseZIPReader(t.Context(), source, int64(len(archive))); !errors.Is(err, failure) {
		t.Fatalf("archive read failure was lost: %v", err)
	}
}

func TestReleaseZIPReaderRejectsInvalidSizesAndEndLayouts(t *testing.T) {
	archive := releaseZIPTestArchive(t, "file")
	for _, size := range []int64{-1, 0, releaseZIPDirectoryEndSize - 1, catalogPackageLimit + 1} {
		source := &releaseZIPTestReader{reader: bytes.NewReader(archive)}
		if _, err := releaseZIPReader(t.Context(), source, size); err == nil || source.reads != 0 {
			t.Fatalf("invalid size reached archive reads: %d %v", size, err)
		}
	}
	for _, data := range [][]byte{archive[:len(archive)-1], append(bytes.Clone(archive), 0), bytes.Repeat([]byte("x"), 100)} {
		if _, err := releaseZIPReader(t.Context(), bytes.NewReader(data), int64(len(data))); err == nil {
			t.Fatal("truncated, trailing or missing end record was accepted")
		}
	}
	if _, err := releaseZIPReader(t.Context(), nil, int64(len(archive))); err == nil {
		t.Fatal("nil reader accepted")
	}
}
