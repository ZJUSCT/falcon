import type { Metadata } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'Falcon Dashboard',
  description: 'Read-only dashboard for the Falcon Kubernetes controller',
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: `
          (() => {
            let theme;
            try { theme = localStorage.getItem('falcon-theme'); } catch {}
            const light = theme === 'light' || (theme !== 'dark' && window.matchMedia('(prefers-color-scheme: light)').matches);
            document.documentElement.classList.toggle('light', light);
          })();
        ` }} />
      </head>
      <body>
        <div className="h-screen bg-background flex overflow-hidden">
          {children}
        </div>
      </body>
    </html>
  );
}
