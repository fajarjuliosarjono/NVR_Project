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

### Cara kerja
1. ffmpeg merekam ke disk lokal (`data/recordings/<id_kamera>/YYYYMMDD_HHMMSS.mp4`).
2. Setiap 30 detik, segmen yang sudah selesai ditulis diunggah dengan `rclone copyto` ke `RCLONE_REMOTE/<id_kamera>/<nama_file>`. Jika gagal (internet putus, kuota, dll.), segmen tetap di lokal dan dicoba lagi pada siklus berikutnya.
3. Salinan lokal dihapus jika sudah terunggah **dan** salah satu kondisi ini terpenuhi: umurnya melebihi `LOCAL_BUFFER_HOURS`, atau total buffer lokal melebihi `MAX_STORAGE_GB`. Segmen yang belum terunggah tidak pernah dihapus oleh batas ukuran.
4. `RETENTION_DAYS` menghapus rekaman dari lokal, dari Drive, dan dari database.
5. Saat rekaman yang hanya ada di Drive diputar, file diunduh ke `data/cache/` lalu diputar dari sana. Cache dibersihkan setelah 1 jam.

Jika `RCLONE_REMOTE` kosong, tidak ada upload dan semua rekaman tetap di disk lokal.

### Variabel terkait
| Variabel | Default | Arti |
|---|---|---|
| `RCLONE_REMOTE` | kosong | Tujuan upload, format `namaremote:folder`, mis. `gdrive:nvr`. Kosong = upload mati. |
| `LOCAL_BUFFER_HOURS` | 24 | Lama salinan lokal dipertahankan setelah terunggah. |
| `MAX_STORAGE_GB` | 0 | Batas ukuran buffer lokal. 0 = tanpa batas. |
| `RETENTION_DAYS` | 7 | Total masa simpan (lokal + Drive). |
| `RCLONE_BIN` | rclone | Lokasi biner rclone. |
| `RCLONE_CONFIG` | (default rclone) | Lokasi `rclone.conf`. Di Docker sudah diset ke `/config/rclone.conf`. |

### Setup langkah demi langkah

**1. Pasang rclone** (di mesin yang punya browser, mis. Ubuntu Desktop server Anda):
```sh
sudo apt install -y rclone     # atau: curl https://rclone.org/install.sh | sudo bash
rclone version
```

