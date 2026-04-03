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
        <div className="min-h-screen bg-background flex">
          {children}
        </div>
      </body>
    </html>
  );
}
