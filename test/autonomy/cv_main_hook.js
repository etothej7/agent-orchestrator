// Test-only main-process recorder, loaded with NODE_OPTIONS=--require in the SCRATCH GUI only.
// Records (never displays) the dock bounce type and OS toast construction the real app code requests.
const fs = require('fs');
const log = process.env.CV_HOOK_LOG;
const rec = (kind, data) => { try { fs.appendFileSync(log, JSON.stringify({ kind, at: Date.now(), ...data }) + '\n'); } catch (_) {} };
try {
  const electron = require('electron');
  if (electron.Notification && electron.Notification.prototype) {
    const proto = electron.Notification.prototype;
    const origShow = proto.show;
    proto.show = function () { rec('notification-show', { title: this.title, silent: this.silent }); return undefined; };
    rec('hook', { patched: 'Notification.show', hadOriginal: typeof origShow === 'function' });
  }
  if (electron.app && electron.app.dock && typeof electron.app.dock.bounce === 'function') {
    electron.app.dock.bounce = function (type) { rec('dock-bounce', { type }); return 0; };
    rec('hook', { patched: 'app.dock.bounce' });
  }
} catch (e) { rec('hook-error', { message: String(e && e.message || e).slice(0, 120) }); }
