# UI Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Redesign MirrorGo dashboard with collapsible sidebar navigation, dark theme, cluster-aware pages (Workers, Mirrors), preserving the existing clock visualization.

**Architecture:** Rewrite layout.tsx as sidebar shell, replace tab-based page.tsx with hash router + sidebar nav. Refactor existing components into new page structure. Add new Workers page and merge Repos+Jobs into Mirrors page.

**Tech Stack:** Next.js 14 (static export), React 18, Tailwind CSS, shadcn/ui, Lucide icons, TypeScript

**Spec:** `docs/superpowers/specs/2026-04-03-ui-redesign-design.md`

---

## File Map

### New files
| File | Responsibility |
|---|---|
| `ui/components/sidebar.tsx` | Collapsible sidebar navigation component |
| `ui/components/mirrors-view.tsx` | Unified mirrors list (repos + jobs merged) |
| `ui/components/mirror-detail.tsx` | Mirror detail: repo info + action history + logs |
| `ui/components/workers-view.tsx` | Worker list with status, labels, actions |
| `ui/components/configs-view.tsx` | Read-only repo config viewer (replaces repos-view) |

### Files to modify
| File | Changes |
|---|---|
| `ui/types/index.ts` | Add Worker type, update Action (worker_name, Reconciling), update SyncConfig (node, nodeSelector) |
| `ui/lib/api.ts` | Add getWorkers(), removeWorker() |
| `ui/lib/utils.ts` | Add getStatusColor cases for Reconciling + Online/Offline |
| `ui/app/globals.css` | Replace with dark theme as default (no light mode) |
| `ui/app/layout.tsx` | Remove header, make body a flex container for sidebar + main |
| `ui/app/page.tsx` | Rewrite: use sidebar for nav instead of tabs, update route types |
| `ui/components/overview-view.tsx` | Rewrite: stats cards + clock (preserved) + workers grid + running/failures |
| `ui/components/actions-view.tsx` | Add worker_name column |
| `ui/components/action-detail.tsx` | Show worker_name field |
| `ui/components/queue-view.tsx` | Adapt to standalone page (remove card wrapper duplication) |
| `ui/components/status-badge.tsx` | Add Reconciling + Online/Offline status colors |

### Files to delete
| File | Replaced by |
|---|---|
| `ui/components/repos-view.tsx` | `configs-view.tsx` |
| `ui/components/jobs-view.tsx` | `mirrors-view.tsx` |
| `ui/components/job-detail.tsx` | `mirror-detail.tsx` |
| `ui/components/queue-job-controls.tsx` | Inlined into queue-view.tsx |

---

## Task Breakdown

### Task 1: Update types and API client

**Files:**
- Modify: `ui/types/index.ts`
- Modify: `ui/lib/api.ts`

- [ ] **Step 1: Update types/index.ts**

Add Worker type, update SyncConfig with node affinity, update Action with worker_name and Reconciling status:

```typescript
// Add to SyncConfig (after environments field):
  node?: string;
  nodeSelector?: Record<string, string>;

// Update Action.status to include Reconciling:
  status: 'Running' | 'Succeeded' | 'Failed' | 'Reconciling';

// Add worker_name to Action (after message field):
  worker_name?: string;

// Add new Worker interface at the end:
export interface Worker {
  name: string;
  addr: string;
  labels: Record<string, string> | null;
  status: 'Online' | 'Offline';
  last_heartbeat: string;
  running_actions: string[] | null;
  registered_at: string;
}
```

- [ ] **Step 2: Update lib/api.ts**

Add Worker import and two new methods to ApiClient class:

```typescript
// Add Worker to import line:
import { Repo, Job, Action, QueueItem, QueueResponse, LogListResponse, Worker } from '@/types';

// Add to ApiClient class:
  async getWorkers(): Promise<Worker[]> {
    return this.fetch<Worker[]>('/workers');
  }

  async removeWorker(name: string): Promise<void> {
    const response = await fetch(`${API_BASE}/workers/remove?name=${encodeURIComponent(name)}`, {
      method: 'POST',
    });
    if (!response.ok) {
      const errorData = await response.json().catch(() => ({ error: response.statusText }));
      throw new Error(errorData.error || `API request failed: ${response.statusText}`);
    }
  }
```

