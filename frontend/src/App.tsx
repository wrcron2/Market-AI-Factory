import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import { FactoryShell } from './components/layout/FactoryShell'
import { ProductsGrid } from './components/ProductsGrid'
import { ProductDetail } from './components/ProductDetail'
import { ProductDashboard } from './components/ProductDashboard'
import { PipelinePage } from './components/PipelinePage'
import { WizardStart, WizardRun } from './components/wizard/AddWizard'

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        {/* Dashboard lives OUTSIDE FactoryShell's <main> because it
            needs the full viewport for an iframe-mounted product UI.
            The ProductDashboard component owns its own top toolbar +
            back link. */}
        <Route path="/products/:name/dashboard" element={<ProductDashboard />} />
        <Route element={<FactoryShell />}>
          <Route path="/products" element={<ProductsGrid />} />
          <Route path="/products/:name" element={<ProductDetail />} />
          <Route path="/pipeline" element={<PipelinePage />} />
          <Route path="/wizard/new" element={<WizardStart />} />
          <Route path="/wizard/:runId" element={<WizardRun />} />
          <Route path="*" element={<Navigate to="/products" replace />} />
        </Route>
      </Routes>
    </BrowserRouter>
  )
}
