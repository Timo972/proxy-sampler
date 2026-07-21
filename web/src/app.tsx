import { useState } from 'react'
import { Route, Routes } from 'react-router-dom'

import { AppShell } from './components/app-shell'
import { NewSessionDialog } from './components/new-session-dialog'
import { HomePage } from './pages/home-page'

export function App() {
  const [newSessionOpen, setNewSessionOpen] = useState(false)
  return (
    <AppShell onNewSession={() => setNewSessionOpen(true)}>
      <Routes>
        <Route path="/" element={<HomePage onNewSession={() => setNewSessionOpen(true)} />} />
        <Route path="/sessions/:id" element={<main className="route-placeholder" aria-label="Session report loading boundary" />} />
      </Routes>
      <NewSessionDialog open={newSessionOpen} onOpenChange={setNewSessionOpen} />
    </AppShell>
  )
}