- [ ] **Step 3: Update lib/utils.ts getStatusColor**

Add cases for new statuses:

```typescript
    case 'Reconciling':
      return 'bg-yellow-600 border-yellow-200 hover:bg-yellow-700';
    case 'Online':
      return 'bg-green-600 border-green-200 hover:bg-green-700';
    case 'Offline':
      return 'bg-red-600 border-red-200 hover:bg-red-700';
```

- [ ] **Step 4: Verify build**

Run: `cd /Users/star/mirrorgo/ui && npm run build`

- [ ] **Step 5: Commit**

```bash
git -c commit.gpgsign=false add ui/types/index.ts ui/lib/api.ts ui/lib/utils.ts
git -c commit.gpgsign=false commit -m "feat(ui): add Worker type, cluster fields, and API methods"
```

---

### Task 2: Dark theme CSS

**Files:**
- Modify: `ui/app/globals.css`

- [ ] **Step 1: Replace globals.css with dark-only theme**

Replace the entire content of `ui/app/globals.css`:

```css
@tailwind base;
@tailwind components;
@tailwind utilities;

:root {
  --background: 0 0% 4%;
  --foreground: 240 5% 90%;
  --card: 0 0% 8%;
  --card-foreground: 240 5% 90%;
  --popover: 0 0% 8%;
  --popover-foreground: 240 5% 90%;
  --primary: 217 91% 60%;
  --primary-foreground: 0 0% 100%;
  --secondary: 240 4% 16%;
  --secondary-foreground: 240 5% 90%;
  --muted: 240 4% 16%;
  --muted-foreground: 240 5% 50%;
  --accent: 240 4% 16%;
  --accent-foreground: 240 5% 90%;
  --destructive: 0 84% 60%;
  --destructive-foreground: 0 0% 100%;
  --border: 240 4% 12%;
  --input: 240 4% 12%;
  --ring: 217 91% 60%;
  --radius: 0.5rem;
}

* {
  border-color: hsl(var(--border));
}

body {
  color: hsl(var(--foreground));
  background: hsl(var(--background));
}
```

- [ ] **Step 2: Verify build**

Run: `cd /Users/star/mirrorgo/ui && npm run build`

- [ ] **Step 3: Commit**

```bash
git -c commit.gpgsign=false add ui/app/globals.css
git -c commit.gpgsign=false commit -m "feat(ui): switch to dark-only theme"
```

---

### Task 3: Create sidebar component

**Files:**
- Create: `ui/components/sidebar.tsx`

- [ ] **Step 1: Create sidebar.tsx**

