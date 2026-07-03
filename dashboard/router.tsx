import { createBrowserRouter } from "react-router-dom"

import RootLayout from "@/app/layout"
import IndexRedirect from "@/app/page"
import LoginPage from "@/app/login/page"
import DashboardLayout from "@/app/dashboard/layout"
import DashboardPage from "@/app/dashboard/page"
import ProxiesPage from "@/app/dashboard/proxies/page"
import PoolsPage from "@/app/dashboard/pools/page"
import SourcesPage from "@/app/dashboard/sources/page"
import UsersPage from "@/app/dashboard/users/page"
import MetricsPage from "@/app/dashboard/metrics/page"
import LogsPage from "@/app/dashboard/logs/page"
import SettingsPage from "@/app/dashboard/settings/page"

export const router = createBrowserRouter([
  {
    element: <RootLayout />,
    children: [
      { index: true, element: <IndexRedirect /> },
      { path: "login", element: <LoginPage /> },
      {
        element: <DashboardLayout />,
        children: [
          { path: "dashboard", element: <DashboardPage /> },
          { path: "dashboard/proxies", element: <ProxiesPage /> },
          { path: "dashboard/pools", element: <PoolsPage /> },
          { path: "dashboard/sources", element: <SourcesPage /> },
          { path: "dashboard/users", element: <UsersPage /> },
          { path: "dashboard/metrics", element: <MetricsPage /> },
          { path: "dashboard/logs", element: <LogsPage /> },
          { path: "dashboard/settings", element: <SettingsPage /> },
        ],
      },
    ],
  },
])
