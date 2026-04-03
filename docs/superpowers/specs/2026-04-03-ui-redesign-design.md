# MirrorGo UI Redesign

## Overview

Redesign the MirrorGo dashboard to support the cluster architecture (Workers, node affinity) and improve navigation with a collapsible sidebar layout. Dark theme inspired by ServerAgent.

## Tech Stack

Same as current: Next.js (static export), Tailwind CSS, shadcn/ui components, Lucide icons. No new dependencies.

## Layout

### Sidebar (Collapsible)

- Left side, always visible
- **Expanded**: 200px wide, shows icon + text label for each nav item
- **Collapsed**: 56px wide, shows icon only
- Toggle button at bottom of sidebar
- Active item highlighted with blue background tint
- Logo/brand at top: "MG" icon + "MirrorGo" text (text hidden when collapsed)
- Mobile: sidebar hidden, hamburger menu in top bar opens overlay

### Navigation Items (top to bottom)

1. Overview (📊)
2. Mirrors (🪞)
3. Workers (🖥️)
4. Queue (📋)
5. Actions (⚡)
6. Configs (⚙️)

### Main Content Area

- Right of sidebar, full remaining width
- Each page has a top bar with page title + subtitle
- Content scrolls independently of sidebar

## Pages

### 1. Overview

Top-to-bottom layout:

**LIVE indicator** — green dot + "LIVE" text, top-left

**Stats cards row** — 5 cards in a horizontal grid:
- Workers Online: `N / total` (green)
- Running: count (blue) with delta indicator
- Queue Depth: count (amber)
- Succeeded (24h): count (green)
- Failed (24h): count (red)

Data sources: `GET /api/workers` (count online/total), `GET /api/jobs` (count by status), `GET /api/actions/recent?limit=1000` (count succeeded/failed in 24h), `GET /api/queue` (queue length)

**Schedule Timeline (Clock)** — the existing SVG radial clock visualization, preserved as-is from the current `overview-view.tsx`. Features retained: zoom, pan, hover tooltips, color-coded events (blue=running, green=success, red=failed, amber=next sync). Moved to hero position below stats.

**Workers grid** — card per worker, 3 columns:
- Status dot (green=online, red=offline) + worker name
- Label badges (colored pills)
- Running count + completed count
- Heartbeat latency (if available)

Data source: `GET /api/workers`

**Currently Running + Recent Failures** — two columns side by side:
- Running: list of `job_name → worker_name` with duration
- Failures: list of `job_name` with exit code and time ago

Data sources: `GET /api/actions` (active/running), `GET /api/actions/recent?limit=20` (filter failed)

**Auto-refresh**: all data on Overview polls every 5 seconds (same as current).

### 2. Mirrors

Replaces the separate "Repos" and "Jobs" tabs. One unified view.

**Table columns:**
- Name (repo ID, clickable)
- Status badge (Waiting / Scheduled / Running / Orphan)
- Last Action Status (Succeeded / Failed / Running / Reconciling)
- Worker (which worker last ran it, or assigned via `node`)
- Last Sync (relative time)
- Next Sync (relative time)
- Trigger button (same as current `trigger-button.tsx`)

Data sources: `GET /api/repos` + `GET /api/jobs` joined by repo ID

**Search/filter**: text search on repo name, filter by status dropdown

**Click row → Mirror Detail** (replaces current job-detail):
- Repo info (upstream, image, interval, node affinity)
- Action history table (from `GET /api/actions/by_repo`)
- Click action → Action Detail with log viewer (preserve current `log-viewer.tsx`)

### 3. Workers (New)

**Table/card view** of all workers:
- Name
- Status (Online badge green / Offline badge red)
- Address
- Labels (colored badges)
- Running Actions (count, expandable to list)
- Last Heartbeat (relative time)
- Registered At

Data source: `GET /api/workers`

**Actions:**
- Remove button (only enabled for Offline workers, calls `POST /api/workers/remove?name=`)

**Auto-refresh**: every 10 seconds

### 4. Queue