```typescript
'use client';

import { useState } from 'react';
import { cn } from '@/lib/utils';
import {
  BarChart3, Disc3, Monitor, ListOrdered, Zap, Settings, PanelLeftClose, PanelLeft, Menu, X,
} from 'lucide-react';

export type PageId = 'overview' | 'mirrors' | 'workers' | 'queue' | 'actions' | 'configs';

interface SidebarProps {
  activePage: PageId;
  onNavigate: (page: PageId) => void;
}

const navItems: { id: PageId; label: string; icon: typeof BarChart3 }[] = [
  { id: 'overview', label: 'Overview', icon: BarChart3 },
  { id: 'mirrors', label: 'Mirrors', icon: Disc3 },
  { id: 'workers', label: 'Workers', icon: Monitor },
  { id: 'queue', label: 'Queue', icon: ListOrdered },
  { id: 'actions', label: 'Actions', icon: Zap },
  { id: 'configs', label: 'Configs', icon: Settings },
];

export function Sidebar({ activePage, onNavigate }: SidebarProps) {
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);

  const nav = (
    <nav className="flex-1 px-2 py-2 space-y-0.5">
      {navItems.map((item) => {
        const Icon = item.icon;
        const active = activePage === item.id;
        return (
          <button
            key={item.id}
            onClick={() => { onNavigate(item.id); setMobileOpen(false); }}
            className={cn(
              'w-full flex items-center gap-3 px-3 py-2 rounded-md text-sm font-medium transition-colors',
              active
                ? 'bg-primary/15 text-primary'
                : 'text-muted-foreground hover:text-foreground hover:bg-accent'
            )}
          >
            <Icon className="h-4 w-4 flex-shrink-0" />
            {!collapsed && <span>{item.label}</span>}
          </button>
        );
      })}
    </nav>
  );

  return (
    <>
      {/* Desktop sidebar */}
      <aside
        className={cn(
          'hidden sm:flex flex-col border-r bg-card flex-shrink-0 transition-all duration-200',
          collapsed ? 'w-14' : 'w-48'
        )}
      >
        {/* Brand */}
        <div className="flex items-center gap-2 px-3 py-4 border-b">
          <div className="w-7 h-7 rounded-md bg-primary flex items-center justify-center text-primary-foreground text-xs font-bold flex-shrink-0">
            MG
          </div>
          {!collapsed && <span className="font-bold text-sm">MirrorGo</span>}
        </div>
        {nav}
        {/* Collapse toggle */}
        <button
          onClick={() => setCollapsed(!collapsed)}
          className="flex items-center justify-center gap-2 px-3 py-3 border-t text-muted-foreground hover:text-foreground text-xs"
        >
          {collapsed ? <PanelLeft className="h-4 w-4" /> : <PanelLeftClose className="h-4 w-4" />}
          {!collapsed && <span>Collapse</span>}
        </button>
      </aside>

      {/* Mobile top bar */}
      <div className="sm:hidden fixed top-0 left-0 right-0 z-50 flex items-center justify-between px-4 py-3 border-b bg-card">
        <div className="flex items-center gap-2">
          <div className="w-7 h-7 rounded-md bg-primary flex items-center justify-center text-primary-foreground text-xs font-bold">
            MG
          </div>
          <span className="font-bold text-sm">MirrorGo</span>
        </div>
        <button onClick={() => setMobileOpen(!mobileOpen)} className="text-muted-foreground">
          {mobileOpen ? <X className="h-5 w-5" /> : <Menu className="h-5 w-5" />}
        </button>
      </div>

      {/* Mobile overlay */}
      {mobileOpen && (
        <div className="sm:hidden fixed inset-0 z-40 bg-background/80 backdrop-blur-sm" onClick={() => setMobileOpen(false)}>
          <div className="w-48 h-full bg-card border-r" onClick={(e) => e.stopPropagation()}>
            <div className="pt-16">
              {nav}
            </div>
          </div>
        </div>
      )}
    </>
  );
}
```

- [ ] **Step 2: Verify build**

Run: `cd /Users/star/mirrorgo/ui && npm run build`

- [ ] **Step 3: Commit**

```bash
git -c commit.gpgsign=false add ui/components/sidebar.tsx
git -c commit.gpgsign=false commit -m "feat(ui): add collapsible sidebar navigation component"
```

---

### Task 4: Rewrite layout.tsx and page.tsx

**Files:**
- Modify: `ui/app/layout.tsx`
- Modify: `ui/app/page.tsx`

- [ ] **Step 1: Rewrite layout.tsx**

Remove the old header. Make body a flex container. Add mobile top-bar spacing.

```typescript
import type { Metadata } from 'next';
import { Inter } from 'next/font/google';
import './globals.css';

const inter = Inter({ subsets: ['latin'] });

export const metadata: Metadata = {
  title: 'MirrorGo Dashboard',
  description: 'Cluster-aware mirror management dashboard',
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body className={inter.className}>
        <div className="min-h-screen bg-background flex">
          {children}
        </div>
      </body>
    </html>
  );
}
```

- [ ] **Step 2: Rewrite page.tsx**

Replace the entire content. New structure: Sidebar + main content area with hash-based routing. Import only existing components that haven't been renamed yet (overview-view, actions-view, queue-view, action-detail). For mirrors-view, workers-view, configs-view, mirror-detail — use placeholder divs for now (they'll be created in later tasks).

