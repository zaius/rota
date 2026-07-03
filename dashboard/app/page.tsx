import { Navigate } from "react-router-dom"

export default function Home() {
  const token = localStorage.getItem("auth_token")
  return <Navigate to={token ? "/dashboard" : "/login"} replace />
}
