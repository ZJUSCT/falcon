const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const ts = require('typescript');
const React = require('react');
const { create, act } = require('react-test-renderer');

// Load the real components with the project's compiler and path aliases.
const modules = new Map();
function load(relative) {
  if (modules.has(relative)) return modules.get(relative);
  const base = path.join(__dirname, '..', relative);
  const filename = ['.tsx', '.ts', '/index.ts'].map(suffix => base + suffix).find(fs.existsSync);
  const exports = {};
  modules.set(relative, exports);
  const { outputText } = ts.transpileModule(fs.readFileSync(filename, 'utf8'), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020, jsx: ts.JsxEmit.ReactJSX },
  });
  new Function('require', 'exports', outputText)(name => name.startsWith('@/') ? load(name.slice(2)) : require(name), exports);
  return exports;
}
const { MirrorDetail } = load('components/mirror-detail');

test('detail refreshes retain one log panel and stream; changing mirrors and leaving cancel it', async t => {
  const intervals = new Map();
  const timeouts = new Map();
  let timerID = 0;
  t.mock.method(global, 'setInterval', (callback, delay) => { intervals.set(++timerID, { callback, delay }); return timerID; });
  t.mock.method(global, 'clearInterval', id => intervals.delete(id));
  t.mock.method(global, 'setTimeout', (callback, delay) => { timeouts.set(++timerID, { callback, delay }); return timerID; });
  t.mock.method(global, 'clearTimeout', id => timeouts.delete(id));
  const errors = [];
  t.mock.method(console, 'error', (...args) => errors.push(args.join(' ')));
  const activeStreams = new Set();
  let streamRequests = 0;
  t.mock.method(global, 'fetch', async (url, { signal } = {}) => {
    const pathname = new URL(url, 'http://falcon.test').pathname;
    if (pathname === '/api/jobs') return Response.json(['debian', 'ubuntu'].map(id => ({
      id, kind: 'Mirror', conditions: [], sync_phase: 'Syncing', sync_busy: true, phase: 'Syncing', paused: false,
    })));
    if (pathname === '/api/usage') return Response.json({ mirrors: [] });
    if (pathname.startsWith('/api/repos/')) return new Response('sync: {}');
    if (pathname.endsWith('/logs')) return Response.json({
      job: pathname, jobUID: pathname, current: true, jobs: [],
      pods: [{ name: 'sync-pod', uid: pathname, containers: [{ name: 'sync', state: 'running', init: false }] }],
    });
    assert.ok(pathname.endsWith('/logs/stream'), `unexpected request: ${url}`);
    streamRequests++;
    activeStreams.add(signal);
    return new Response(new ReadableStream({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(JSON.stringify({ data: btoa('sync output\n') }) + '\n'));
        signal.addEventListener('abort', () => { activeStreams.delete(signal); controller.close(); }, { once: true });
      },
    }));
  });

  let view;
  const render = mirrorId => React.createElement(MirrorDetail, { mirrorId, onBack() {} });
  const assertSinglePanel = () => {
    assert.equal(view.root.findAllByProps({ 'aria-label': 'Synchronization logs' }).length, 1);
    assert.equal(activeStreams.size, 1);
  };
  try {
    await act(async () => { view = create(render('debian')); });
    assertSinglePanel();
    for (let n = 0; n < 8; n++) {
      // Both the detail data and source metadata refresh every five seconds.
      await act(async () => {
        for (const { callback, delay } of [...intervals.values()]) if (delay === 5000) callback();
        for (const [id, { callback, delay }] of [...timeouts]) if (delay === 5000) { timeouts.delete(id); callback(); }
      });
      assertSinglePanel();
    }
    assert.equal(streamRequests, 1, 'unchanged log selection must retain its connection');
    await act(async () => { view.update(render('ubuntu')); });
    assertSinglePanel();
    assert.equal(streamRequests, 2, 'a different mirror opens one replacement stream');
    assert.deepEqual(errors, [], 'React must not report reconciliation warnings');
  } finally {
    await act(async () => { view?.unmount(); });
  }
  assert.equal(activeStreams.size, 0);
  assert.equal(intervals.size, 0);
  assert.equal(timeouts.size, 0);
});
