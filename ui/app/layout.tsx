import type { Metadata } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'Falcon Dashboard',
  description: 'Read-only dashboard for the Falcon Kubernetes controller',
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
