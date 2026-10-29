package media

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type FileItem struct {
	Path string
	Info fs.FileInfo
}

// IsMediaFile checks if a file has a recognized photo/video extension and is not a Takeout thumbnail.
func IsMediaFile(filename string, size int64) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	if !PhotoExtensions[ext] && !VideoExtensions[ext] {
		return false
	}

	// Filter out Google Takeout auto-generated low-res thumbnails
	lowerName := strings.ToLower(filename)
	if strings.Contains(lowerName, "thumbnail") && size < 40960 {
		return false
	}

	return true
}

// ScanMediaFiles traverses a directory (recursively or non-recursively) and yields media files.
func ScanMediaFiles(root string, recursive bool, onItem func(item FileItem) error) error {
	if !recursive {
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if IsMediaFile(e.Name(), info.Size()) {
				fullPath := filepath.Join(root, e.Name())
				if err := onItem(FileItem{Path: fullPath, Info: info}); err != nil {
					return err
				}
			}
		}
		return nil
	}

	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // continue walking even if some file gives permission error
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if IsMediaFile(d.Name(), info.Size()) {
			return onItem(FileItem{Path: path, Info: info})
		}
		return nil
	})
}
