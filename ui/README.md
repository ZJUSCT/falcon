# MirrorGo Web Dashboard

A modern Next.js web frontend for the MirrorGo mirror management system.

## Features

- **Real-time Dashboard**: Monitor repositories, jobs, actions, and queue in real-time
- **Modern UI**: Built with Next.js 14, React 18, TypeScript, and Tailwind CSS
- **Responsive Design**: Works seamlessly on desktop and mobile devices
- **Auto-refresh**: Automatically updates data every few seconds
- **Status Monitoring**: Visual status indicators for all components

## Getting Started

### Prerequisites

- Node.js 18+ 
- npm or yarn
- MirrorGo backend running on port 8080

### Installation

```bash
# Install dependencies
npm install

# Start development server
npm run dev

# Build for production
npm run build

# Start production server
npm start
```

The application will be available at http://localhost:3000

### API Integration

The frontend automatically proxies API calls to the Go backend running on port 8080:

- `/api/repos` - Repository configurations
- `/api/jobs` - Job statuses and schedules  
- `/api/actions` - Active container actions
- `/api/queue` - Current job queue

## Project Structure

```
ui/
├── app/                 # Next.js 14 app directory
│   ├── globals.css     # Global styles
│   ├── layout.tsx      # Root layout
│   └── page.tsx        # Main dashboard page
├── components/         # React components
│   ├── ui/            # Base UI components
│   ├── repos-view.tsx # Repository view
│   ├── jobs-view.tsx  # Jobs view
│   ├── actions-view.tsx # Actions view
│   └── queue-view.tsx # Queue view
├── lib/               # Utilities
│   ├── api.ts        # API client
│   └── utils.ts      # Helper functions
└── types/            # TypeScript definitions
    └── index.ts      # Data type interfaces
```

## Configuration

The application is configured to proxy API requests to `http://localhost:8080`. If your Go backend runs on a different port, update `next.config.js`:

```javascript
async rewrites() {
  return [
    {
      source: '/api/:path*',
      destination: 'http://localhost:YOUR_PORT/api/:path*',
    },
  ];
}
```

## Tech Stack

- **Framework**: Next.js 14 with App Router
- **Language**: TypeScript
- **Styling**: Tailwind CSS
- **Icons**: Lucide React
- **State**: React hooks with automatic refresh
- **Build**: Next.js built-in bundler

## Development

The project includes:

- TypeScript for type safety
- ESLint for code quality
- Tailwind CSS for styling
- Automatic code formatting
- Real-time data updates

## License

Same as MirrorGo main project.
