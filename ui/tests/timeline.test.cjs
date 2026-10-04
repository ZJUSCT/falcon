const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const ts = require('typescript');

// Run pure UI calculations with the project's compiler, without a DOM runner.
function load(relative) {
  const exports = {};
  const source = fs.readFileSync(path.join(__dirname, '..', relative), 'utf8');
  const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 } });
  new Function('require', 'exports', outputText)(name => name === '@/types' ? load('types/index.ts') : require(name), exports);
  return exports;
}
const { hour, timelineEvents, timelineBounds, packTimeline } = load('lib/timeline.ts');
const now = Date.parse('2026-10-03T23:00:00Z');
const iso = time => new Date(time).toISOString();
const job = overrides => ({ kind: 'Mirror', id: 'debian', namespace: 'mirrors', sync_phase: 'Waiting', paused: false, next_attempt_at: iso(now + hour), last_sync_duration_seconds: 7200, ...overrides });

test('planned duration uses previous run across midnight; paused and proxy modes have no schedule', () => {
  const events = timelineEvents([job({}), job({ id: 'paused', paused: true }), job({ id: 'proxy', kind: 'ProxyMirror' }), job({ id: 'cache', kind: 'CacheMirror' })], now);
  assert.equal(events.length, 1);
  assert.equal(events[0].start, now + hour);
  assert.equal(events[0].end, now + 3 * hour);
  assert.equal(events[0].durationKnown, true);
});

test('active work stays visible while schedule is paused and ends at now', () => {
  const [event] = timelineEvents([job({ paused: true, sync_phase: 'Syncing', last_attempt_at: iso(now - 36 * hour) })], now);
  assert.equal(event.kind, 'active');
  assert.equal(event.start, now - 36 * hour);
  assert.equal(event.end, now);
  const [bar] = packTimeline([event], now - 6 * hour, now + 18 * hour, 960);
  assert.equal(bar.clippedStart, true);
  assert.equal(bar.left, 0);
  assert.equal(bar.width, 240);
});

test('48-hour horizon excludes distant schedules and clips estimates without losing active work', () => {
  const events = timelineEvents([
    job({ id: 'weekly', next_attempt_at: iso(now + 168 * hour) }),
    job({ id: 'just-outside', next_attempt_at: iso(now + 48 * hour + 1) }),
    job({ id: 'near-edge', next_attempt_at: iso(now + 47 * hour), last_sync_duration_seconds: 72000 }),
    job({ id: 'active', sync_phase: 'Syncing', last_attempt_at: iso(now - 72 * hour) }),
  ], now);
  assert.deepEqual(events.map(event => event.mirror), ['near-edge', 'active']);
  const bounds = timelineBounds(events, now);
  assert.equal(bounds.start, now - 72 * hour);
  assert.equal(bounds.end, now + 48 * hour);
  const bar = packTimeline(events, bounds.start, bounds.end, 1200).find(bar => bar.mirror === 'near-edge');
  assert.equal(bar.clippedEnd, true);
  assert.equal(bar.end, now + 67 * hour);
  assert.equal(bar.left + bar.width, 1200);
});

test('unknown duration is a marked placeholder; queued jobs use queue time', () => {
  const [unknown] = timelineEvents([job({ last_sync_duration_seconds: undefined })], now);
  assert.equal(unknown.end - unknown.start, 15 * 60_000);
  assert.equal(unknown.durationKnown, false);
  const [queued] = timelineEvents([job({ sync_phase: 'Pending', phase: 'Initializing', sync_busy: true, last_attempt_at: iso(now - hour) })], now);
  assert.equal(queued.start, now - hour);
  assert.equal(queued.overdue, true);
  assert.equal(timelineEvents([job({ next_attempt_at: 'invalid' })], now).length, 0);
});

test('first-fit packing reuses gaps and reserves labels for short bars', () => {
  const make = (id, start, end) => ({ id, mirror: id, kind: 'planned', phase: 'Waiting', start, end, durationKnown: true, overdue: false });
  const bars = packTimeline([make('a', 0, 100), make('b', 50, 200), make('c', 150, 160), make('d', 250, 300)], 0, 1000, 1000);
  assert.deepEqual(bars.map(b => b.track), [0, 1, 0, 0]);
  assert.ok(bars[2].labelWidth > bars[2].width);
  for (const a of bars) for (const b of bars) {
    if (a.id !== b.id && a.track === b.track) assert.ok(a.left + a.labelWidth <= b.left || b.left + b.labelWidth <= a.left);
  }
});

test('dense same-time schedules have no collisions; clipping and empty windows are bounded', () => {
  const events = timelineEvents(Array.from({ length: 200 }, (_, n) => job({ id: `mirror-${n}` })), now);
  const bars = packTimeline(events, now, now + 2 * hour, 900);
  assert.equal(bars.length, 200);
  assert.equal(new Set(bars.map(b => b.track)).size, 200);
  assert.ok(bars.every(b => b.left >= 0 && b.left + b.labelWidth <= 900 && b.clippedEnd));
  assert.equal(packTimeline(events, now - 2 * hour, now - hour, 900).length, 0);
  assert.equal(packTimeline(events, now, now, 900).length, 0);
});
