import crypto from 'node:crypto';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import express from 'express';
import cookieParser from 'cookie-parser';
import jwt from 'jsonwebtoken';
import { createProxyMiddleware } from 'http-proxy-middleware';

const {
  PORT = '3000',
  RECORDER_URL = 'http://127.0.0.1:8081',
  MEDIAMTX_HLS_URL = 'http://127.0.0.1:8888',
  ADMIN_USER = 'admin',
  ADMIN_PASSWORD,
  JWT_SECRET,
  COOKIE_SECURE = 'false',
} = process.env;

if (!ADMIN_PASSWORD || !JWT_SECRET || JWT_SECRET.length < 32) {
  console.error('ADMIN_PASSWORD dan JWT_SECRET (min. 32 karakter) wajib diisi.');
  process.exit(1);
}

const sha = (s) => crypto.createHash('sha256').update(s).digest();
const safeEqual = (a, b) => crypto.timingSafeEqual(sha(a), sha(b));

const app = express();
app.disable('x-powered-by');
app.set('trust proxy', 1);
app.use(cookieParser());

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

app.post('/auth/login', express.json({ limit: '4kb' }), (req, res) => {
  if (tooMany(req.ip)) return res.status(429).json({ error: 'terlalu banyak percobaan' });
  const { username = '', password = '' } = req.body ?? {};
  const ok = safeEqual(String(username), ADMIN_USER) & safeEqual(String(password), ADMIN_PASSWORD);
  if (!ok) {
    noteFail(req.ip);
    return res.status(401).json({ error: 'kredensial salah' });
  }
  fails.delete(req.ip);
  const token = jwt.sign({ sub: ADMIN_USER }, JWT_SECRET, { expiresIn: '12h' });
  res.cookie('nvr_session', token, {
    httpOnly: true,
    sameSite: 'strict',
    secure: COOKIE_SECURE === 'true',
    maxAge: 12 * 3600 * 1000,
  });
  res.json({ ok: true });
});

app.post('/auth/logout', (req, res) => {
  res.clearCookie('nvr_session');
  res.json({ ok: true });
});

function requireAuth(req, res, next) {
  try {
    req.user = jwt.verify(req.cookies.nvr_session ?? '', JWT_SECRET);
    next();
  } catch {
    res.status(401).json({ error: 'belum login' });
  }
}

app.get('/healthz', (req, res) => res.json({ status: 'ok' }));
app.get('/auth/me', requireAuth, (req, res) => res.json({ user: req.user.sub }));

// Hanya endpoint recorder yang diizinkan diteruskan (tanpa /healthz internal).
app.use(
  '/api',
  requireAuth,
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

const here = path.dirname(fileURLToPath(import.meta.url));
app.use('/vendor/hls', express.static(path.join(here, 'node_modules/hls.js/dist')));
app.use(express.static(path.join(here, 'public')));

app.listen(Number(PORT), () => console.log(`api listening on :${PORT}, recorder=${RECORDER_URL}`));
