import "./globals.css";
import type { Metadata, Viewport } from "next";
import { AuthProvider } from "@/lib/auth";
import Nav from "./nav";

export const metadata: Metadata = {
  title: {
    default: "Automail",
    template: "%s · Automail",
  },
  description:
    "Send a document to a real mailbox. Encrypted in your browser, printed on arrival, never readable in between.",
};

export const viewport: Viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#fbfaf7" },
    { media: "(prefers-color-scheme: dark)", color: "#0e1117" },
  ],
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>
        {/* The par-avion chevron edge, across the top of every page. */}
        <div className="airmail-band" aria-hidden="true" />
        <AuthProvider>
          <Nav />
          {children}
          <footer className="site-footer">
            <span>Encrypted in your browser</span>
            <span className="dot" aria-hidden="true">
              ·
            </span>
            <span>Zero-knowledge relay</span>
            <span className="dot" aria-hidden="true">
              ·
            </span>
            <span>Printed and wiped</span>
          </footer>
        </AuthProvider>
      </body>
    </html>
  );
}
