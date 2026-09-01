'use client';

// Hash-based routing, exactly like the legacy dashboard: works from any
// static file server with no rewrite rules.

import { useState, useEffect } from 'react';
import { Sidebar, PageId } from '@/components/sidebar';
import { OverviewView } from '@/components/overview-view';
import { MirrorsView } from '@/components/mirrors-view';
import { MirrorDetail } from '@/components/mirror-detail';

type RouteType =
  | PageId
  | { type: 'mirror-detail'; mirrorId: string };

export default function Dashboard() {
  const [authenticated, setAuthenticated] = useState<boolean | null>(null);
  const [currentRoute, setCurrentRoute] = useState<RouteType>('overview');

  const getRouteFromHash = (): RouteType => {
    if (typeof window === 'undefined') return 'overview';
    // Tolerate both "#mirrors" and "#/mirrors" so pasted deep links resolve.
    const hash = window.location.hash.replace(/^#\/?/, '');
    if (hash.startsWith('mirrors/')) {
      const id = hash.substring(8);
      if (id) return { type: 'mirror-detail', mirrorId: decodeURIComponent(id) };
    }
    const pages: PageId[] = ['overview', 'mirrors'];
    return pages.includes(hash as PageId) ? (hash as PageId) : 'overview';
  };

  const navigate = (route: RouteType) => {
    setCurrentRoute(route);
    if (typeof window !== 'undefined') {
      if (typeof route === 'string') {
        window.location.hash = route;
      } else if (route.type === 'mirror-detail') {
        window.location.hash = `mirrors/${encodeURIComponent(route.mirrorId)}`;
      }
    }
  };

  useEffect(() => {
    fetch('/oauth/session').then((r) => { if (r.ok) setAuthenticated(true); else { window.location.href = '/oauth/login'; } }).catch(() => { window.location.href = '/oauth/login'; });
  }, []);

  useEffect(() => {
    setCurrentRoute(getRouteFromHash());
    const onHash = () => setCurrentRoute(getRouteFromHash());
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  if (authenticated !== true) return <div className="p-8">Authenticating…</div>;

  const getActivePage = (): PageId => {
    if (typeof currentRoute === 'object') {
      return 'mirrors';
    }
    return currentRoute;
  };

  const renderView = () => {
    if (typeof currentRoute === 'object') {
      return <MirrorDetail mirrorId={currentRoute.mirrorId} onBack={() => navigate('mirrors')} />;
    }
    switch (currentRoute) {
      case 'overview':
        return <OverviewView onNavigateToJob={(id) => navigate({ type: 'mirror-detail', mirrorId: id })} />;
      case 'mirrors':
        return <MirrorsView onMirrorClick={(id) => navigate({ type: 'mirror-detail', mirrorId: id })} />;
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