```typescript
'use client';

import { useState, useEffect } from 'react';
import { Sidebar, PageId } from '@/components/sidebar';
import { OverviewView } from '@/components/overview-view';
import { ActionsView } from '@/components/actions-view';
import { QueueView } from '@/components/queue-view';
import { ActionDetail } from '@/components/action-detail';

type RouteType =
  | PageId
  | { type: 'mirror-detail'; mirrorId: string }
  | { type: 'action-detail'; actionId: string };

export default function Dashboard() {
  const [currentRoute, setCurrentRoute] = useState<RouteType>('overview');

  const getRouteFromHash = (): RouteType => {
    if (typeof window === 'undefined') return 'overview';
    const hash = window.location.hash.replace('#', '');
    if (hash.startsWith('mirrors/')) {
      const id = hash.substring(8);
      if (id) return { type: 'mirror-detail', mirrorId: id };
    }
    if (hash.startsWith('actions/')) {
      const id = hash.substring(8);
      if (id) return { type: 'action-detail', actionId: id };
    }
    const pages: PageId[] = ['overview', 'mirrors', 'workers', 'queue', 'actions', 'configs'];
    return pages.includes(hash as PageId) ? (hash as PageId) : 'overview';
  };

  const navigate = (route: RouteType) => {
    setCurrentRoute(route);
    if (typeof window !== 'undefined') {
      if (typeof route === 'string') {
        window.location.hash = route;
      } else if (route.type === 'mirror-detail') {
        window.location.hash = `mirrors/${route.mirrorId}`;
      } else if (route.type === 'action-detail') {
        window.location.hash = `actions/${route.actionId}`;
      }
    }
  };

  useEffect(() => {
    setCurrentRoute(getRouteFromHash());
    const onHash = () => setCurrentRoute(getRouteFromHash());
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  const getActivePage = (): PageId => {
    if (typeof currentRoute === 'object') {
      if (currentRoute.type === 'mirror-detail') return 'mirrors';
      if (currentRoute.type === 'action-detail') return 'actions';
    }
    return currentRoute as PageId;
  };

  const renderView = () => {
    if (typeof currentRoute === 'object') {
      if (currentRoute.type === 'mirror-detail') {
        return <div className="p-6 text-muted-foreground">Mirror detail: {currentRoute.mirrorId} (placeholder)</div>;
      }
      if (currentRoute.type === 'action-detail') {
        return <ActionDetail actionId={currentRoute.actionId} onBack={() => navigate('actions')} onJobClick={(id) => navigate({ type: 'mirror-detail', mirrorId: id })} />;
      }
    }
    switch (currentRoute) {
      case 'overview':
        return <OverviewView onNavigateToJob={(id) => navigate({ type: 'mirror-detail', mirrorId: id })} />;
      case 'mirrors':
        return <div className="p-6 text-muted-foreground">Mirrors view (placeholder)</div>;
      case 'workers':
        return <div className="p-6 text-muted-foreground">Workers view (placeholder)</div>;
      case 'queue':
        return <QueueView />;
      case 'actions':
        return <ActionsView onActionClick={(id) => navigate({ type: 'action-detail', actionId: id })} />;
      case 'configs':
        return <div className="p-6 text-muted-foreground">Configs view (placeholder)</div>;
      default:
        return <OverviewView onNavigateToJob={(id) => navigate({ type: 'mirror-detail', mirrorId: id })} />;
    }
  };

  return (
    <>
      <Sidebar activePage={getActivePage()} onNavigate={navigate} />
      <main className="flex-1 overflow-auto sm:pt-0 pt-14">
        {renderView()}
      </main>
    </>
  );
}
```

- [ ] **Step 3: Verify build**

Run: `cd /Users/star/mirrorgo/ui && npm run build`

- [ ] **Step 4: Commit**

```bash
git -c commit.gpgsign=false add ui/app/layout.tsx ui/app/page.tsx
git -c commit.gpgsign=false commit -m "feat(ui): rewrite layout with sidebar shell and hash router"
```

---

### Task 5: Rewrite overview-view.tsx

**Files:**
- Modify: `ui/components/overview-view.tsx`

- [ ] **Step 1: Rewrite overview-view.tsx**

This is the largest component (1058 lines currently). Rewrite to new layout: LIVE indicator → stats cards → clock (preserved SVG logic) → workers grid → running/failures lists.

Read the current `ui/components/overview-view.tsx` fully. The SVG clock logic (all the `TimeEvent`, `ClockDimensions`, SVG rendering, zoom/pan handlers) must be preserved exactly. Wrap it in the new layout structure.

The new structure top-to-bottom:
1. Page header with "Overview" title + LIVE indicator
2. Stats cards row (5 cards): Workers, Running, Queue, Succeeded 24h, Failed 24h — fetch from `/api/workers`, `/api/jobs`, `/api/queue`, `/api/actions/recent`
3. Schedule Timeline card containing the existing SVG clock (preserve all zoom/pan/hover logic)
4. Workers grid — fetch from `/api/workers`, display as cards with status dot, labels, running count
5. Two-column: Currently Running (from active actions) + Recent Failures (from recent actions filtered)

