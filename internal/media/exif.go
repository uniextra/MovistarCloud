package media

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	VideoExtensions = map[string]bool{
		".mp4":  true,
		".mov":  true,
		".avi":  true,
		".mkv":  true,
		".m4v":  true,
		".3gp":  true,
		".webm": true,
		".wmv":  true,
		".flv":  true,
		".mts":  true,
	}

	PhotoExtensions = map[string]bool{
		".jpg":  true,
		".jpeg": true,
		".png":  true,
		".gif":  true,
		".bmp":  true,
		".webp": true,
		".heic": true,
		".heif": true,
		".tiff": true,
		".tif":  true,
		".dng":  true,
		".cr2":  true,
		".nef":  true,
		".arw":  true,
		".raw":  true,
	}
)

type TakeoutMetadata struct {
	PhotoTakenTime *struct {
		Timestamp string `json:"timestamp"`
	} `json:"photoTakenTime"`
	CreationTime *struct {
		Timestamp string `json:"timestamp"`
	} `json:"creationTime"`
}

// CaptureTime returns the best capture date for a media file:
// 1. Google Takeout sidecar JSON photoTakenTime (if isTakeout == true)
// 2. Video modification date (Funambol patchCreationDateForVideos rule)
// 3. EXIF DateTimeOriginal / DateTimeDigitized / DateTime for photos
// 4. Fallback to file system ModTime
func CaptureTime(filePath string, isTakeout bool) time.Time {
	fi, err := os.Stat(filePath)
	if err != nil {
		return time.Now().UTC()
	}
	defaultTime := fi.ModTime().UTC()

	// 1. Takeout mode: inspect JSON sidecars
	if isTakeout {
		candidates := GetTakeoutJSONCandidates(filePath)
		for _, cand := range candidates {
			data, err := os.ReadFile(cand)
			if err != nil {
				continue
			}

			var meta TakeoutMetadata
			if err := json.Unmarshal(data, &meta); err == nil {
				var tsStr string
				if meta.PhotoTakenTime != nil && meta.PhotoTakenTime.Timestamp != "" {
					tsStr = meta.PhotoTakenTime.Timestamp
				} else if meta.CreationTime != nil && meta.CreationTime.Timestamp != "" {
					tsStr = meta.CreationTime.Timestamp
				}

				if tsStr != "" {
					if ts, err := strconv.ParseInt(tsStr, 10, 64); err == nil && ts > 0 {
						return time.Unix(ts, 0).UTC()
					}
				}
			}
		}
	}

	ext := strings.ToLower(filepath.Ext(filePath))

	// 2. Official rule for videos: patchCreationDateForVideos
	// Container MP4/MOV metadata often has 00:00 UTC or corrupt epochs.
	if VideoExtensions[ext] {
		return defaultTime
	}

	// 3. EXIF for JPEG/TIFF photos
	if ext == ".jpg" || ext == ".jpeg" || ext == ".tiff" || ext == ".tif" {
		if exifTime, ok := readJPEGExifDate(filePath); ok {
			return exifTime
		}
	}

	return defaultTime
}

func FormatAPIDate(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

func MimeFor(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".heic":
		return "image/heic"
	case ".heif":
		return "image/heif"
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".avi":
		return "video/x-msvideo"
	case ".mkv":
		return "video/x-matroska"
	case ".3gp":
		return "video/3gpp"
	case ".webm":
		return "video/webm"
	default:
		m := mime.TypeByExtension(ext)
		if m != "" {
			return m
		}
		return "application/octet-stream"
	}
}

