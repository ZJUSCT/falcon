'use client';

import { useState, useEffect } from 'react';
import { cn } from '@/lib/utils';
import {
  BarChart3, Disc3, ExternalLink, PanelLeftClose, PanelLeft, Menu, X,
  Sun, Moon, MonitorSmartphone,
} from 'lucide-react';

// The admin UI is read-only: two pages instead of the legacy seven.
// Workers, Storage, Queue, Actions and Configs views were removed along with
// their backend endpoints (the Kubernetes controller serves a strictly
// read-only API).
export type PageId = 'overview' | 'mirrors';

interface SidebarProps {
  activePage: PageId;
  onNavigate: (page: PageId) => void;
}

const navItems: { id: PageId; label: string; icon: typeof BarChart3 }[] = [
  { id: 'overview', label: 'Overview', icon: BarChart3 },
  { id: 'mirrors', label: 'Mirrors', icon: Disc3 },
];

export function Sidebar({ activePage, onNavigate }: SidebarProps) {
  const [collapsed, setCollapsed] = useState(false);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [theme, setThemeState] = useState<'dark' | 'light' | 'system'>('dark');

  useEffect(() => {
    const saved = localStorage.getItem('falcon-theme') || 'dark';
    setThemeState(saved as any);
    applyTheme(saved as any);
  }, []);

  useEffect(() => {
    if (theme !== 'system') return;
    const mql = window.matchMedia('(prefers-color-scheme: light)');
    const handler = () => applyTheme('system');
    mql.addEventListener('change', handler);
    return () => mql.removeEventListener('change', handler);
  }, [theme]);

  function applyTheme(t: 'dark' | 'light' | 'system') {
    const root = document.documentElement;
    root.classList.remove('light');
    if (t === 'light') {
      root.classList.add('light');
    } else if (t === 'system') {
      if (window.matchMedia('(prefers-color-scheme: light)').matches) {
        root.classList.add('light');
      }
    }
  }

  function cycleTheme() {
    const next = theme === 'dark' ? 'light' : theme === 'light' ? 'system' : 'dark';
    setThemeState(next);
    localStorage.setItem('falcon-theme', next);
    applyTheme(next);
  }

  const nav = (
    <nav className="flex-1 px-2 py-3 space-y-1">
      {navItems.map((item) => {
        const Icon = item.icon;
        const active = activePage === item.id;
        return (
          <button
            key={item.id}
            onClick={() => { onNavigate(item.id); setMobileOpen(false); }}
            style={{ padding: '8px 10px', gap: '10px' }}
            className={cn(
              'w-full flex items-center rounded-md text-xs font-medium transition-colors',
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
      {!collapsed && (
        <a
          href="https://mirrors.zjusct.io/mirrorz.json"
          target="_blank"
          rel="noreferrer"
          style={{ padding: '8px 10px', gap: '10px' }}
          className="w-full flex items-center rounded-md text-xs font-medium text-muted-foreground hover:text-foreground hover:bg-accent transition-colors"
        >
          <ExternalLink className="h-4 w-4 flex-shrink-0" />
          <span>mirrorz.json</span>
        </a>
      )}
    </nav>
  );

  return (
    <>
      {/* Desktop sidebar */}
      <aside
        className="hidden sm:flex flex-col border-r bg-card flex-shrink-0 transition-all duration-200 h-full overflow-y-auto"
        style={{ width: collapsed ? 52 : 180 }}
      >
        <div className="flex items-center gap-2 px-3 py-4 border-b">
          <img src="/falcon.svg" alt="Falcon logo" className="w-7 h-7 rounded-md flex-shrink-0" />
          {!collapsed && <span className="font-bold text-sm">Falcon</span>}
        </div>
        {nav}
        <div className="px-2 pb-1">
          <button
            onClick={cycleTheme}
            style={{ padding: '8px 10px', gap: '10px' }}
            className="w-full flex items-center rounded-md text-xs font-medium text-muted-foreground hover:text-foreground hover:bg-accent"
          >
            {theme === 'dark' ? <Moon className="h-4 w-4 flex-shrink-0" /> :
             theme === 'light' ? <Sun className="h-4 w-4 flex-shrink-0" /> :
             <MonitorSmartphone className="h-4 w-4 flex-shrink-0" />}
            {!collapsed && <span>{theme === 'dark' ? 'Dark' : theme === 'light' ? 'Light' : 'System'}</span>}
          </button>
        </div>
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
          <img src="/falcon.svg" alt="Falcon logo" className="w-7 h-7 rounded-md flex-shrink-0" />
          <span className="font-bold text-sm">Falcon</span>
        </div>
        <button onClick={() => setMobileOpen(!mobileOpen)} className="text-muted-foreground">
          {mobileOpen ? <X className="h-5 w-5" /> : <Menu className="h-5 w-5" />}
        </button>
      </div>

      {/* Mobile overlay */}
      {mobileOpen && (
        <div className="sm:hidden fixed inset-0 z-40 bg-background/80 backdrop-blur-sm" onClick={() => setMobileOpen(false)}>
          <div className="h-full bg-card border-r" style={{ width: 180 }} onClick={(e) => e.stopPropagation()}>
            <div className="pt-16">{nav}</div>
          </div>
        </div>
      )}
    </>
  );
}
