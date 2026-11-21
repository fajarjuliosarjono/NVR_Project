package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Alur: rekam ke disk lokal (buffer) -> unggah segmen selesai ke remote (rclone)
// -> hapus salinan lokal setelah melewati BufferHours atau saat melebihi MaxGB.

func (a *App) remoteEnabled() bool { return a.cfg.Remote != "" }

func (a *App) remotePath(camID int64, file string) string {
	return a.cfg.Remote + "/" + strconv.FormatInt(camID, 10) + "/" + filepath.Base(file)
}

func (a *App) rclone(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return exec.CommandContext(ctx, a.cfg.Rclone, args...).CombinedOutput()
}

type recRow struct {
	id       int64
	cam      int64
	path     string
	uploaded bool
	local    bool
}

func (a *App) queryRows(query string, args ...any) []recRow {
	rows, err := a.db.Query(query, args...)
	if err != nil {
		log.Printf("query: %v", err)
		return nil
	}
	defer rows.Close()
	var out []recRow
	for rows.Next() {
		var r recRow
		rows.Scan(&r.id, &r.cam, &r.path, &r.uploaded, &r.local)
		out = append(out, r)
	}
	return out
}

const selCols = `SELECT id,camera_id,path,uploaded,local FROM recordings `

func (a *App) uploadOnce(ctx context.Context) {
	if !a.remoteEnabled() {
		return
	}
	for _, r := range a.queryRows(selCols + `WHERE uploaded=0 AND local=1 ORDER BY start LIMIT 20`) {
		if ctx.Err() != nil {
			return
		}
		out, err := a.rclone(ctx, 30*time.Minute, "copyto", r.path, a.remotePath(r.cam, r.path))
		if err != nil {
			log.Printf("upload %d gagal: %v: %s", r.id, err, strings.TrimSpace(string(out)))
			return // coba lagi di siklus berikutnya
		}
		a.db.Exec(`UPDATE recordings SET uploaded=1 WHERE id=?`, r.id)
	}
}

func (a *App) removeLocal(r recRow) bool {
	if err := os.Remove(r.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("hapus lokal %s: %v", r.path, err)
		return false
	}
	return true
}

func (a *App) retainOnce(ctx context.Context) {
	// 1. Retensi total: hapus lokal + remote + baris DB untuk rekaman yang melewati RETENTION_DAYS.
	if a.cfg.RetentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -a.cfg.RetentionDays).Unix()
		for _, r := range a.queryRows(selCols+`WHERE start<?`, cutoff) {
			if r.local && !a.removeLocal(r) {
				continue
			}
			if r.uploaded {
				out, err := a.rclone(ctx, 2*time.Minute, "deletefile", a.remotePath(r.cam, r.path))
				if err != nil && !strings.Contains(string(out), "not found") {
					log.Printf("hapus remote %d: %v", r.id, err)
					continue
				}
			}
			a.db.Exec(`DELETE FROM recordings WHERE id=?`, r.id)
		}
	}

	// 2. Eviksi buffer lokal.
	if a.remoteEnabled() {
		if a.cfg.BufferHours > 0 {
			cutoff := time.Now().Add(-time.Duration(a.cfg.BufferHours) * time.Hour).Unix()
			for _, r := range a.queryRows(selCols+`WHERE uploaded=1 AND local=1 AND start<?`, cutoff) {
				if a.removeLocal(r) {
					a.db.Exec(`UPDATE recordings SET local=0 WHERE id=?`, r.id)
				}
			}
		}
		if a.cfg.MaxGB > 0 {
			limit := int64(a.cfg.MaxGB * 1024 * 1024 * 1024)
			for {
				var total int64
				a.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM recordings WHERE local=1`).Scan(&total)
				if total <= limit {
					break
				}
				rs := a.queryRows(selCols + `WHERE uploaded=1 AND local=1 ORDER BY start LIMIT 1`)
				if len(rs) == 0 {
					log.Printf("PERINGATAN: buffer lokal melebihi MAX_STORAGE_GB tetapi belum ada segmen yang terunggah")
					break
				}
				if !a.removeLocal(rs[0]) {
					break
				}
				a.db.Exec(`UPDATE recordings SET local=0 WHERE id=?`, rs[0].id)
			}
		}
	} else if a.cfg.MaxGB > 0 {
		// Tanpa remote: hapus rekaman terlama saat melebihi batas.
		limit := int64(a.cfg.MaxGB * 1024 * 1024 * 1024)
		for {
			var total int64
			a.db.QueryRow(`SELECT COALESCE(SUM(size),0) FROM recordings`).Scan(&total)
			if total <= limit {
				break
			}
			rs := a.queryRows(selCols + `ORDER BY start LIMIT 1`)
			if len(rs) == 0 || !a.removeLocal(rs[0]) {
				break
			}
			a.db.Exec(`DELETE FROM recordings WHERE id=?`, rs[0].id)
		}
	}

	a.cleanCache()
}

// Cache unduhan untuk pemutaran rekaman yang sudah tidak ada di lokal.
func (a *App) cacheDir() string { return filepath.Join(a.cfg.DataDir, "cache") }

func (a *App) cleanCache() {
	entries, _ := os.ReadDir(a.cacheDir())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.Remove(filepath.Join(a.cacheDir(), e.Name()))
		}
	}
}

var dlLocks sync.Map // id -> *sync.Mutex

// fileForPlayback mengembalikan path file lokal; mengunduh dari remote jika perlu.
func (a *App) fileForPlayback(ctx context.Context, id int64) (string, error) {
	rs := a.queryRows(selCols+`WHERE id=?`, id)
	if len(rs) == 0 {
		return "", os.ErrNotExist
	}
	r := rs[0]
	if r.local {
		if _, err := os.Stat(r.path); err == nil {
			return r.path, nil
		}
	}
	if !r.uploaded || !a.remoteEnabled() {
		return "", os.ErrNotExist
	}
	dst := filepath.Join(a.cacheDir(), fmt.Sprintf("%d.mp4", id))
	mu, _ := dlLocks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if _, err := os.Stat(dst); err == nil {
		os.Chtimes(dst, time.Now(), time.Now())
		return dst, nil
	}
	if err := os.MkdirAll(a.cacheDir(), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".part"
	if out, err := a.rclone(ctx, 10*time.Minute, "copyto", a.remotePath(r.cam, r.path), tmp); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("unduh remote: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return dst, os.Rename(tmp, dst)
}
