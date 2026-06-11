import type { Metadata } from "next";
import Link from "next/link";
import { Lora, DM_Sans, JetBrains_Mono } from "next/font/google";
import "./globals.css";

// ── Google Fonts — self-hosted via next/font ──────────────────────────────
// These CSS variables are declared in @theme (globals.css) and referenced
// in @layer base html { font-family: var(--font-sans) }.

const lora = Lora({
  subsets: ["latin"],
  weight: ["400", "500", "600", "700"],
  style: ["normal", "italic"],
  variable: "--font-serif",
  display: "swap",
});

const dmSans = DM_Sans({
  subsets: ["latin"],
  weight: ["300", "400", "500", "600"],
  variable: "--font-sans",
  display: "swap",
});

const jetbrainsMono = JetBrains_Mono({
  subsets: ["latin"],
  weight: ["400", "500"],
  variable: "--font-mono",
  display: "swap",
});

export const metadata: Metadata = {
  title: "Second Brain",
  description: "Personal second brain — search, collect, govern.",
};

interface RootLayoutProps {
  children: React.ReactNode;
}

export default function RootLayout({ children }: RootLayoutProps) {
  return (
    <html
      lang="ko"
      className={`${lora.variable} ${dmSans.variable} ${jetbrainsMono.variable}`}
      suppressHydrationWarning
    >
      <body className="min-h-screen bg-background text-foreground antialiased">
        {/* Site header */}
        <header className="sticky top-0 z-40 border-b border-border bg-background/90 backdrop-blur-sm">
          <div className="mx-auto flex max-w-4xl items-center justify-between px-4 py-3">
            <Link
              href="/"
              className="font-serif text-lg font-semibold tracking-tight text-foreground transition-colors hover:text-accent"
            >
              Second Brain
            </Link>
            <nav className="flex items-center gap-1 text-sm">
              <Link
                href="/"
                className="rounded-md px-3 py-1.5 text-foreground-muted transition-colors hover:bg-surface-subtle hover:text-foreground"
              >
                검색
              </Link>
              <Link
                href="/dashboard"
                className="rounded-md px-3 py-1.5 text-foreground-muted transition-colors hover:bg-surface-subtle hover:text-foreground"
              >
                수집 현황
              </Link>
              <Link
                href="/governance"
                className="rounded-md px-3 py-1.5 text-foreground-muted transition-colors hover:bg-surface-subtle hover:text-foreground"
              >
                거버넌스
              </Link>
            </nav>
          </div>
        </header>

        {/* Page content */}
        <main className="mx-auto max-w-4xl px-4 py-8">{children}</main>
      </body>
    </html>
  );
}
