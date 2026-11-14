import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter } from 'react-router-dom'
import { Toaster } from 'sonner'
import { useTheme } from '@/lib/theme'
import App from '@/app'
import '@/styles.css'

const queryClient = new QueryClient({ defaultOptions: { queries: { staleTime: 20_000, retry: false, refetchOnWindowFocus: false }, mutations: { retry: false } } })
createRoot(document.getElementById('root')!).render(<StrictMode><QueryClientProvider client={queryClient}><BrowserRouter><App /></BrowserRouter><ThemedToaster /></QueryClientProvider></StrictMode>)

// Toasts follow the resolved theme so they never show dark-on-light or vice versa.
function ThemedToaster() {
  const { resolved } = useTheme()
  return <Toaster theme={resolved} richColors closeButton position="top-right" toastOptions={{ style: { fontFamily: 'inherit' } }} />
}
