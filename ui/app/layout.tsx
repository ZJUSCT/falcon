import type { Metadata } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'MirrorGo Dashboard',
  description: 'Cluster-aware mirror management dashboard',
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <div className="h-screen bg-background flex overflow-hidden">
          {children}
        </div>
      </body>
    </html>
  );
}
