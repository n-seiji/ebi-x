// Package attachment validates files a work turn asks the bot to upload to
// Slack. Only regular files with an allowed extension inside the Slack
// thread's own work areas qualify, so a path, a symbolic link, or a file
// swapped in after validation cannot send another thread's files or secrets.
package attachment

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// MaxFiles is the most files one work turn may attach.
	MaxFiles = 10
	// MaxSize is the largest file the bot uploads. Slack accepts more, but
	// slide previews and decks stay well below this.
	MaxSize = 100 << 20
)

// allowedExtensions are the deliverable types the bot uploads. Anything
// else, such as source files or .env, is rejected even inside a work area.
var allowedExtensions = map[string]struct{}{
	".pdf":  {},
	".png":  {},
	".jpg":  {},
	".jpeg": {},
	".gif":  {},
	".webp": {},
	".pptx": {},
}

// File is a validated attachment.
type File struct {
	// Path is the absolute path the work turn listed.
	Path string
	// Name is the file name shown in Slack.
	Name string
	Size int64
	info fs.FileInfo
}

// Rejection explains why a listed path will not be uploaded.
type Rejection struct {
	Path   string
	Reason string
}

// Resolve validates paths listed by a work turn. Relative paths are relative
// to cwd. Each file must lie inside one of roots without passing through a
// symbolic link below that root. Duplicates are attached once, and paths past
// MaxFiles are rejected.
func Resolve(roots []string, cwd string, paths []string) ([]File, []Rejection) {
	type area struct{ lexical, real string }
	var areas []area
	for _, root := range roots {
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		areas = append(areas, area{lexical: filepath.Clean(root), real: real})
	}

	var files []File
	var rejections []Rejection
	seen := make(map[string]struct{})
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		if len(files) >= MaxFiles {
			rejections = append(rejections, Rejection{Path: path, Reason: fmt.Sprintf("1回の添付は%d件までです", MaxFiles)})
			continue
		}
		file, err := resolveOne(path, func(path, real string) bool {
			for _, area := range areas {
				lexicalRel, ok := within(area.lexical, path)
				if !ok {
					continue
				}
				// The real path must match the lexical one below the root, so
				// no symbolic link inside the work area redirects the file.
				if realRel, ok := within(area.real, real); ok && realRel == lexicalRel {
					return true
				}
			}
			return false
		})
		if err != nil {
			rejections = append(rejections, Rejection{Path: path, Reason: err.Error()})
			continue
		}
		files = append(files, file)
	}
	return files, rejections
}

func resolveOne(path string, inArea func(path, real string) bool) (File, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if _, ok := allowedExtensions[ext]; !ok {
		return File{}, errors.New("添付できない種類のファイルです")
	}
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, errors.New("ファイルが見つかりません")
	} else if err != nil {
		return File{}, errors.New("ファイルを確認できません")
	}
	if !inArea(path, real) {
		return File{}, errors.New("このスレッドの作業領域外のファイルです")
	}
	info, err := os.Lstat(real)
	if err != nil {
		return File{}, errors.New("ファイルを確認できません")
	}
	if !info.Mode().IsRegular() {
		return File{}, errors.New("通常のファイルではありません")
	}
	if info.Size() == 0 {
		return File{}, errors.New("ファイルが空です")
	}
	if info.Size() > MaxSize {
		return File{}, fmt.Errorf("%dMBを超えています", MaxSize>>20)
	}
	return File{Path: real, Name: filepath.Base(real), Size: info.Size(), info: info}, nil
}

// Open opens a validated file and checks it is still the file that was
// validated, so a file replaced after Resolve is not uploaded.
func (f File) Open() (*os.File, error) {
	file, err := os.Open(f.Path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if f.info == nil || !os.SameFile(info, f.info) || !info.Mode().IsRegular() ||
		info.Size() != f.Size || !info.ModTime().Equal(f.info.ModTime()) {
		file.Close()
		return nil, errors.New("検証後にファイルが変更されました")
	}
	return file, nil
}

// within reports path relative to root when path is root itself or below it.
func within(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}
