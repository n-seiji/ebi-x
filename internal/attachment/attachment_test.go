package attachment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveAcceptsFilesInThreadAreas(t *testing.T) {
	base := t.TempDir()
	workspaceDir := filepath.Join(base, "workspace", "C1-1.2")
	checkout := filepath.Join(base, "checkouts", "C1-1.2", "slides-abcd")
	writeFile(t, filepath.Join(workspaceDir, "out", "preview.png"), "png")
	writeFile(t, filepath.Join(checkout, "dist", "deck.PPTX"), "pptx")

	files, rejections := Resolve([]string{workspaceDir, checkout}, workspaceDir, []string{
		"out/preview.png",
		filepath.Join(checkout, "dist", "deck.PPTX"),
		filepath.Join(workspaceDir, "out", "..", "out", "preview.png"),
	})
	if len(rejections) != 0 {
		t.Fatalf("rejections = %+v, want none", rejections)
	}
	if len(files) != 2 || files[0].Name != "preview.png" || files[1].Name != "deck.PPTX" || files[1].Size != 4 {
		t.Fatalf("files = %+v, want preview.png and deck.PPTX once each", files)
	}
	content, err := files[1].Open()
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	content.Close()
}

func TestResolveRejectsUnsafeFiles(t *testing.T) {
	base := t.TempDir()
	workspaceDir := filepath.Join(base, "workspace", "C1-1.2")
	other := filepath.Join(base, "workspace", "C1-9.9")
	writeFile(t, filepath.Join(other, "secret.pdf"), "other thread")
	writeFile(t, filepath.Join(base, "secret.png"), "outside")
	writeFile(t, filepath.Join(workspaceDir, ".env"), "TOKEN=x")
	writeFile(t, filepath.Join(workspaceDir, "empty.pdf"), "")
	if err := os.MkdirAll(filepath.Join(workspaceDir, "dir.pdf"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "secret.pdf"), filepath.Join(workspaceDir, "link.pdf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(workspaceDir, "linked")); err != nil {
		t.Fatal(err)
	}
	// A link that stays inside the area still hides what is sent.
	writeFile(t, filepath.Join(workspaceDir, "real.pdf"), "pdf")
	if err := os.Symlink(filepath.Join(workspaceDir, "real.pdf"), filepath.Join(workspaceDir, "alias.pdf")); err != nil {
		t.Fatal(err)
	}

	tests := map[string]string{
		"../C1-9.9/secret.pdf":                   "作業領域外",
		filepath.Join(other, "secret.pdf"):       "作業領域外",
		filepath.Join(base, "secret.png"):        "作業領域外",
		"link.pdf":                               "作業領域外",
		"linked/secret.pdf":                      "作業領域外",
		"alias.pdf":                              "作業領域外",
		".env":                                   "種類",
		"empty.pdf":                              "空",
		"dir.pdf":                                "通常のファイルではありません",
		"missing.pdf":                            "見つかりません",
		filepath.Join(workspaceDir, "notes.txt"): "種類",
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			files, rejections := Resolve([]string{workspaceDir}, workspaceDir, []string{path})
			if len(files) != 0 {
				t.Fatalf("files = %+v, want none", files)
			}
			if len(rejections) != 1 || !strings.Contains(rejections[0].Reason, want) {
				t.Fatalf("rejections = %+v, want reason containing %q", rejections, want)
			}
		})
	}
}

func TestResolveLimitsFileCount(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := range MaxFiles + 2 {
		path := filepath.Join(dir, string(rune('a'+i))+".png")
		writeFile(t, path, "png")
		paths = append(paths, path)
	}
	files, rejections := Resolve([]string{dir}, dir, paths)
	if len(files) != MaxFiles || len(rejections) != 2 {
		t.Fatalf("got %d files and %d rejections, want %d and 2", len(files), len(rejections), MaxFiles)
	}
}

func TestOpenRejectsReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deck.pdf")
	writeFile(t, path, "approved")
	files, _ := Resolve([]string{dir}, dir, []string{path})
	if len(files) != 1 {
		t.Fatal("file was not resolved")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// The replacement may reuse the inode, so only its content differs.
	writeFile(t, path, "swapped!")
	later := files[0].info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if content, err := files[0].Open(); err == nil {
		content.Close()
		t.Fatal("Open() accepted a file replaced after validation")
	}
}
