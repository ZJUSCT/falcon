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