Key data fetching: add `workers` state (`Worker[]`) fetched from `apiClient.getWorkers()` alongside existing jobs fetch. Add `recentActions` state fetched from `apiClient.getRecentActions(100)`.

Keep the existing 5-second polling interval for all data.

The SVG clock section: extract from the existing component as-is, just wrap it in a card with "Schedule Timeline" header and a legend (blue=running, green=success, red=failed, amber=next sync).

- [ ] **Step 2: Verify build**

Run: `cd /Users/star/mirrorgo/ui && npm run build`

- [ ] **Step 3: Commit**

```bash
git -c commit.gpgsign=false add ui/components/overview-view.tsx
git -c commit.gpgsign=false commit -m "feat(ui): rewrite overview with stats, clock hero, workers grid"
```

---

### Task 6: Create mirrors-view.tsx and mirror-detail.tsx

**Files:**
- Create: `ui/components/mirrors-view.tsx`
- Create: `ui/components/mirror-detail.tsx`

- [ ] **Step 1: Create mirrors-view.tsx**

Merges repos + jobs into one table. Read current `ui/components/jobs-view.tsx` for reference on how jobs are displayed, and `ui/components/repos-view.tsx` for repo data.

Fetches: `apiClient.getRepos()` + `apiClient.getJobs()`, joined by repo ID.

Table columns: Name (clickable), Status badge, Last Action Status badge, Worker (from last action's worker_name or repo's sync.node), Last Sync (relative time), Next Sync (relative time), Trigger button.

Search bar for filtering by name. Status filter dropdown.

Props: `onMirrorClick: (id: string) => void`

- [ ] **Step 2: Create mirror-detail.tsx**

Read current `ui/components/job-detail.tsx` for reference. Adapt to use the new "mirror" naming.

Shows: repo info (upstream, image, interval, node/nodeSelector affinity), action history table (from `apiClient.getActionsByRepo`), click action to navigate to action-detail.

Props: `mirrorId: string`, `onBack: () => void`, `onActionClick: (id: string) => void`

- [ ] **Step 3: Wire into page.tsx**

Update `ui/app/page.tsx`:
- Import `MirrorsView` and `MirrorDetail`
- Replace placeholder divs in renderView for 'mirrors' and 'mirror-detail'

```typescript
// Add imports:
import { MirrorsView } from '@/components/mirrors-view';
import { MirrorDetail } from '@/components/mirror-detail';

// In renderView:
case 'mirrors':
  return <MirrorsView onMirrorClick={(id) => navigate({ type: 'mirror-detail', mirrorId: id })} />;

// And for mirror-detail route:
if (currentRoute.type === 'mirror-detail') {
  return <MirrorDetail mirrorId={currentRoute.mirrorId} onBack={() => navigate('mirrors')} onActionClick={(id) => navigate({ type: 'action-detail', actionId: id })} />;
}
```

- [ ] **Step 4: Verify build**

Run: `cd /Users/star/mirrorgo/ui && npm run build`

- [ ] **Step 5: Commit**

```bash
git -c commit.gpgsign=false add ui/components/mirrors-view.tsx ui/components/mirror-detail.tsx ui/app/page.tsx
git -c commit.gpgsign=false commit -m "feat(ui): add Mirrors page merging repos and jobs views"
```

---

### Task 7: Create workers-view.tsx

**Files:**
- Create: `ui/components/workers-view.tsx`

- [ ] **Step 1: Create workers-view.tsx**

Fetches: `apiClient.getWorkers()`, polls every 10s.

Layout: page header "Workers" + grid of worker cards (3 columns desktop, 1 mobile).

Each card shows:
- Status dot (green Online / red Offline) + worker name
- Address (muted text)
- Labels as colored badge pills
- Running actions count
- Last heartbeat (relative time)
- Registered at (relative time)
- Remove button (only for Offline workers, calls `apiClient.removeWorker(name)`)

- [ ] **Step 2: Wire into page.tsx**

