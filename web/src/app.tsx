import { lazy, Suspense, useState } from 'react'
import { Route, Routes } from 'react-router-dom'

import { AppShell } from './components/app-shell'
import { NewSessionDialog } from './components/new-session-dialog'
import { HomePage } from './pages/home-page'

const SessionPage = lazy(() => import('./pages/session-page').then((module) => ({ default: module.SessionPage })))

export function App() {
  const [newSessionOpen, setNewSessionOpen] = useState(false)
  return (
    <AppShell onNewSession={() => setNewSessionOpen(true)}>
      <Routes>
        <Route path="/" element={<HomePage onNewSession={() => setNewSessionOpen(true)} />} />
        <Route path="/sessions/:id" element={<Suspense fallback={<main className="route-placeholder" aria-label="Session report loading boundary" />}><SessionPage /></Suspense>} />
      </Routes>
      <NewSessionDialog open={newSessionOpen} onOpenChange={setNewSessionOpen} />
    </AppShell>
  )
}
