package media

import (
	"path/filepath"
	"regexp"
	"strings"
)

var (
	bracketRegex = regexp.MustCompile(`\(\d+\)$`)
	editedRegex  = regexp.MustCompile(`(-[a-zA-Z\x{00C0}-\x{00FF}]+)(\(\d+\))?$`)
)

// GetTakeoutJSONCandidates returns potential Takeout sidecar JSON paths for a given media file.
// Ports the 5 proven rules from immich-go and Google Takeout reverse engineering:
// 1. Standard patterns & supplemental metadata (.supplemental-metadata.json, -supplemental-metadata.json)
// 2. Bracket swapping for numbered duplicates (e.g. IMG_1234(1).jpg -> IMG_1234.jpg(1).json)
// 3. Takeout character limits (46, 47, 51 chars) & partial extension truncations
// 4. Edited photos (e.g. IMG_1234-edited.jpg falling back to IMG_1234.json)
// 5. Live/Motion photos (.mp4/.mov pairing with .heic/.jpg JSONs)
func GetTakeoutJSONCandidates(mediaPath string) []string {
	dir := filepath.Dir(mediaPath)
	filename := filepath.Base(mediaPath)
	suffix := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, suffix)

	var candidates []string

	// Case sensitivity variations for extension (common in Takeout archives on Linux)
	suffixesToTry := []string{suffix}
	lowerSfx := strings.ToLower(suffix)
	upperSfx := strings.ToUpper(suffix)
	if lowerSfx != suffix {
		suffixesToTry = append(suffixesToTry, lowerSfx)
	}
	if upperSfx != suffix && upperSfx != lowerSfx {
		suffixesToTry = append(suffixesToTry, upperSfx)
	}

	// 1. Standard patterns and supplemental metadata
	for _, sfx := range suffixesToTry {
		candidates = append(candidates, filepath.Join(dir, stem+sfx+".json"))
		candidates = append(candidates, filepath.Join(dir, stem+sfx+".supplemental-metadata.json"))
		candidates = append(candidates, filepath.Join(dir, stem+sfx+"-supplemental-metadata.json"))
	}
	candidates = append(candidates, filepath.Join(dir, stem+".json"))
	candidates = append(candidates, filepath.Join(dir, stem+".supplemental-metadata.json"))
	candidates = append(candidates, filepath.Join(dir, stem+"-supplemental-metadata.json"))

	// 2. Numbered duplicates with bracket swapping: e.g. "IMG_1234(1).jpg" -> "IMG_1234.jpg(1).json"
	if loc := bracketRegex.FindStringIndex(stem); loc != nil {
		idx := stem[loc[0]:loc[1]] // e.g. "(1)"
		baseStem := stem[:loc[0]]
		for _, sfx := range suffixesToTry {
			candidates = append(candidates, filepath.Join(dir, baseStem+sfx+idx+".json"))
			candidates = append(candidates, filepath.Join(dir, baseStem+idx+sfx+".json"))
			candidates = append(candidates, filepath.Join(dir, baseStem+sfx+".supplemental-metadata"+idx+".json"))
			candidates = append(candidates, filepath.Join(dir, baseStem+sfx+"-supplemental-metadata"+idx+".json"))
		}
		candidates = append(candidates, filepath.Join(dir, baseStem+".supplemental-metadata"+idx+".json"))
		candidates = append(candidates, filepath.Join(dir, baseStem+"-supplemental-metadata"+idx+".json"))
	}

	// 3. Truncated names (Google Takeout 46, 47, 51 character limits)
	nameRunes := []rune(filename)
	stemRunes := []rune(stem)
	for _, length := range []int{46, 47, 51} {
		if len(nameRunes) >= length {
			candidates = append(candidates, filepath.Join(dir, string(nameRunes[:length])+".json"))
		}
		if len(stemRunes) >= length {
			candidates = append(candidates, filepath.Join(dir, string(stemRunes[:length])+".json"))
		}
	}

	// Partial extension cuts (.j, .jp, .jpe, etc.)
	for _, sfx := range suffixesToTry {
		for cutLen := 1; cutLen < len(sfx); cutLen++ {
			candidates = append(candidates, filepath.Join(dir, stem+sfx[:cutLen]+".json"))
		}
	}

	// 4. Edited photos without their own JSON (e.g. IMG_001-edited.jpg)
	if loc := editedRegex.FindStringSubmatchIndex(stem); loc != nil && loc[0] > 0 {
		cleanStem := stem[:loc[0]]
		numberSuffix := ""
		if loc[4] != -1 && loc[5] != -1 {
			numberSuffix = stem[loc[4]:loc[5]]
		}
		for _, sfx := range suffixesToTry {
			candidates = append(candidates, filepath.Join(dir, cleanStem+sfx+numberSuffix+".json"))
			candidates = append(candidates, filepath.Join(dir, cleanStem+sfx+".supplemental-metadata"+numberSuffix+".json"))
			candidates = append(candidates, filepath.Join(dir, cleanStem+sfx+"-supplemental-metadata"+numberSuffix+".json"))
		}
		candidates = append(candidates, filepath.Join(dir, cleanStem+numberSuffix+".json"))
		candidates = append(candidates, filepath.Join(dir, cleanStem+".supplemental-metadata"+numberSuffix+".json"))
		candidates = append(candidates, filepath.Join(dir, cleanStem+"-supplemental-metadata"+numberSuffix+".json"))
	}

	// 5. Live / Motion Photo cross-extension (MP4/MOV paired with HEIC/JPG)
	if lowerSfx == ".mp4" || lowerSfx == ".mov" {
		for _, photoExt := range []string{".heic", ".HEIC", ".jpg", ".JPG", ".jpeg", ".JPEG"} {
			candidates = append(candidates, filepath.Join(dir, stem+photoExt+".json"))
			candidates = append(candidates, filepath.Join(dir, stem+photoExt+".supplemental-metadata.json"))
			candidates = append(candidates, filepath.Join(dir, stem+photoExt+"-supplemental-metadata.json"))
		}
	}

	// Return unique candidates preserving order
	seen := make(map[string]bool)
	var uniqueCandidates []string
	for _, c := range candidates {
		if !seen[c] {
			seen[c] = true
			uniqueCandidates = append(uniqueCandidates, c)
		}
	}

	return uniqueCandidates
}
