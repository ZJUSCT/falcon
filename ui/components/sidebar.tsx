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
    </nav>
  );

  return (
    <>
      {/* Desktop sidebar */}
      <aside
        className="hidden sm:flex flex-col border-r bg-card flex-shrink-0 transition-all duration-200"
        style={{ width: collapsed ? 52 : 180 }}
      >
        <div className="flex items-center gap-2 px-3 py-4 border-b">
          <div className="w-7 h-7 rounded-md bg-primary flex items-center justify-center text-primary-foreground text-xs font-bold flex-shrink-0">
            MG
          </div>
          {!collapsed && <span className="font-bold text-sm">MirrorGo</span>}
        </div>
        {nav}
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
          <div className="h-full bg-card border-r" style={{ width: 180 }} onClick={(e) => e.stopPropagation()}>
            <div className="pt-16">{nav}</div>
          </div>
        </div>
      )}
    </>
  );
}
