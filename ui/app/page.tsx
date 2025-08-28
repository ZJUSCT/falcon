'use client';

import { useState, useEffect } from 'react';
import { ReposView } from '@/components/repos-view';
import { JobsView } from '@/components/jobs-view';
import { ActionsView } from '@/components/actions-view';
import { QueueView } from '@/components/queue-view';
import { OverviewView } from '@/components/overview-view';
import { JobDetail } from '@/components/job-detail';
import { ActionDetail } from '@/components/action-detail';
import { Card, CardContent } from '@/components/ui/card';
import { cn } from '@/lib/utils';
import { Database, Briefcase, Play, Clock, Menu, X, Eye } from 'lucide-react';

type ViewType = 'overview' | 'repos' | 'jobs' | 'actions' | 'queue';
type RouteType = ViewType | { type: 'job-detail'; jobId: string } | { type: 'action-detail'; actionId: string };

const tabs = [
  { id: 'overview', label: 'Overview', icon: Eye },
  { id: 'repos', label: 'Repositories', icon: Database },
  { id: 'jobs', label: 'Jobs', icon: Briefcase },
  { id: 'actions', label: 'Actions', icon: Play },
  { id: 'queue', label: 'Queue', icon: Clock },
] as const;

export default function Dashboard() {
  const [currentRoute, setCurrentRoute] = useState<RouteType>('overview');
  const [isMobileMenuOpen, setIsMobileMenuOpen] = useState(false);

  // Function to parse route from URL hash
  const getRouteFromHash = (): RouteType => {
    if (typeof window === 'undefined') return 'overview';
    const hash = window.location.hash.replace('#', '');
    
    // Handle job detail routes: #jobs/job-id
    if (hash.startsWith('jobs/')) {
      const jobId = hash.substring(5); // Remove 'jobs/' prefix
      if (jobId) {
        return { type: 'job-detail', jobId };
      }
    }
    
    // Handle action detail routes: #actions/action-id
    if (hash.startsWith('actions/')) {
      const actionId = hash.substring(8); // Remove 'actions/' prefix
      if (actionId) {
        return { type: 'action-detail', actionId };
      }
    }
    
    // Handle simple view routes
    const validViews: ViewType[] = ['overview', 'repos', 'jobs', 'actions', 'queue'];
    return validViews.includes(hash as ViewType) ? (hash as ViewType) : 'overview';
  };

  // Function to update URL hash
  const updateHash = (route: RouteType) => {
    if (typeof window !== 'undefined') {
      if (typeof route === 'string') {
        window.location.hash = route;
      } else if (route.type === 'job-detail') {
        window.location.hash = `jobs/${route.jobId}`;
      } else if (route.type === 'action-detail') {
        window.location.hash = `actions/${route.actionId}`;
      }
    }
  };

  // Handle navigation
  const handleNavigate = (route: RouteType) => {
    setCurrentRoute(route);
    updateHash(route);
    setIsMobileMenuOpen(false); // Close mobile menu on navigation
  };

  // Navigate to job detail
  const handleJobClick = (jobId: string) => {
    handleNavigate({ type: 'job-detail', jobId });
  };

  // Navigate to action detail
  const handleActionClick = (actionId: string) => {
    handleNavigate({ type: 'action-detail', actionId });
  };

  // Navigate back to jobs list
  const handleBackToJobs = () => {
    handleNavigate('jobs');
  };

  // Navigate back to actions list
  const handleBackToActions = () => {
    handleNavigate('actions');
  };

  // Initialize route from URL hash on component mount
  useEffect(() => {
    const initialRoute = getRouteFromHash();
    setCurrentRoute(initialRoute);

    // Listen for hash changes (back/forward browser navigation)
    const handleHashChange = () => {
      const newRoute = getRouteFromHash();
      setCurrentRoute(newRoute);
    };

    window.addEventListener('hashchange', handleHashChange);
    return () => window.removeEventListener('hashchange', handleHashChange);
  }, []);

  const renderView = () => {
    if (typeof currentRoute === 'object') {
      if (currentRoute.type === 'job-detail') {
        return <JobDetail jobId={currentRoute.jobId} onBack={handleBackToJobs} onActionClick={handleActionClick} />;
      } else if (currentRoute.type === 'action-detail') {
        return <ActionDetail actionId={currentRoute.actionId} onBack={handleBackToActions} onJobClick={handleJobClick} />;
      }
    }
    
    switch (currentRoute) {
      case 'overview':
        return <OverviewView onNavigateToJob={(jobId) => handleNavigate({ type: 'job-detail', jobId })} />;
      case 'repos':
        return <ReposView />;
      case 'jobs':
        return <JobsView onJobClick={handleJobClick} />;
      case 'actions':
        return <ActionsView onActionClick={handleActionClick} />;
      case 'queue':
        return <QueueView />;
      default:
        return <OverviewView onNavigateToJob={(jobId) => handleNavigate({ type: 'job-detail', jobId })} />;
    }
  };

  // Get active tab for navigation highlighting
  const getActiveTab = (): ViewType => {
    if (typeof currentRoute === 'object') {
      if (currentRoute.type === 'job-detail') {
        return 'jobs';
      } else if (currentRoute.type === 'action-detail') {
        return 'actions';
      }
    }
    return currentRoute as ViewType;
  };

  return (
    <div className="space-y-4 sm:space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl sm:text-4xl font-bold tracking-tight">Dashboard</h1>
          <p className="text-sm sm:text-base text-muted-foreground">
            Monitor and manage your mirror repositories
          </p>
        </div>
      </div>

      <Card>
        <CardContent className="p-0">
          {/* Desktop Navigation */}
          <nav className="hidden sm:flex border-b">
            {tabs.map((tab) => {
              const Icon = tab.icon;
              return (
                <button
                  key={tab.id}
                  onClick={() => handleNavigate(tab.id)}
                  className={cn(
                    'flex items-center gap-2 px-6 py-4 text-sm font-medium transition-colors border-b-2 border-transparent',
                    getActiveTab() === tab.id
                      ? 'text-primary border-primary bg-muted/50'
                      : 'text-muted-foreground hover:text-foreground hover:bg-muted/30'
                  )}
                >
                  <Icon className="h-4 w-4" />
                  {tab.label}
                </button>
              );
            })}
          </nav>

          {/* Mobile Navigation */}
          <div className="sm:hidden">
            <div className="flex items-center justify-between p-4 border-b">
              <div className="flex items-center gap-2">
                {(() => {
                  const activeTab = tabs.find(tab => tab.id === getActiveTab());
                  const Icon = activeTab?.icon || Eye;
                  return (
                    <>
                      <Icon className="h-5 w-5" />
                      <span className="font-medium">{activeTab?.label || 'Overview'}</span>
                    </>
                  );
                })()}
              </div>
              <button
                onClick={() => setIsMobileMenuOpen(!isMobileMenuOpen)}
                className="p-2 -mr-2 text-muted-foreground hover:text-foreground"
              >
                {isMobileMenuOpen ? (
                  <X className="h-5 w-5" />
                ) : (
                  <Menu className="h-5 w-5" />
                )}
              </button>
            </div>

            {/* Mobile Menu Dropdown */}
            {isMobileMenuOpen && (
              <div className="border-b bg-muted/30">
                {tabs.map((tab) => {
                  const Icon = tab.icon;
                  return (
                    <button
                      key={tab.id}
                      onClick={() => handleNavigate(tab.id)}
                      className={cn(
                        'w-full flex items-center gap-3 px-4 py-3 text-left transition-colors',
                        getActiveTab() === tab.id
                          ? 'text-primary bg-primary/10'
                          : 'text-muted-foreground hover:text-foreground hover:bg-muted/50'
                      )}
                    >
                      <Icon className="h-5 w-5" />
                      <span className="font-medium">{tab.label}</span>
                    </button>
                  );
                })}
              </div>
            )}
          </div>
        </CardContent>
      </Card>

      <div className="min-h-[400px] sm:min-h-[600px]">
        {renderView()}
      </div>
    </div>
  );
}
