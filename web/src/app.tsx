import { lazy, Suspense, useState } from 'react'
import { Route, Routes } from 'react-router-dom'

import { AppShell } from './components/app-shell'
import { NewRunDialog } from './components/new-run-dialog'
import { NewSessionDialog } from './components/new-session-dialog'
import { HomePage } from './pages/home-page'

const SessionPage = lazy(() => import('./pages/session-page').then((module) => ({ default: module.SessionPage })))
const RunPage = lazy(() => import('./pages/run-page').then((module) => ({ default: module.RunPage })))

export function App() {
  const [newSessionOpen, setNewSessionOpen] = useState(false)
  const [newRunOpen, setNewRunOpen] = useState(false)
  return (
    <AppShell onNewSession={() => setNewSessionOpen(true)}>
      <Routes>
        <Route path="/" element={<HomePage onNewSession={() => setNewSessionOpen(true)} onNewRun={() => setNewRunOpen(true)} />} />
        <Route path="/sessions/:id" element={<Suspense fallback={<main className="route-placeholder" aria-label="Session report loading boundary" />}><SessionPage /></Suspense>} />
        <Route path="/runs/:id" element={<Suspense fallback={<main className="route-placeholder" aria-label="Run report loading boundary" />}><RunPage /></Suspense>} />
      </Routes>
      <NewSessionDialog open={newSessionOpen} onOpenChange={setNewSessionOpen} />
      <NewRunDialog open={newRunOpen} onClose={() => setNewRunOpen(false)} />
    </AppShell>
  )
}
