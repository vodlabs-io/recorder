import React from 'react'
import { AppSidebar } from "@/components/app-sidebar"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Separator } from "@/components/ui/separator"
import {
  SidebarInset,
  SidebarProvider,
  SidebarTrigger,
} from "@/components/ui/sidebar"
import { ThemeProvider } from "@/components/theme-provider"
import { TooltipProvider } from "@/components/ui/tooltip"
import { QuitDialog } from "@/components/quit-dialog"
import { WelcomeDialog } from "@/components/welcome-dialog"
import { VideoPlayerDialog } from "@/components/video-player-dialog"
import { HashRouter, Routes, Route, useLocation, Navigate } from 'react-router-dom'
import SettingsGeneral from './screens/SettingsGeneral'
import Dashboard from './screens/Dashboard'
import SettingsFiles from './screens/SettingsFiles'
import SettingsRecording from './screens/SettingsRecording'
import SettingsProcessing from './screens/SettingsProcessing'
import About from './screens/About'
import Accounts from './screens/Accounts'
import Update from './screens/Update'
import VODArchive from './screens/vod-archive'
import Encoding from './screens/encoding'
import { appStore } from './state/app-state'
import { getDaemonClient } from '@/lib/daemon'

const DynamicHashBreadcrumbs = () => {
  const location = useLocation();
  const pathSegments = location.pathname.substring(1).split('/').filter(Boolean);

  const breadcrumbItems = pathSegments.map((segment, index) => {
    const href = '#' + pathSegments.slice(0, index + 1).join('/');
    const isLast = index === pathSegments.length - 1;
    const capitalizedSegment = segment.charAt(0).toUpperCase() + segment.slice(1);

    return (
      <React.Fragment key={href}>
        <BreadcrumbItem>
          <BreadcrumbPage>
            {capitalizedSegment}
          </BreadcrumbPage>
        </BreadcrumbItem>
        {!isLast && <BreadcrumbSeparator />}
      </React.Fragment>
    );
  });

  return (
    <Breadcrumb>
      <BreadcrumbList>
        {breadcrumbItems}
      </BreadcrumbList>
    </Breadcrumb>
  );
};

function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/dashboard" />} />
      <Route path="/dashboard" element={<Dashboard />} />
      <Route path="/vods" element={<VODArchive />} />
      <Route path="/encoding" element={<Encoding />} />
      <Route path="/accounts" element={<Accounts />} />
      <Route path="/update" element={<Update />} />
      <Route path="/settings/general" element={<SettingsGeneral />} />
      <Route path="/settings/files" element={<SettingsFiles />} />
      <Route path="/settings/recording" element={<SettingsRecording />} />
      <Route path="/settings/processing" element={<SettingsProcessing />} />
      <Route path="/about" element={<About />} />
    </Routes>
  )
}

function App() {

  React.useEffect(() => {
    appStore.getState().hydratePrefs()
    appStore.getState().initDaemon()
    // App-wide so the sidebar badge reflects update availability on any screen,
    // not just the Update tab.
    appStore.getState().initUpdates()
  }, [])

  return (
    <HashRouter basename={"/"}>
      <ThemeProvider defaultTheme="system" storageKey="vite-ui-theme">
        {/* App-wide tooltip provider (shadcn tooltips don't self-provide); its
            default delay=0 keeps tooltips instant. */}
        <TooltipProvider>
        {/* The quit flow only exists where the host owns the daemon lifecycle
            (desktop); the Web UI just closes its tab. */}
        {getDaemonClient().capabilities.quit && <QuitDialog />}
        <WelcomeDialog />
        {getDaemonClient().capabilities.browserMedia && <VideoPlayerDialog />}
        <SidebarProvider>
          <AppSidebar />
          <SidebarInset>
            <header className="flex h-16 shrink-0 items-center gap-2 transition-[width,height] ease-linear group-has-data-[collapsible=icon]/sidebar-wrapper:h-12">
              <div className="flex items-center gap-2 px-4">
                <SidebarTrigger className="-ml-1" />
                <Separator
                  orientation="vertical"
                  className="mr-2 data-[orientation=vertical]:h-4"
                />
                <DynamicHashBreadcrumbs />
              </div>
            </header>
            <AppRoutes />
          </SidebarInset>
        </SidebarProvider>
        </TooltipProvider>
      </ThemeProvider>
    </HashRouter>
  )
}

export default App
