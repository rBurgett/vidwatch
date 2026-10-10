// Wipe vidwatch back to a clean slate: drop the SQLite database and every
// uploaded video, thumbnail, and avatar. Honors DATA_DIR / UPLOADS_DIR from
// .env so it targets the same locations the server uses.
//
//   npm run reset
//
// The folders themselves are kept (and re-created on server boot anyway), so
// this is safe to run any time you want a fresh start.

require('dotenv').config();

const path = require('path');
const fs = require('fs');
const readline = require('readline');

const dataDir = path.resolve(process.env.DATA_DIR || path.join(__dirname, '..', 'data'));
const uploadsDir = path.resolve(process.env.UPLOADS_DIR || path.join(__dirname, '..', 'uploads'));
const skipPrompt = process.argv.includes('--yes') || process.argv.includes('-y');

function confirm() {
  return new Promise((resolve) => {
    if (skipPrompt || !process.stdin.isTTY) return resolve(true);
    const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
    rl.question('This deletes ALL accounts, videos, thumbnails, and avatars. Are you sure? (Y/N) ', (answer) => {
      rl.close();
      resolve(/^y(es)?$/i.test(answer.trim()));
    });
  });
}

function run() {
  let removed = 0;

  function rm(p) {
    if (fs.existsSync(p)) {
      fs.rmSync(p, { recursive: true, force: true });
      console.log('  removed', path.relative(process.cwd(), p));
      removed += 1;
    }
  }
  function emptyDir(dir) {
    if (!fs.existsSync(dir)) return;
    for (const name of fs.readdirSync(dir)) rm(path.join(dir, name));
  }

  console.log('Resetting vidwatch...');

  // SQLite database (+ its WAL/SHM sidecar files).
  for (const name of ['vidwatch.db', 'vidwatch.db-wal', 'vidwatch.db-shm', 'vidwatch.db-journal']) {
    rm(path.join(dataDir, name));
  }
  // Uploaded media.
  for (const sub of ['videos', 'thumbnails', 'avatars']) {
    emptyDir(path.join(uploadsDir, sub));
  }

  console.log(removed ? `Done — removed ${removed} item(s). Start the server to begin fresh.` : 'Nothing to remove — already clean.');
}

confirm().then((ok) => {
  if (!ok) { console.log('Cancelled — nothing was deleted.'); return; }
  run();
});
