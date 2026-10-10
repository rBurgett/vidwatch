const jwt = require('jsonwebtoken');
const db = require('./db');

const JWT_SECRET = process.env.JWT_SECRET
  || (process.env.NODE_ENV === 'production'
    ? (() => { throw new Error('JWT_SECRET env var is required in production'); })()
    : 'dev-only-insecure-secret-change-me');
const TOKEN_TTL = '30d';

function signToken(user) {
  return jwt.sign({ sub: user.id, username: user.username }, JWT_SECRET, { expiresIn: TOKEN_TTL });
}

function readToken(req) {
  const header = req.headers.authorization || '';
  if (header.startsWith('Bearer ')) return header.slice(7);
  return null;
}

function optionalAuth(req, _res, next) {
  const token = readToken(req);
  if (!token) return next();
  try {
    const payload = jwt.verify(token, JWT_SECRET);
    const user = db.prepare('SELECT id, email, username, bio, avatar_filename, banner_filename, playback_speed FROM users WHERE id = ?').get(payload.sub);
    if (user) req.user = user;
  } catch (_) { /* ignore — anonymous */ }
  next();
}

function requireAuth(req, res, next) {
  if (!req.user) return res.status(401).json({ error: 'Authentication required' });
  next();
}

module.exports = { signToken, optionalAuth, requireAuth, JWT_SECRET };