```typescript
import { WorkersView } from '@/components/workers-view';
// In renderView:
case 'workers':
  return <WorkersView />;
```

- [ ] **Step 3: Verify build**

Run: `cd /Users/star/mirrorgo/ui && npm run build`

- [ ] **Step 4: Commit**

```bash
git -c commit.gpgsign=false add ui/components/workers-view.tsx ui/app/page.tsx
git -c commit.gpgsign=false commit -m "feat(ui): add Workers page with status, labels, and remove"
```

---

### Task 8: Create configs-view.tsx and update actions-view

**Files:**
- Create: `ui/components/configs-view.tsx`
- Modify: `ui/components/actions-view.tsx`
- Modify: `ui/components/action-detail.tsx`

- [ ] **Step 1: Create configs-view.tsx**

Read current `ui/components/repos-view.tsx`. Adapt as configs-view with same read-only display but add node affinity fields (`node`, `nodeSelector`) display.

- [ ] **Step 2: Update actions-view.tsx**

Add a "Worker" column to the actions table showing `action.worker_name`. Read current file and add the column after the "Status" column.

- [ ] **Step 3: Update action-detail.tsx**

Add worker_name display in the action detail info section. Read current file and add a row showing "Worker: {action.worker_name}" in the details grid.

- [ ] **Step 4: Wire configs into page.tsx**

```typescript
import { ConfigsView } from '@/components/configs-view';
// In renderView:
case 'configs':
  return <ConfigsView />;
```

- [ ] **Step 5: Verify build**

Run: `cd /Users/star/mirrorgo/ui && npm run build`

- [ ] **Step 6: Commit**

```bash
git -c commit.gpgsign=false add ui/components/configs-view.tsx ui/components/actions-view.tsx ui/components/action-detail.tsx ui/app/page.tsx
git -c commit.gpgsign=false commit -m "feat(ui): add Configs page, worker column in actions"
```

---

### Task 9: Delete old files and update status-badge

**Files:**
- Delete: `ui/components/repos-view.tsx`
- Delete: `ui/components/jobs-view.tsx`
- Delete: `ui/components/job-detail.tsx`
- Delete: `ui/components/queue-job-controls.tsx`
- Modify: `ui/components/status-badge.tsx`

- [ ] **Step 1: Delete old files**

```bash
rm ui/components/repos-view.tsx ui/components/jobs-view.tsx ui/components/job-detail.tsx ui/components/queue-job-controls.tsx
```

- [ ] **Step 2: Check for remaining imports of deleted files**

Search for imports of the deleted components in all .tsx files. Remove any stale imports. The only files that should reference them were page.tsx (already updated) and potentially queue-view.tsx (if it imports queue-job-controls).

Read `ui/components/queue-view.tsx` — if it imports `queue-job-controls`, inline that functionality or remove the import.

- [ ] **Step 3: Verify build**

Run: `cd /Users/star/mirrorgo/ui && npm run build`

- [ ] **Step 4: Commit**

```bash
git -c commit.gpgsign=false add -A
git -c commit.gpgsign=false commit -m "refactor(ui): remove old components replaced by new pages"
```

---

### Task 10: Build and embed into Go binary

**Files:**
- None (build step)

- [ ] **Step 1: Build UI**

Run: `cd /Users/star/mirrorgo/ui && npm run build`
Expected: `ui/dist/` directory populated with static files

- [ ] **Step 2: Build Go binary**

Run: `cd /Users/star/mirrorgo && go build .`
Expected: `mirrorgo` binary with embedded UI

- [ ] **Step 3: Smoke test**

Run master with dryrun worker:
```bash
./mirrorgo master --addr :18080 --auth-token test --configs Configs --db /tmp/ui-test.db --basedir /tmp/ui-test &
sleep 2
./mirrorgo worker --name test --master http://localhost:18080 --auth-token test --addr localhost:19090 --basedir /tmp/ui-test --dryrun &
sleep 5
# Open http://localhost:18080 in browser — verify sidebar, dark theme, overview page loads
# Check Workers page shows "test" worker online
# Check Mirrors page shows repos with job status
pkill -f mirrorgo
```

- [ ] **Step 4: Commit**

```bash
git -c commit.gpgsign=false add -A
git -c commit.gpgsign=false commit -m "build: rebuild UI with new dashboard layout"
```
