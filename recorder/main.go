package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type Config struct {
	DataDir       string
	Addr          string
	SegmentSec    int
	RetentionDays int
	MaxGB         float64
	FFmpeg        string
	Remote        string // contoh: gdrive:nvr (kosong = tanpa upload)
	Rclone        string
	BufferHours   int
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func loadConfig() Config {
	seg, _ := strconv.Atoi(env("SEGMENT_SECONDS", "300"))
	days, _ := strconv.Atoi(env("RETENTION_DAYS", "7"))
	gb, _ := strconv.ParseFloat(env("MAX_STORAGE_GB", "0"), 64)
	buf, _ := strconv.Atoi(env("LOCAL_BUFFER_HOURS", "24"))
	return Config{
		Remote:        strings.TrimRight(env("RCLONE_REMOTE", ""), "/"),
		Rclone:        env("RCLONE_BIN", "rclone"),
		BufferHours:   buf,
		DataDir:       env("DATA_DIR", "./data"),
		Addr:          env("RECORDER_ADDR", "127.0.0.1:8081"),
		SegmentSec:    seg,
		RetentionDays: days,
		MaxGB:         gb,
		FFmpeg:        env("FFMPEG_BIN", "ffmpeg"),
	}
}

type Camera struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	RTSPURL string `json:"rtsp_url"`
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
}

type Recording struct {
	ID       int64  `json:"id"`
	CameraID int64  `json:"camera_id"`
	Start    int64  `json:"start"` // unix seconds
	Size     int64  `json:"size"`
	Path     string `json:"-"`
	Uploaded bool   `json:"uploaded"`
	Local    bool   `json:"local"`
}

type App struct {
	cfg     Config
	db      *sql.DB
	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
	rootCtx context.Context
}

func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

func (a *App) initDB() error {
	_, err := a.db.Exec(`
CREATE TABLE IF NOT EXISTS cameras (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  rtsp_url TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS recordings (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  camera_id INTEGER NOT NULL,
  start INTEGER NOT NULL,
  size INTEGER NOT NULL,
  path TEXT NOT NULL UNIQUE
);
CREATE INDEX IF NOT EXISTS idx_rec_cam_start ON recordings(camera_id, start);`)
	if err != nil {
		return err
	}
	// Migrasi untuk database lama; error "duplicate column" diabaikan.
	a.db.Exec(`ALTER TABLE recordings ADD COLUMN uploaded INTEGER NOT NULL DEFAULT 0`)
	a.db.Exec(`ALTER TABLE recordings ADD COLUMN local INTEGER NOT NULL DEFAULT 1`)
	return nil
}

func (a *App) camDir(id int64) string {
	return filepath.Join(a.cfg.DataDir, "recordings", strconv.FormatInt(id, 10))
}

// runCamera menjalankan ffmpeg untuk satu kamera dan restart dengan backoff jika berhenti.
func (a *App) runCamera(ctx context.Context, c Camera) {
	dir := a.camDir(c.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("camera %d: mkdir: %v", c.ID, err)
		return
	}
	backoff := time.Second
	for ctx.Err() == nil {
		args := []string{
			"-hide_banner", "-loglevel", "error",
			"-rtsp_transport", "tcp", "-rw_timeout", "10000000",
			"-i", c.RTSPURL,
			"-c", "copy",
			"-f", "segment", "-segment_time", strconv.Itoa(a.cfg.SegmentSec),
			"-segment_atclocktime", "1", "-reset_timestamps", "1",
			"-strftime", "1",
			"-segment_format", "mp4",
			"-segment_format_options", "movflags=+frag_keyframe+empty_moov+default_base_moof",
			filepath.Join(dir, "%Y%m%d_%H%M%S.mp4"),
		}
		cmd := exec.CommandContext(ctx, a.cfg.FFmpeg, args...)
		start := time.Now()
		log.Printf("camera %d (%s): start recording", c.ID, c.Name)
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			return
		}
		// Jangan log URL (mengandung kredensial); log hanya output ffmpeg.
		log.Printf("camera %d: ffmpeg exited (%v): %s", c.ID, err, strings.ReplaceAll(strings.TrimSpace(string(out)), c.RTSPURL, maskURL(c.RTSPURL)))
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *App) startCamera(c Camera) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.cancels[c.ID]; ok || !c.Enabled {
		return
	}
	ctx, cancel := context.WithCancel(a.rootCtx)
	a.cancels[c.ID] = cancel
	go a.runCamera(ctx, c)
}

func (a *App) stopCamera(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cancel, ok := a.cancels[id]; ok {
		cancel()
		delete(a.cancels, id)
	}
}

func (a *App) isRunning(id int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.cancels[id]
	return ok
}

