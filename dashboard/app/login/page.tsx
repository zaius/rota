import { useState, useEffect } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Card, CardContent } from "@/components/ui/card";
import { api } from "@/lib/api";

export default function LoginPage() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [isLoading, setIsLoading] = useState(false);
  const [error, setError] = useState("");
  const [sessionExpired, setSessionExpired] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  // Check if already logged in or redirected due to expired session
  useEffect(() => {
    if (searchParams.get("reason") === "session_expired") {
      setSessionExpired(true);
    } else {
      const token = localStorage.getItem("auth_token");
      if (token) {
        navigate("/dashboard");
      }
    }
  }, [navigate, searchParams]);

  const handleSubmit = async (e: React.FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    setIsLoading(true);
    setError("");

    try {
      await api.login(username, password);
      navigate("/dashboard");
    } catch (err: any) {
      setError(err.message || "Invalid credentials. Please try again.");
      console.error("Login failed:", err);
    } finally {
      setIsLoading(false);
    }
  };

  return (
    <div className="relative flex min-h-screen flex-col items-center justify-center bg-background px-4">
      <div className="w-full max-w-[400px]">
        {/* Login card */}
        <Card className="border-[#333333] bg-transparent">
          <CardContent className="space-y-12 px-8 pb-8 pt-10">
            {/* Logo and Title */}
            <div className="flex flex-col items-center gap-6 text-center">
              <img
                src="/logo.png"
                alt="Rota Logo"
                width={80}
                height={80}
                className="object-contain"
              />
              <h1 className="text-[40px] font-semibold leading-none tracking-tight">Login to Rota</h1>
            </div>

            {/* Login form */}
            <form onSubmit={handleSubmit} className="space-y-4">
              {sessionExpired && !error && (
                <div className="rounded-lg bg-yellow-500/10 border border-yellow-500/20 p-3 text-sm text-yellow-500">
                  Your session has expired. Please log in again.
                </div>
              )}
              {error && (
                <div className="rounded-lg bg-red-500/10 border border-red-500/20 p-3 text-sm text-red-500">
                  {error}
                </div>
              )}
              <Input
                id="username"
                type="text"
                placeholder="Username"
                required
                autoComplete="username"
                disabled={isLoading}
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                className="h-14 rounded-lg !text-[17px] placeholder:text-[17px]"
                style={{ fontSize: '17px' }}
              />
              <Input
                id="password"
                type="password"
                placeholder="Password"
                required
                autoComplete="current-password"
                disabled={isLoading}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                className="h-14 rounded-lg !text-[17px] placeholder:text-[17px]"
                style={{ fontSize: '17px' }}
              />
              <Button
                type="submit"
                className="h-14 w-full rounded-lg text-[17px] font-medium"
                disabled={isLoading}
              >
                {isLoading ? (
                  <div className="flex items-center gap-2">
                    <div className="h-4 w-4 animate-spin rounded-full border-2 border-current border-t-transparent" />
                    Logging in...
                  </div>
                ) : (
                  "Login"
                )}
              </Button>
            </form>
          </CardContent>
        </Card>
      </div>

      {/* Version info */}
      <div className="absolute bottom-8 text-center">
        <p className="text-sm text-muted-foreground">
          Version 1.0.0
        </p>
      </div>
    </div>
  );
}
