//go:build linux && cgo

package extension

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type nativeDependencyFile struct {
	Path   string
	Inode  uint64
	Device string
	SHA256 string
}

func nativeDependencyIdentity(filename string) (string, error) {
	for _, name := range []string{"LD_LIBRARY_PATH", "LD_PRELOAD", "LD_AUDIT"} {
		if os.Getenv(name) != "" {
			return "", errors.New("native loader environment prevents dependency equivalence")
		}
	}
	main, err := elf.Open(filename)
	if err != nil {
		return "", err
	}
	defer main.Close()
	required, err := nativeDependencyNames(main)
	if err != nil {
		return "", err
	}
	mappings, err := os.ReadFile("/proc/self/maps")
	if err != nil || len(mappings) > 8<<20 {
		return "", errors.New("native dependency mappings are unavailable")
	}
	paths := map[string]nativeDependencyFile{}
	for _, line := range strings.Split(string(mappings), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 || !filepath.IsAbs(fields[5]) {
			continue
		}
		inode, err := strconv.ParseUint(fields[4], 10, 64)
		if err == nil && inode != 0 {
			paths[fields[5]] = nativeDependencyFile{Path: fields[5], Inode: inode, Device: fields[3]}
		}
	}
	available := map[string][]nativeDependencyFile{}
	for path, mapped := range paths {
		image, err := elf.Open(path)
		if err != nil {
			continue
		}
		names, err := image.DynString(elf.DT_SONAME)
		image.Close()
		if err != nil || len(names) != 1 {
			continue
		}
		available[names[0]] = append(available[names[0]], mapped)
	}
	visited := map[string]bool{}
	var closure []nativeDependencyFile
	for len(required) != 0 {
		name := required[0]
		required = required[1:]
		if visited[name] {
			continue
		}
		visited[name] = true
		if len(visited) > 64 || strings.ContainsAny(name, `/\`) {
			return "", errors.New("native dependency closure is not bounded")
		}
		matches := available[name]
		if len(matches) != 1 {
			return "", fmt.Errorf("native dependency %q is not uniquely mapped", name)
		}
		dependency := matches[0]
		resolved, err := filepath.EvalSymlinks(dependency.Path)
		if err != nil || !nativeSystemLibraryPath(resolved) {
			return "", errors.New("native dependency is outside immutable system libraries")
		}
		file, err := os.Open(resolved)
		if err != nil {
			return "", err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return "", err
		}
		stat, valid := info.Sys().(*syscall.Stat_t)
		if !valid || !info.Mode().IsRegular() || !nativeSystemOwner(stat.Uid) || stat.Ino != dependency.Inode ||
			info.Mode().Perm()&0o022 != 0 || info.Size() > catalogFileLimit {
			file.Close()
			return "", errors.New("native mapped dependency no longer matches a stable system file")
		}
		if dependency.Device != fmt.Sprintf("%02x:%02x", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev))) {
			file.Close()
			return "", errors.New("native mapped dependency device changed")
		}
		image, err := elf.NewFile(file)
		if err != nil {
			file.Close()
			return "", err
		}
		names, err := nativeDependencyNames(image)
		if err != nil {
			file.Close()
			return "", err
		}
		required = append(required, names...)
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			file.Close()
			return "", err
		}
		hash := sha256.New()
		size, readErr := io.Copy(hash, io.LimitReader(file, catalogFileLimit+1))
		after, statErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil || statErr != nil || closeErr != nil || size != info.Size() ||
			after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return "", errors.New("native dependency changed while hashing")
		}
		dependency.Path, dependency.SHA256 = resolved, hex.EncodeToString(hash.Sum(nil))
		closure = append(closure, dependency)
	}
	slices.SortFunc(closure, func(left, right nativeDependencyFile) int {
		return strings.Compare(left.Path, right.Path)
	})
	body, err := json.Marshal(closure)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func nativeSystemOwner(owner uint32) bool {
	return owner == 0
}

func nativeDependencyNames(image *elf.File) ([]string, error) {
	for _, tag := range []elf.DynTag{elf.DT_RPATH, elf.DT_RUNPATH} {
		values, err := image.DynString(tag)
		if err != nil || len(values) != 0 {
			return nil, errors.New("native origin-sensitive dependency resolution is not cacheable")
		}
	}
	for _, tag := range []elf.DynTag{elf.DT_FILTER, elf.DT_AUXILIARY, elf.DT_AUDIT, elf.DT_DEPAUDIT, elf.DT_CONFIG} {
		values, err := image.DynValue(tag)
		if err != nil || len(values) != 0 {
			return nil, errors.New("native dependency filters are not cacheable")
		}
	}
	return image.ImportedLibraries()
}

func nativeSystemLibraryPath(path string) bool {
	for _, prefix := range []string{"/lib/", "/lib64/", "/usr/lib/", "/usr/lib64/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
