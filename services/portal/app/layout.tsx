import "./globals.css";
import Link from "next/link";

export const metadata = {
  title: "Automail",
  description: "Send a letter without a stamp.",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>
        <nav className="top">
          <Link href="/">Send</Link>
          <Link href="/track">Track</Link>
        </nav>
        {children}
      </body>
    </html>
  );
}