Same functionality as current `queue-view.tsx` + `queue-controls.tsx`:
- Pause/Resume button
- Max concurrency control
- Draggable/reorderable queue list
- Move to head/tail, delete from queue

Adapted to new layout (no tab wrapper, standalone page).

### 5. Actions

Same as current `actions-view.tsx`:
- Recent actions table (from `GET /api/actions/recent`)
- Click → Action Detail with log viewer

**New column**: Worker Name (shows which worker ran the action)

### 6. Configs

Same as current `repos-view.tsx` but renamed:
- Read-only config viewer
- Shows image, volumes, command, interval, node affinity (new: `node`, `nodeSelector` fields)

## Type Updates

Update `types/index.ts`:

```typescript
// Add to SyncConfig
interface SyncConfig {
  // ...existing fields
  node?: string;
  nodeSelector?: Record<string, string>;
}

// Add to Action
interface Action {
  // ...existing fields
  worker_name?: string;
  // status adds 'Reconciling'
  status: 'Running' | 'Succeeded' | 'Failed' | 'Reconciling';
}

// New type
interface Worker {
  name: string;
  addr: string;
  labels: Record<string, string> | null;
  status: 'Online' | 'Offline';
  last_heartbeat: string;
  running_actions: string[] | null;
  registered_at: string;
}
```

## API Client Updates

Add to `lib/api.ts`:

```typescript
async getWorkers(): Promise<Worker[]>
async removeWorker(name: string): Promise<void>
```

## Component Structure

```
ui/
├── app/
│   ├── layout.tsx          — sidebar + main content shell
│   ├── page.tsx            — route handling (simplified)
│   └── globals.css         — dark theme tokens
├── components/
│   ├── sidebar.tsx         — collapsible sidebar navigation (NEW)
│   ├── overview-view.tsx   — rewritten: stats + clock + workers grid + lists
│   ├── mirrors-view.tsx    — replaces repos-view + jobs-view (NEW)
│   ├── mirror-detail.tsx   — replaces job-detail (RENAME + adapt)
│   ├── workers-view.tsx    — worker list/grid (NEW)
│   ├── queue-view.tsx      — adapted from current
│   ├── actions-view.tsx    — adapted (add worker_name column)
│   ├── action-detail.tsx   — adapted from current
│   ├── configs-view.tsx    — adapted from repos-view (RENAME)
│   ├── log-viewer.tsx      — unchanged
│   ├── status-badge.tsx    — add Reconciling + Worker status badges
│   ├── relative-time.tsx   — unchanged
│   ├── trigger-button.tsx  — unchanged
│   ├── queue-controls.tsx  — unchanged
│   └── ui/                 — shadcn primitives (unchanged)
├── lib/
│   ├── api.ts              — add getWorkers, removeWorker
│   ├── hooks.ts            — unchanged
│   └── utils.ts            — unchanged
└── types/
    └── index.ts            — add Worker type, update Action/SyncConfig
```

## Files to Delete

- `components/repos-view.tsx` — replaced by `configs-view.tsx`
- `components/jobs-view.tsx` — merged into `mirrors-view.tsx`
- `components/job-detail.tsx` — replaced by `mirror-detail.tsx`
- `components/queue-job-controls.tsx` — inline into queue-view if still needed

## Dark Theme

Use Tailwind CSS dark mode as default (no light mode toggle). Color palette:
- Background: `#0a0a0a` (zinc-950)
- Card background: `#141414`
- Border: `#1e1e1e` (zinc-800)
- Text primary: `#e4e4e7` (zinc-200)
- Text secondary: `#71717a` (zinc-500)
- Text muted: `#52525b` (zinc-600)
- Blue (running/active): `#3b82f6`
- Green (success/online): `#22c55e`
- Red (failed/offline): `#ef4444`
- Amber (queue/warning): `#f59e0b`
- Purple (labels): `#a855f7`

## Mobile

- Sidebar hidden, replaced by top bar with hamburger menu
- Stats cards: 2 columns on mobile, scroll horizontally on very small screens
- Tables: horizontal scroll with sticky first column
- Clock: full width, zoom/pan preserved