**2. Buat remote Google Drive**
```sh
rclone config
```
Jawab pertanyaannya kira-kira seperti ini:
- `n` (New remote), name: `gdrive`
- Storage: ketik `drive` (Google Drive)
- `client_id` dan `client_secret`: kosongkan untuk mencoba cepat. Untuk pemakaian jangka panjang, buat OAuth client sendiri di Google Cloud Console (lihat https://rclone.org/drive/#making-your-own-client-id), karena client bawaan rclone dipakai bersama dan sering kena pembatasan.
- `scope`: pilih `1` (full access) atau `3` (`drive.file`, hanya file yang dibuat rclone, lebih aman).
- `root_folder_id`, `service_account_file`: kosongkan.
- `Edit advanced config`: `n`
- `Use web browser to automatically authenticate`: `y`. Browser terbuka, login ke akun Google, lalu izinkan.
- `Configure as Shared Drive`: `n` (ketik `y` jika memakai Shared Drive Workspace)
- Simpan dengan `y`, lalu `q`.

Server tanpa browser (headless): pilih `n` pada pertanyaan auto-authenticate, lalu jalankan `rclone authorize "drive"` di komputer lain yang punya browser dan tempel token hasilnya.

Jika aplikasi OAuth Anda berstatus **Testing** di Google Cloud Console, token kedaluwarsa dalam 7 hari dan upload akan berhenti. Ubah status ke **In production** agar token tidak kedaluwarsa.

**3. (Disarankan) Enkripsi dengan remote `crypt`**
Rekaman kamera bersifat sensitif. Buat remote kedua di atas Drive:
```sh
rclone config
```
- `n`, name: `gdrive-crypt`
- Storage: `crypt`
- `remote`: `gdrive:nvr`
- Filename encryption: `standard`, directory name encryption: `true`
- Password: pilih `g` (generate), lalu buat juga salt dengan `g`

**Simpan kedua password (password dan salt) di password manager.** Tanpa keduanya, rekaman di Drive tidak bisa dibuka lagi. Dengan crypt, set `RCLONE_REMOTE=gdrive-crypt:`.

**4. Tes koneksi**
```sh
rclone lsd gdrive:                                  # menampilkan folder di Drive
echo tes > /tmp/tes.txt
rclone copyto /tmp/tes.txt gdrive-crypt:tes.txt     # unggah
rclone ls gdrive-crypt:                             # harus tampil tes.txt
rclone deletefile gdrive-crypt:tes.txt              # hapus
```

**5. Pasang konfigurasi di server**
- Docker: salin file konfigurasi ke folder proyek.
  ```sh
  mkdir -p rclone && cp ~/.config/rclone/rclone.conf rclone/rclone.conf
  chmod 600 rclone/rclone.conf
  ```
  Folder `rclone/` sudah ada di `.gitignore`. **Jangan pernah commit `rclone.conf`**, isinya token akses ke Drive Anda. File ini harus bisa ditulis oleh container karena rclone memperbarui token di dalamnya.
- Tanpa Docker: biarkan di `~/.config/rclone/rclone.conf` milik user yang menjalankan recorder, atau tunjuk dengan `RCLONE_CONFIG`.

**6. Aktifkan di `.env`**
```
RCLONE_REMOTE=gdrive-crypt:
LOCAL_BUFFER_HOURS=24
RETENTION_DAYS=14
MAX_STORAGE_GB=50
```
Lalu `docker compose up -d --build`.

**7. Pastikan berjalan**
```sh
docker compose logs -f recorder       # cari baris "upload ... gagal" jika ada masalah
rclone ls gdrive-crypt: | head        # file muncul setelah segmen pertama selesai (default 5 menit + sampai 30 detik)
```
Di UI, kolom lokasi rekaman berubah dari `lokal` menjadi `lokal + drive`, dan menjadi `drive` setelah salinan lokal dihapus.

### Batasan dan catatan
- Google Drive membatasi unggahan sekitar 750 GB per hari per akun, dan kuota gratis hanya 15 GB. Hitung kebutuhan: kamera 4 Mbps menghasilkan sekitar 43 GB per hari.
- Rclone memindahkan file yang dihapus ke Trash Drive, yang tetap memakan kuota sampai dikosongkan. Compose sudah mengatur `RCLONE_DRIVE_USE_TRASH=false` agar penghapusan oleh retensi langsung permanen.
- Disk lokal harus cukup untuk menampung rekaman selama upload tertunda. Jika `MAX_STORAGE_GB` terlampaui dan belum ada segmen yang terunggah, recorder hanya mencatat peringatan di log dan tidak menghapus apa pun.
- Memutar rekaman lama butuh waktu unduh dulu (ukuran segmen 5 menit biasanya puluhan MB).

### Masalah umum
| Gejala | Penyebab dan solusi |
|---|---|
| `upload N gagal: ... didn't find section in config file` | `rclone.conf` tidak terbaca. Periksa mount `./rclone:/config` dan nama remote di `RCLONE_REMOTE`. |
| `invalid_grant` / `token expired` | Token Drive kedaluwarsa atau dicabut. Jalankan `rclone config reconnect gdrive:` lalu salin ulang `rclone.conf`. Lihat catatan status Testing di atas. |
| `rateLimitExceeded` / `userRateLimitExceeded` | Kena batas API. Buat OAuth client sendiri, atau perbesar `SEGMENT_SECONDS`. |
| `storageQuotaExceeded` | Kuota Drive penuh. Kurangi `RETENTION_DAYS` atau tambah kapasitas. |
| Rekaman tidak muncul di Drive | Cek log recorder, pastikan `RCLONE_REMOTE` terisi dan `rclone` ada di container (`docker compose exec recorder rclone version`). |

## Belum ada
Live view (rencana: mediamtx + HLS/WebRTC), deteksi gerakan, multi-user.
