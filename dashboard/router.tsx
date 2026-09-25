import { createBrowserRouter } from "react-router-dom"

import RootLayout from "@/app/layout"
import IndexRedirect from "@/app/page"
import ErrorPage from "@/app/error"
import NotFound from "@/app/not-found"
import LoginPage from "@/app/login/page"
import DashboardLayout from "@/app/dashboard/layout"
import DashboardPage from "@/app/dashboard/page"
import ProxiesPage from "@/app/dashboard/proxies/page"
import PoolsPage from "@/app/dashboard/pools/page"
import SourcesPage from "@/app/dashboard/sources/page"
import UsersPage from "@/app/dashboard/users/page"
import SettingsPage from "@/app/dashboard/settings/page"

// The pathless routes carrying errorElement keep a crash inside the theme
// provider, and a crash in one dashboard page inside the navigation.
export const router = createBrowserRouter([
  {
    element: <RootLayout />,
    children: [
      {
        errorElement: <ErrorPage />,
        children: [
          { index: true, element: <IndexRedirect /> },
          { path: "login", element: <LoginPage /> },
          {
            element: <DashboardLayout />,
            children: [
              {
                errorElement: <ErrorPage />,
                children: [
                  { path: "dashboard", element: <DashboardPage /> },
                  { path: "dashboard/proxies", element: <ProxiesPage /> },
                  { path: "dashboard/pools", element: <PoolsPage /> },
                  { path: "dashboard/sources", element: <SourcesPage /> },
                  { path: "dashboard/users", element: <UsersPage /> },
                  { path: "dashboard/settings", element: <SettingsPage /> },
                ],
              },
            ],
          },
          { path: "*", element: <NotFound /> },
        ],
      },
    ],
  },
])