// Pure Go lightweight EXIF parser for JPEG files
func readJPEGExifDate(filePath string) (time.Time, bool) {
	f, err := os.Open(filePath)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()

	// Read SOI marker (0xFF, 0xD8)
	var soi [2]byte
	if _, err := io.ReadFull(f, soi[:]); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return time.Time{}, false
	}

	// Search for APP1 marker (0xFF, 0xE1)
	for {
		var marker [2]byte
		if _, err := io.ReadFull(f, marker[:]); err != nil {
			return time.Time{}, false
		}
		if marker[0] != 0xFF {
			return time.Time{}, false
		}

		// Skip fill bytes
		for marker[1] == 0xFF {
			if _, err := io.ReadFull(f, marker[1:]); err != nil {
				return time.Time{}, false
			}
		}

		var segLen uint16
		if err := binary.Read(f, binary.BigEndian, &segLen); err != nil {
			return time.Time{}, false
		}
		if segLen < 2 {
			return time.Time{}, false
		}
		dataLen := int(segLen) - 2

		if marker[1] == 0xE1 { // APP1 Exif
			payload := make([]byte, dataLen)
			if _, err := io.ReadFull(f, payload); err != nil {
				return time.Time{}, false
			}
			return parseExifPayload(payload)
		} else if marker[1] == 0xDA || marker[1] == 0xD9 { // SOS (Start of Scan) or EOI
			break
		} else {
			// Skip other segments
			if _, err := f.Seek(int64(dataLen), io.SeekCurrent); err != nil {
				return time.Time{}, false
			}
		}
	}

	return time.Time{}, false
}

func parseExifPayload(payload []byte) (time.Time, bool) {
	if len(payload) < 14 || !bytes.Equal(payload[:6], []byte("Exif\x00\x00")) {
		return time.Time{}, false
	}

	tiffData := payload[6:]
	var byteOrder binary.ByteOrder
	if bytes.Equal(tiffData[:2], []byte("II")) {
		byteOrder = binary.LittleEndian
	} else if bytes.Equal(tiffData[:2], []byte("MM")) {
		byteOrder = binary.BigEndian
	} else {
		return time.Time{}, false
	}

	magic := byteOrder.Uint16(tiffData[2:4])
	if magic != 42 {
		return time.Time{}, false
	}

	ifdOffset := byteOrder.Uint32(tiffData[4:8])
	dateStr, ok := parseIFDTags(tiffData, int(ifdOffset), byteOrder, 0)
	if !ok || dateStr == "" {
		return time.Time{}, false
	}

	// Parse EXIF date format "2006:01:02 15:04:05"
	cleaned := strings.TrimRight(strings.TrimSpace(dateStr), "\x00")
	t, err := time.Parse("2006:01:02 15:04:05", cleaned)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func parseIFDTags(tiffData []byte, offset int, order binary.ByteOrder, depth int) (string, bool) {
	if depth > 3 || offset+2 > len(tiffData) {
		return "", false
	}

	numEntries := int(order.Uint16(tiffData[offset : offset+2]))
	pos := offset + 2

	var exifSubIFDOffset int
	var foundDate string

	for i := 0; i < numEntries; i++ {
		if pos+12 > len(tiffData) {
			break
		}
		tag := order.Uint16(tiffData[pos : pos+2])
		count := order.Uint32(tiffData[pos+4 : pos+8])
		valBytes := tiffData[pos+8 : pos+12]

		if tag == 0x8769 { // ExifIFD pointer
			exifSubIFDOffset = int(order.Uint32(valBytes))
		} else if (tag == 0x9003 || tag == 0x9004 || tag == 0x0132) && foundDate == "" {
			// DateTimeOriginal (0x9003), DateTimeDigitized (0x9004), DateTime (0x0132)
			if count > 4 {
				dataOff := int(order.Uint32(valBytes))
				if dataOff+int(count) <= len(tiffData) {
					foundDate = string(tiffData[dataOff : dataOff+int(count)])
				}
			} else {
				foundDate = string(valBytes[:count])
			}
		}

		pos += 12
	}

	if exifSubIFDOffset > 0 {
		if subDate, ok := parseIFDTags(tiffData, exifSubIFDOffset, order, depth+1); ok && subDate != "" {
			return subDate, true
		}
	}

	if foundDate != "" {
		return foundDate, true
	}

	return "", false
}