func (a *App) getCamera(id int64) (Camera, error) {
	var c Camera
	err := a.db.QueryRow(`SELECT id,name,rtsp_url,enabled FROM cameras WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.RTSPURL, &c.Enabled)
	return c, err
}

// indexLoop mengindeks segmen yang sudah selesai ditulis dan menerapkan retensi.
func (a *App) indexLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		a.indexOnce()
		a.uploadOnce(ctx)
		a.retainOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *App) indexOnce() {
	root := filepath.Join(a.cfg.DataDir, "recordings")
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".mp4") {
			return nil
		}
		info, err := d.Info()
		if err != nil || time.Since(info.ModTime()) < 20*time.Second || info.Size() == 0 {
			return nil // masih ditulis
		}
		camID, err := strconv.ParseInt(filepath.Base(filepath.Dir(p)), 10, 64)
		if err != nil {
			return nil
		}
		ts, err := time.ParseInLocation("20060102_150405", strings.TrimSuffix(d.Name(), ".mp4"), time.Local)
		if err != nil {
			return nil
		}
		a.db.Exec(`INSERT INTO recordings(camera_id,start,size,path) VALUES(?,?,?,?)
ON CONFLICT(path) DO UPDATE SET size=excluded.size`, camID, ts.Unix(), info.Size(), p)
		return nil
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /cameras", func(w http.ResponseWriter, r *http.Request) {
		rows, err := a.db.Query(`SELECT id,name,rtsp_url,enabled FROM cameras ORDER BY id`)
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		defer rows.Close()
		list := []Camera{}
		for rows.Next() {
			var c Camera
			rows.Scan(&c.ID, &c.Name, &c.RTSPURL, &c.Enabled)
			c.RTSPURL = maskURL(c.RTSPURL)
			c.Running = a.isRunning(c.ID)
			list = append(list, c)
		}
		writeJSON(w, 200, list)
	})

	mux.HandleFunc("POST /cameras", func(w http.ResponseWriter, r *http.Request) {
		var in Camera
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil ||
			strings.TrimSpace(in.Name) == "" || !strings.HasPrefix(in.RTSPURL, "rtsp") {
			httpErr(w, 400, "name dan rtsp_url (rtsp:// atau rtsps://) wajib diisi")
			return
		}
		res, err := a.db.Exec(`INSERT INTO cameras(name,rtsp_url,enabled) VALUES(?,?,?)`, in.Name, in.RTSPURL, in.Enabled)
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		in.ID, _ = res.LastInsertId()
		a.startCamera(in)
		in.RTSPURL = maskURL(in.RTSPURL)
		in.Running = a.isRunning(in.ID)
		writeJSON(w, 201, in)
	})

	mux.HandleFunc("DELETE /cameras/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		a.stopCamera(id)
		a.db.Exec(`DELETE FROM cameras WHERE id=?`, id)
		w.WriteHeader(204)
	})

	mux.HandleFunc("POST /cameras/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		c, err := a.getCamera(id)
		if err != nil {
			httpErr(w, 404, "kamera tidak ditemukan")
			return
		}
		switch r.PathValue("action") {
		case "start":
			a.db.Exec(`UPDATE cameras SET enabled=1 WHERE id=?`, id)
			c.Enabled = true
			a.startCamera(c)
		case "stop":
			a.db.Exec(`UPDATE cameras SET enabled=0 WHERE id=?`, id)
			a.stopCamera(id)
		default:
			httpErr(w, 404, "aksi tidak dikenal")
			return
		}
		w.WriteHeader(204)
	})

	mux.HandleFunc("GET /recordings", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
		to, _ := strconv.ParseInt(q.Get("to"), 10, 64)
		if to == 0 {
			to = time.Now().Unix() + 1
		}
		cam, _ := strconv.ParseInt(q.Get("camera_id"), 10, 64)
		rows, err := a.db.Query(`SELECT id,camera_id,start,size,uploaded,local FROM recordings
WHERE (?=0 OR camera_id=?) AND start>=? AND start<=? ORDER BY start DESC LIMIT 500`, cam, cam, from, to)
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		defer rows.Close()
		list := []Recording{}
		for rows.Next() {
			var x Recording
			rows.Scan(&x.ID, &x.CameraID, &x.Start, &x.Size, &x.Uploaded, &x.Local)
			list = append(list, x)
		}
		writeJSON(w, 200, list)
	})

	mux.HandleFunc("GET /recordings/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		p, err := a.fileForPlayback(r.Context(), id)
		if err != nil {
			code := 500
			if errors.Is(err, os.ErrNotExist) {
				code = 404
			}
			log.Printf("playback %d: %v", id, err)
			httpErr(w, code, "rekaman tidak tersedia")
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeFile(w, r, p) // mendukung Range request
	})

	return mux
}

func main() {
	cfg := loadConfig()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(cfg.DataDir, "nvr.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := &App{cfg: cfg, db: db, cancels: map[int64]context.CancelFunc{}, rootCtx: ctx}
	if err := a.initDB(); err != nil {
		log.Fatal(err)
	}

	rows, err := db.Query(`SELECT id,name,rtsp_url,enabled FROM cameras WHERE enabled=1`)
	if err != nil {
		log.Fatal(err)
	}
	var cams []Camera
	for rows.Next() {
		var c Camera
		rows.Scan(&c.ID, &c.Name, &c.RTSPURL, &c.Enabled)
		cams = append(cams, c)
	}
	rows.Close()
	for _, c := range cams {
		a.startCamera(c)
	}
	go a.indexLoop(ctx)

	srv := &http.Server{Addr: cfg.Addr, Handler: a.routes()}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		srv.Shutdown(sctx)
	}()
	log.Printf("recorder listening on %s, data=%s", cfg.Addr, cfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
