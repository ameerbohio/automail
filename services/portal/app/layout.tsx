import "./globals.css";
import { AuthProvider } from "@/lib/auth";
import Nav from "./nav";

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
        <AuthProvider>
          <Nav />
          {children}
        </AuthProvider>
      </body>
    </html>
  );
}
