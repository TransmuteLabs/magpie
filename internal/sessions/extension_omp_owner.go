package sessions

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func extensionOMPFileOwners(files []file) []file {
	if len(files) == 0 {
		return files
	}
	owners := sessionOMPMainFiles(files, extensionOMPSessionRoots(nil), "pi")
	for index := range files {
		item := &files[index]
		if item.agent == "pi" && sessionOMPHasMain(*item, owners) {
			item.agent = "omp"
			item.key = "omp:" + strings.TrimPrefix(item.key, "pi:")
		}
	}
	return files
}

func extensionOMPSharedFiles(files []file, root string) []file {
	before, err := os.Stat(root)
	if err != nil {
		return files
	}
	other, err := os.Stat(filepath.Join(PiDir(), "sessions"))
	sharedRoot := err == nil && os.SameFile(before, other)
	flat := piSessionDir()
	flatInfo, flatErr := os.Stat(flat)
	if !sharedRoot && flatErr != nil {
		return files
	}
	shared := map[string][]string{}
	for _, item := range files {
		relative, err := filepath.Rel(root, item.path)
		if !item.main || item.agent != "omp" || err != nil || !filepath.IsLocal(relative) || relative == "." {
			continue
		}
		ambiguous := sharedRoot || flat != "" && filepath.Clean(filepath.Dir(item.path)) == filepath.Clean(flat)
		if !ambiguous && flatErr == nil {
			parent, err := os.Stat(filepath.Dir(item.path))
			ambiguous = err == nil && os.SameFile(parent, flatInfo)
		}
		if ambiguous {
			shared[item.key] = append(shared[item.key], item.path)
		}
	}
	if len(shared) == 0 {
		return files
	}
	owners := sessionOMPMainFiles(files, []string{root}, "omp")
	out := files[:0]
	for _, item := range files {
		if item.agent == "omp" && sessionOMPHasMain(item, shared) && !sessionOMPHasMain(item, owners) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func sessionOMPMainFiles(files []file, roots []string, agent string) map[string][]string {
	owners := map[string][]string{}
	for _, item := range files {
		if !item.main || item.agent != agent {
			continue
		}
		for _, root := range roots {
			relative, err := filepath.Rel(root, item.path)
			if err == nil && filepath.IsLocal(relative) && relative != "." {
				if sessionOMPFixedTitle(item.path) {
					owners[item.key] = append(owners[item.key], item.path)
				}
				break
			}
		}
	}
	return owners
}

func sessionOMPHasMain(item file, owners map[string][]string) bool {
	for _, main := range owners[item.key] {
		if item.path == main || strings.HasPrefix(item.path, strings.TrimSuffix(main, ".jsonl")+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func sessionOMPFixedTitle(path string) bool {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 256 {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return false
	}
	var body [256]byte
	if _, err := io.ReadFull(file, body[:]); err != nil || body[255] != '\n' ||
		bytes.ContainsRune(body[:255], '\n') || !utf8.Valid(body[:]) {
		return false
	}
	after, err := file.Stat()
	current, currentErr := os.Lstat(path)
	if err != nil || currentErr != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) ||
		opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return false
	}
	var slot struct {
		Type      string          `json:"type"`
		Version   int             `json:"v"`
		Title     *string         `json:"title"`
		UpdatedAt *string         `json:"updatedAt"`
		Pad       *string         `json:"pad"`
		Source    json.RawMessage `json:"source"`
	}
	if json.Unmarshal(body[:], &slot) != nil || slot.Type != "title" || slot.Version != 1 ||
		slot.Title == nil || slot.UpdatedAt == nil || slot.Pad == nil || strings.Trim(*slot.Pad, " ") != "" {
		return false
	}
	if len(slot.Source) != 0 {
		var source string
		if json.Unmarshal(slot.Source, &source) != nil || source != "auto" && source != "user" {
			return false
		}
	}
	return true
}
