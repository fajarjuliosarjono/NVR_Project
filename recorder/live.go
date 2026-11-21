package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Live view: setiap kamera didaftarkan sebagai path "cam<ID>" di mediamtx lewat Control API.
// Sumber ditarik sesuai permintaan (sourceOnDemand), jadi kamera hanya dibuka saat ada penonton.
// Konfigurasi API mediamtx tidak persisten, maka reconcile berkala menyusun ulang setelah restart.

func (a *App) liveEnabled() bool { return a.cfg.MediaMTXAPI != "" }

func livePath(id int64) string { return "cam" + strconv.FormatInt(id, 10) }

func (a *App) mtx(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.MediaMTXAPI+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

func (a *App) reconcileLive(ctx context.Context) {
	if !a.liveEnabled() {
		return
	}
	a.liveMu.Lock()
	defer a.liveMu.Unlock()

	rows, err := a.db.Query(`SELECT id,rtsp_url FROM cameras`)
	if err != nil {
		return
	}
	want := map[string]string{}
	for rows.Next() {
		var id int64
		var u string
		rows.Scan(&id, &u)
		want[livePath(id)] = u
	}
	rows.Close()

	code, body, err := a.mtx(ctx, http.MethodGet, "/v3/config/paths/list?itemsPerPage=1000", nil)
	if err != nil || code != 200 {
		if ctx.Err() == nil {
			log.Printf("live: mediamtx belum siap (%v, status %d)", err, code)
		}
		return
	}
	var list struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	json.Unmarshal(body, &list)
	have := map[string]bool{}
	for _, it := range list.Items {
		have[it.Name] = true
	}

	for name, u := range want {
		if have[name] {
			continue
		}
		code, out, err := a.mtx(ctx, http.MethodPost, "/v3/config/paths/add/"+name, map[string]any{
			"source":                     u,
			"sourceOnDemand":             true,
			"sourceOnDemandStartTimeout": "15s",
			"sourceOnDemandCloseAfter":   "20s",
			"rtspTransport":              "tcp",
		})
		if err != nil || code != 200 {
			log.Printf("live: tambah %s gagal (%v, status %d): %s", name, err, code,
				strings.ReplaceAll(strings.TrimSpace(string(out)), u, maskURL(u)))
		}
	}
	for name := range have {
		if _, ok := want[name]; !ok && strings.HasPrefix(name, "cam") {
			a.mtx(ctx, http.MethodDelete, "/v3/config/paths/delete/"+name, nil)
		}
	}
}

func (a *App) triggerLive() {
	if a.liveEnabled() {
		go a.reconcileLive(a.rootCtx)
	}
}

func (a *App) liveLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		a.reconcileLive(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
