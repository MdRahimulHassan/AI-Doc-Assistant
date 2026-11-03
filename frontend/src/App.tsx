import { Route, Routes } from "react-router-dom"
import { Toaster } from "@/components/ui/sonner"
import { AppLayout } from "@/components/AppLayout"
import { AuthGate } from "@/components/AuthGate"
import { GuestRoute } from "@/components/GuestRoute"
import { ProtectedRoute } from "@/components/ProtectedRoute"
import { AuthProvider } from "@/context/AuthProvider"
import DashboardPage from "@/pages/DashboardPage"
import DesignPreview from "@/pages/DesignPreview"
import LoginPage from "@/pages/LoginPage"
import NotFoundPage from "@/pages/NotFoundPage"
import RegisterPage from "@/pages/RegisterPage"
import DocumentPage from "@/pages/DocumentPage"

export default function App() {
  return (
    <AuthProvider>
      <AuthGate>
        <Routes>
          <Route element={<GuestRoute />}>
            <Route path="/login" element={<LoginPage />} />
            <Route path="/register" element={<RegisterPage />} />
          </Route>

          <Route element={<ProtectedRoute />}>
            <Route element={<AppLayout />}>
              <Route index element={<DashboardPage />} />
              <Route path="/documents/:id" element={<DocumentPage />} />
            </Route>
          </Route>

          {import.meta.env.DEV && <Route path="/design" element={<DesignPreview />} />}
          <Route path="*" element={<NotFoundPage />} />
        </Routes>
      </AuthGate>

      {/* Outside the gate so the "session expired" toast can show while the app is still booting. */}
      <Toaster />
    </AuthProvider>
  )
}