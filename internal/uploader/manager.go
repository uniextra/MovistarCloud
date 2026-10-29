package uploader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"movistar-cloud-uploader/internal/cache"
	"movistar-cloud-uploader/internal/config"
	"movistar-cloud-uploader/internal/logger"
	"movistar-cloud-uploader/internal/media"
	"movistar-cloud-uploader/internal/sapi"
)

type JobStats struct {
	State          string    `json:"state"`
	Phone          string    `json:"phone"`
	Folder         string    `json:"folder"`
	Recursive      bool      `json:"recursive"`
	Takeout        bool      `json:"takeout"`
	TotalFiles     int64     `json:"total_files"`
	ProcessedFiles int64     `json:"processed_files"`
	UploadedCount  int64     `json:"uploaded_count"`
	SkippedCount   int64     `json:"skipped_count"`
	ErrorCount     int64     `json:"error_count"`
	CurrentFile    string    `json:"current_file"`
	StartTime      time.Time `json:"start_time"`
	DurationSec    int64     `json:"duration_sec"`
	Percent        float64   `json:"percent"`
}

type JobManager struct {
	client     *sapi.Client
	mu         sync.RWMutex
	cancelFunc context.CancelFunc

	state       string
	phone       string
	folder      string
	recursive   bool
	takeout     bool
	currentFile string
	startTime   time.Time

	totalFiles     int64
	processedFiles int64
	uploadedCount  int64
	skippedCount   int64
	errorCount     int64
}

func NewJobManager(client *sapi.Client) *JobManager {
	return &JobManager{
		client: client,
		state:  "idle",
	}
}

func (m *JobManager) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == "scanning" || m.state == "running"
}

func (m *JobManager) GetStatus() JobStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var duration int64
	if !m.startTime.IsZero() {
		duration = int64(time.Since(m.startTime).Seconds())
	}

	total := atomic.LoadInt64(&m.totalFiles)
	processed := atomic.LoadInt64(&m.processedFiles)
	var percent float64
	if total > 0 {
		percent = float64(processed) / float64(total) * 100.0
		if percent > 100.0 {
			percent = 100.0
		}
	}

	return JobStats{
		State:          m.state,
		Phone:          m.phone,
		Folder:         m.folder,
		Recursive:      m.recursive,
		Takeout:        m.takeout,
		TotalFiles:     total,
		ProcessedFiles: processed,
		UploadedCount:  atomic.LoadInt64(&m.uploadedCount),
		SkippedCount:   atomic.LoadInt64(&m.skippedCount),
		ErrorCount:     atomic.LoadInt64(&m.errorCount),
		CurrentFile:    m.currentFile,
		StartTime:      m.startTime,
		DurationSec:    duration,
		Percent:        percent,
	}
}

func (m *JobManager) StopJob() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancelFunc != nil {
		m.cancelFunc()
		m.cancelFunc = nil
	}
	if m.state == "running" || m.state == "scanning" {
		m.state = "stopped"
		logger.Log("[Job] ■ Subida detenida por el usuario.")
	}
}

func (m *JobManager) StartJob(phone, folder string, recursive, takeout bool) error {
	m.mu.Lock()
	if m.state == "scanning" || m.state == "running" {
		m.mu.Unlock()
		return fmt.Errorf("ya hay un proceso de subida en curso")
	}

	phoneClean := config.SanitizePhone(phone)
	if phoneClean == "" {
		m.mu.Unlock()
		return fmt.Errorf("teléfono inválido")
	}

	accountCfg := config.LoadAccountConfig(phoneClean)
	if accountCfg.RefreshToken == "" && (accountCfg.JSessionID == "" || accountCfg.ValidationKey == "") {
		m.mu.Unlock()
		return fmt.Errorf("la cuenta no tiene credenciales configuradas")
	}

	m.client.SetConfig(accountCfg)

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel
	m.state = "scanning"
	m.phone = phoneClean
	m.folder = folder
	m.recursive = recursive
	m.takeout = takeout
	m.currentFile = ""
	m.startTime = time.Now()

	atomic.StoreInt64(&m.totalFiles, 0)
	atomic.StoreInt64(&m.processedFiles, 0)
	atomic.StoreInt64(&m.uploadedCount, 0)
	atomic.StoreInt64(&m.skippedCount, 0)
	atomic.StoreInt64(&m.errorCount, 0)
	m.mu.Unlock()

	go m.runPipeline(ctx, phoneClean, folder, recursive, takeout)
	return nil
}

