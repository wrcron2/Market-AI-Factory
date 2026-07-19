import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import { FactoryShell } from './components/layout/FactoryShell'
import { ProductsGrid } from './components/ProductsGrid'
import { ProductDetail } from './components/ProductDetail'
import { PipelinePage } from './components/PipelinePage'
import { WizardStart, WizardRun } from './components/wizard/AddWizard'

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
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
