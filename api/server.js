import path from 'node:path';
import { fileURLToPath } from 'node:url';
import express from 'express';
import cookieParser from 'cookie-parser';
import jwt from 'jsonwebtoken';
import { createProxyMiddleware } from 'http-proxy-middleware';
import { UserStore } from './users.js';

const here = path.dirname(fileURLToPath(import.meta.url));

const {
  PORT = '3000',
  RECORDER_URL = 'http://127.0.0.1:8081',
  MEDIAMTX_HLS_URL = 'http://127.0.0.1:8888',
  ADMIN_USER = 'admin',
  ADMIN_PASSWORD,
  JWT_SECRET,
  COOKIE_SECURE = 'false',
  STATE_DIR = path.join(here, 'state'),
} = process.env;

if (!JWT_SECRET || JWT_SECRET.length < 32) {
  console.error('JWT_SECRET (min. 32 karakter) wajib diisi.');
  process.exit(1);
}

const users = new UserStore(STATE_DIR);
// Admin awal dibuat dari .env hanya jika belum ada pengguna sama sekali.
if (users.isEmpty()) {
  if (!ADMIN_PASSWORD) {
    console.error('Belum ada pengguna: isi ADMIN_PASSWORD di .env untuk membuat admin awal.');
    process.exit(1);
  }
  users.create(ADMIN_USER, ADMIN_PASSWORD, 'admin');
  console.log(`admin awal "${ADMIN_USER}" dibuat`);
}

const app = express();
app.disable('x-powered-by');
app.set('trust proxy', 1);
app.use(cookieParser());
const json = express.json({ limit: '4kb' });

// Pembatasan login sederhana: 10 percobaan gagal per IP per 15 menit.
const fails = new Map();
const WINDOW = 15 * 60 * 1000;
function tooMany(ip) {
  const e = fails.get(ip);
  return e && Date.now() - e.t < WINDOW && e.n >= 10;
}
function noteFail(ip) {
  const e = fails.get(ip);
  if (!e || Date.now() - e.t >= WINDOW) fails.set(ip, { n: 1, t: Date.now() });
  else e.n++;
}

const COOKIE = 'nvr_session';
function setSession(res, username, u) {
  const token = jwt.sign({ sub: username, tv: u.tv }, JWT_SECRET, { expiresIn: '12h' });
  res.cookie(COOKIE, token, {
    httpOnly: true,
    sameSite: 'strict',
    secure: COOKIE_SECURE === 'true',
    maxAge: 12 * 3600 * 1000,
  });
}

app.post('/auth/login', json, (req, res) => {
  if (tooMany(req.ip)) return res.status(429).json({ error: 'terlalu banyak percobaan, coba lagi nanti' });
  const username = String(req.body?.username ?? '');
  const u = users.verify(username, String(req.body?.password ?? ''));
  if (!u) {
    noteFail(req.ip);
    return res.status(401).json({ error: 'username atau password salah' });
  }
  fails.delete(req.ip);
  setSession(res, username, u);
  res.json({ user: username, role: u.role });
});

app.post('/auth/logout', (req, res) => {
  res.clearCookie(COOKIE);
  res.json({ ok: true });
});

// Sesi valid hanya jika pengguna masih ada dan versi token cocok (berubah saat password diganti).
// Role selalu diambil dari penyimpanan, jadi perubahan role langsung berlaku.
function requireAuth(req, res, next) {
  try {
    const p = jwt.verify(req.cookies[COOKIE] ?? '', JWT_SECRET);
    const u = users.get(p.sub);
    if (!u || u.tv !== p.tv) throw new Error('stale');
    req.user = { username: p.sub, role: u.role };
    next();
  } catch {
    res.status(401).json({ error: 'belum login' });
  }
}

function requireAdmin(req, res, next) {
  if (req.user.role !== 'admin') return res.status(403).json({ error: 'hanya admin' });
  next();
}

const wrap = (fn) => (req, res) => {
  try {
    fn(req, res);
  } catch (e) {
    res.status(400).json({ error: e.message });
  }
};

app.get('/healthz', (req, res) => res.json({ status: 'ok' }));
app.get('/auth/me', requireAuth, (req, res) => res.json({ user: req.user.username, role: req.user.role }));

// Ganti password sendiri (semua role).
app.post('/auth/password', requireAuth, json, wrap((req, res) => {
  const { current = '', password = '' } = req.body ?? {};
  if (!users.verify(req.user.username, String(current))) {
    return res.status(400).json({ error: 'password lama salah' });
  }
  users.setPassword(req.user.username, String(password));
  setSession(res, req.user.username, users.get(req.user.username));
  res.json({ ok: true });
}));

// Manajemen pengguna (admin).
app.get('/users', requireAuth, requireAdmin, (req, res) => res.json(users.list()));
app.post('/users', requireAuth, requireAdmin, json, wrap((req, res) => {
  const { username = '', password = '', role = 'viewer' } = req.body ?? {};
  users.create(String(username).trim(), String(password), String(role));
  res.status(201).json({ ok: true });
}));
app.put('/users/:name', requireAuth, requireAdmin, json, wrap((req, res) => {
  const name = req.params.name;
  const { role, password } = req.body ?? {};
  if (role !== undefined) {
    if (name === req.user.username && role !== 'admin') throw new Error('tidak bisa menurunkan role diri sendiri');
    users.setRole(name, String(role));
  }
  if (password) users.setPassword(name, String(password));
  res.json({ ok: true });
}));
app.delete('/users/:name', requireAuth, requireAdmin, wrap((req, res) => {
  if (req.params.name === req.user.username) throw new Error('tidak bisa menghapus akun sendiri');
  users.remove(req.params.name);
  res.status(204).end();
}));

// API recorder: viewer hanya boleh GET; perubahan (tambah/hapus/start/stop kamera) khusus admin.
app.use(
  '/api',
  requireAuth,
  (req, res, next) => (req.method === 'GET' || req.user.role === 'admin' ? next() : res.status(403).json({ error: 'hanya admin' })),
  createProxyMiddleware({
    target: RECORDER_URL,
    changeOrigin: true,
    pathRewrite: { '^/api': '' },
  }),
);

// Live view (HLS) dari mediamtx, hanya GET dan wajib login.
app.use(
  '/live',
  requireAuth,
  (req, res, next) => (req.method === 'GET' ? next() : res.status(405).end()),
  createProxyMiddleware({
    target: MEDIAMTX_HLS_URL,
    changeOrigin: true,
    pathRewrite: { '^/live': '' },
    on: {
      proxyRes: (proxyRes) => {
        // mediamtx mengarahkan ke /<path>?cookieCheck=1; tambahkan prefix /live agar tetap lewat proxy.
        const loc = proxyRes.headers.location;
        if (loc && loc.startsWith('/')) proxyRes.headers.location = '/live' + loc;
        // Cookie sesi HLS mediamtx tidak diperlukan (tanpa cookie mediamtx tetap melayani playlist).
        delete proxyRes.headers['set-cookie'];
      },
    },
  }),
);

app.use('/vendor/hls', express.static(path.join(here, 'node_modules/hls.js/dist')));
app.use(express.static(path.join(here, 'public')));

app.listen(Number(PORT), () => console.log(`api listening on :${PORT}, recorder=${RECORDER_URL}`));
