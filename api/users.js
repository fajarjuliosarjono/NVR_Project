import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

// Penyimpanan pengguna dalam file JSON (jumlah pengguna kecil). Password di-hash dengan scrypt.
// Role: "admin" (akses penuh) atau "viewer" (hanya baca).

export const ROLES = ['admin', 'viewer'];
const USERNAME_RE = /^[a-zA-Z0-9._-]{3,32}$/;
export const MIN_PASSWORD = 8;

function hashPassword(password) {
  const salt = crypto.randomBytes(16);
  const hash = crypto.scryptSync(password, salt, 64);
  return `scrypt$${salt.toString('base64')}$${hash.toString('base64')}`;
}

function verifyHash(password, stored) {
  const [alg, s, h] = String(stored).split('$');
  if (alg !== 'scrypt' || !s || !h) return false;
  const expected = Buffer.from(h, 'base64');
  const actual = crypto.scryptSync(password, Buffer.from(s, 'base64'), expected.length);
  return crypto.timingSafeEqual(actual, expected);
}

// Hash tiruan agar waktu respons login sama untuk username yang tidak ada.
const DUMMY_HASH = hashPassword(crypto.randomBytes(16).toString('hex'));

export class UserStore {
  constructor(dir) {
    this.file = path.join(dir, 'users.json');
    fs.mkdirSync(dir, { recursive: true });
    this.users = {};
    if (fs.existsSync(this.file)) {
      this.users = JSON.parse(fs.readFileSync(this.file, 'utf8')).users ?? {};
    }
  }

  save() {
    const tmp = this.file + '.tmp';
    fs.writeFileSync(tmp, JSON.stringify({ users: this.users }, null, 2), { mode: 0o600 });
    fs.renameSync(tmp, this.file);
  }

  isEmpty() {
    return Object.keys(this.users).length === 0;
  }

  get(username) {
    return Object.hasOwn(this.users, username) ? this.users[username] : undefined;
  }

  list() {
    return Object.entries(this.users)
      .map(([username, u]) => ({ username, role: u.role, created: u.created }))
      .sort((a, b) => a.username.localeCompare(b.username));
  }

  adminCount() {
    return Object.values(this.users).filter((u) => u.role === 'admin').length;
  }

  verify(username, password) {
    const u = this.get(username);
    const ok = verifyHash(password, u ? u.hash : DUMMY_HASH);
    return ok && u ? u : null;
  }

  create(username, password, role) {
    if (!USERNAME_RE.test(username)) throw new Error('username 3-32 karakter: huruf, angka, titik, minus, garis bawah');
    if (this.get(username)) throw new Error('username sudah dipakai');
    if (!ROLES.includes(role)) throw new Error('role tidak valid');
    if (String(password).length < MIN_PASSWORD) throw new Error(`password minimal ${MIN_PASSWORD} karakter`);
    this.users[username] = { hash: hashPassword(password), role, tv: 1, created: Date.now() };
    this.save();
  }

  setPassword(username, password) {
    const u = this.get(username);
    if (!u) throw new Error('pengguna tidak ditemukan');
    if (String(password).length < MIN_PASSWORD) throw new Error(`password minimal ${MIN_PASSWORD} karakter`);
    u.hash = hashPassword(password);
    u.tv = (u.tv ?? 1) + 1; // membatalkan sesi lama
    this.save();
  }

  setRole(username, role) {
    const u = this.get(username);
    if (!u) throw new Error('pengguna tidak ditemukan');
    if (!ROLES.includes(role)) throw new Error('role tidak valid');
    if (u.role === 'admin' && role !== 'admin' && this.adminCount() <= 1) throw new Error('harus ada minimal satu admin');
    u.role = role;
    this.save();
  }

  remove(username) {
    const u = this.get(username);
    if (!u) throw new Error('pengguna tidak ditemukan');
    if (u.role === 'admin' && this.adminCount() <= 1) throw new Error('harus ada minimal satu admin');
    delete this.users[username];
    this.save();
  }
}
