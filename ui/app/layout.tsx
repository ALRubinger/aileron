import type { Metadata } from 'next'

export const metadata: Metadata = {
  title: 'Aileron',
  description: 'Agentic control plane — policy, approvals, and audit for AI agents',
}

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  )
}
