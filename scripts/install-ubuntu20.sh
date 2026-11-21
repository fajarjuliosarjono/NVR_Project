#!/usr/bin/env bash
# Pasang semua dependensi NVR di Ubuntu 20.04 (Docker, rclone, git, dll.).
# Jalankan: sudo bash scripts/install-ubuntu20.sh
# Aman dijalankan ulang. Tidak memformat atau me-mount disk apa pun.
set -euo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "Jalankan dengan sudo: sudo bash $0" >&2
  exit 1
fi

. /etc/os-release
if [[ "${ID:-}" != "ubuntu" ]]; then
  echo "Script ini untuk Ubuntu (terdeteksi: ${ID:-?})." >&2
  exit 1
fi
CODENAME="${VERSION_CODENAME:-focal}"
TARGET_USER="${SUDO_USER:-}"
export DEBIAN_FRONTEND=noninteractive

echo "==> Paket dasar"
apt-get update -y
apt-get install -y ca-certificates curl gnupg git ufw mmc-utils unzip

echo "==> Docker Engine + Compose plugin (repo resmi Docker)"
install -m 0755 -d /etc/apt/keyrings
if [[ ! -f /etc/apt/keyrings/docker.asc ]]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
fi
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${CODENAME} stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update -y
apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
systemctl enable --now docker
if [[ -n "$TARGET_USER" ]]; then
  usermod -aG docker "$TARGET_USER"
fi

echo "==> rclone (versi terbaru; versi apt di 20.04 terlalu lama untuk Google Drive)"
if ! command -v rclone >/dev/null 2>&1; then
  curl -fsSL https://rclone.org/install.sh | bash
fi

echo "==> Matikan sleep/suspend agar perekaman tidak berhenti"
systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target

echo "==> Batasi log sistem ke RAM (mengurangi tulisan ke eMMC/flashdisk)"
mkdir -p /etc/systemd/journald.conf.d
cat > /etc/systemd/journald.conf.d/nvr.conf <<'EOF'
[Journal]
Storage=volatile
RuntimeMaxUse=100M
EOF
systemctl restart systemd-journald

echo "==> Firewall: izinkan SSH saja (port 3000/8081 tidak dibuka)"
ufw allow OpenSSH
ufw --force enable

echo "==> TRIM berkala"
systemctl enable --now fstrim.timer || true

echo
echo "Versi terpasang:"
docker --version
docker compose version
rclone version | head -1
git --version
echo
echo "Selesai. Logout lalu login lagi agar grup 'docker' aktif, kemudian lanjut ke langkah clone repo dan .env."
