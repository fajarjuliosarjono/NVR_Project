# NVR

NVR sederhana: `recorder` (Go + ffmpeg) merekam RTSP ke segmen MP4, `api` (Node.js) menangani login, proxy API, dan web UI.

```
browser -> api (Node :3000, auth) -> recorder (Go :8081, ffmpeg, SQLite) -> data/
```

## Jalankan lokal
Butuh Go, Node 20+, ffmpeg (dan rclone jika memakai upload).
```sh
cd recorder && go run .            # http://127.0.0.1:8081
cd api && npm install
ADMIN_PASSWORD=rahasia JWT_SECRET=$(openssl rand -hex 32) npm start   # http://localhost:3000
```

## Deploy (Docker)
```sh
cp .env.example .env   # isi ADMIN_PASSWORD dan JWT_SECRET
docker compose up -d --build
```
UI di `127.0.0.1:3000`. Pasang reverse proxy HTTPS di depannya dan set `COOKIE_SECURE=true`. Jangan expose port recorder.

## Konfigurasi recorder
`DATA_DIR`, `RECORDER_ADDR`, `SEGMENT_SECONDS`, `RETENTION_DAYS`, `MAX_STORAGE_GB`, `LOCAL_BUFFER_HOURS`, `RCLONE_REMOTE`, `RCLONE_BIN`, `FFMPEG_BIN`.

## Penyimpanan: buffer lokal + Google Drive (rclone)
Rekaman ditulis ke disk lokal, segmen yang selesai diunggah dengan `rclone copyto` ke `RCLONE_REMOTE`, dan salinan lokal dihapus setelah `LOCAL_BUFFER_HOURS` (atau saat melebihi `MAX_STORAGE_GB`, hanya yang sudah terunggah). `RETENTION_DAYS` menghapus rekaman lokal dan remote. Rekaman yang hanya ada di Drive diunduh sementara ke `data/cache` saat diputar.

Setup:
1. Install rclone di mesin yang punya browser, jalankan `rclone config`, buat remote bernama `gdrive` (tipe drive). Disarankan tambah remote `crypt` di atasnya agar file terenkripsi.
2. Salin `~/.config/rclone/rclone.conf` ke `./rclone/rclone.conf` di server (folder ini di-gitignore, jangan di-commit).
3. Set `RCLONE_REMOTE=gdrive:nvr` di `.env`.

Jika upload gagal (mis. internet putus), segmen tetap di lokal dan dicoba lagi tiap 30 detik. Pastikan disk lokal cukup untuk menampung rekaman selama gangguan.

## Belum ada
Live view (rencana: mediamtx + HLS/WebRTC), deteksi gerakan, multi-user.