func (m *JobManager) runPipeline(ctx context.Context, phone, folder string, recursive, takeout bool) {
	defer func() {
		m.mu.Lock()
		if m.state == "running" || m.state == "scanning" {
			m.state = "completed"
			logger.Log("[Job] ✔ Proceso de subida finalizado.")
		}
		m.mu.Unlock()
	}()

	logger.Log("[Job] ▶ Iniciando tarea para cuenta %s en carpeta: %s (recursivo=%v, takeout=%v)", phone, folder, recursive, takeout)

	// 1. Initialize SQLite Cache
	dbPath := filepath.Join(config.TokensDir, fmt.Sprintf("cache_%s.sqlite", phone))
	uploadCache, err := cache.OpenCache(dbPath)
	if err != nil {
		logger.Log("[Cache] ⚠️ Error abriendo caché SQLite principal (%v), intentando memoria...", err)
		uploadCache, _ = cache.OpenCache(":memory:")
	}
	defer uploadCache.Close()

	// 2. Fetch cloud inventory for duplicate control
	cloudItems, err := m.client.GetGalleryInventory()
	if err == nil && len(cloudItems) > 0 {
		var cacheImports []cache.CloudItemInfo
		for _, it := range cloudItems {
			cacheImports = append(cacheImports, cache.CloudItemInfo{
				Name:    it.Name,
				Size:    it.Size,
				CloudID: it.ID,
			})
		}
		_ = uploadCache.ImportCloudItems(cacheImports)
	}

	// 3. Scan local files
	logger.Log("[Scan] 🔍 Escaneando archivos multimedia en %s...", folder)
	var files []string
	err = media.ScanMediaFiles(folder, recursive, func(item media.FileItem) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		files = append(files, item.Path)
		return nil
	})

	if err != nil && err != context.Canceled {
		logger.Log("[Scan] ❌ Error escaneando archivos: %v", err)
		m.mu.Lock()
		m.state = "error"
		m.mu.Unlock()
		return
	}

	total := int64(len(files))
	atomic.StoreInt64(&m.totalFiles, total)
	logger.Log("[Scan] ✔ Se encontraron %d archivos multimedia para procesar.", total)

	if total == 0 {
		return
	}

	m.mu.Lock()
	m.state = "running"
	m.mu.Unlock()

	// 4. Worker pool with 3 parallel workers
	numWorkers := 3
	jobs := make(chan string, 100)
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for path := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}

				m.mu.Lock()
				m.currentFile = filepath.Base(path)
				m.mu.Unlock()

				m.processFile(ctx, path, takeout, uploadCache)
			}
		}(w)
	}

	// Feed files to workers
enqueueLoop:
	for _, p := range files {
		select {
		case <-ctx.Done():
			break enqueueLoop
		case jobs <- p:
		}
	}
	close(jobs)

	wg.Wait()
}

func (m *JobManager) processFile(ctx context.Context, filePath string, takeout bool, c *cache.UploadCache) {
	defer atomic.AddInt64(&m.processedFiles, 1)

	fileName := filepath.Base(filePath)
	proc := atomic.LoadInt64(&m.processedFiles) + 1
	total := atomic.LoadInt64(&m.totalFiles)
	prefix := fmt.Sprintf("[%d/%d]", proc, total)

	// 1. Persistent cache duplicate check
	if c.IsUploaded(filePath, true) {
		logger.Log("%s [SKIP] %s (Ya registrado en caché persistente)", prefix, fileName)
		atomic.AddInt64(&m.skippedCount, 1)
		return
	}

	// 2. Prepare upload metadata
	capTime := media.CaptureTime(filePath, takeout)
	mimeType := media.MimeFor(filePath)

	logger.Log("%s [UPLOAD] Subiendo %s (%s, %s)...", prefix, fileName, mimeType, capTime.Format("2006-01-02 15:04:05"))

	// 3. Upload directly to Gallery
	res, err := m.client.UploadToGallery(ctx, filePath, capTime, mimeType)
	if err != nil {
		if err == context.Canceled {
			return
		}
		logger.Log("%s [ERROR] Falló subida de %s: %v", prefix, fileName, err)
		atomic.AddInt64(&m.errorCount, 1)
		return
	}

	// 4. Record success in local SQLite cache
	var cloudID *int64
	if res.ID > 0 {
		cloudID = &res.ID
	}
	_ = c.MarkUploaded(filePath, cloudID, "ok", res.ETag)
	atomic.AddInt64(&m.uploadedCount, 1)
	logger.Log("%s [OK] %s subido exitosamente (ID: %d)", prefix, fileName, res.ID)
}

// GetDirectoryChildren returns child directories of a given path restricted under allowed root.
func GetDirectoryChildren(root, current string) ([]string, error) {
	target := filepath.Clean(current)
	if !strings.HasPrefix(target, filepath.Clean(root)) {
		target = root
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, filepath.Join(target, e.Name()))
		}
	}
	return dirs, nil
}
