import { Navigate, Route, Routes } from "react-router-dom"
import { Toaster } from "@/components/ui/sonner"
import DesignPreview from "@/pages/DesignPreview"

export default function App() {
  return (
    <>
      <Routes>
        <Route path="/design" element={<DesignPreview />} />
        <Route path="*" element={<Navigate to="/design" replace />} />
      </Routes>
      <Toaster />
    </>
  )
}