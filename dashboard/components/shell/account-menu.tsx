import { useNavigate } from "react-router-dom"
import { useTheme } from "next-themes"
import { Check, ChevronsUpDown, LogOut } from "lucide-react"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { api } from "@/lib/api"
import { cn } from "@/lib/utils"

export function AccountMenu({ compact }: { compact?: boolean }) {
  const navigate = useNavigate()
  const { theme, setTheme } = useTheme()

  const signOut = () => {
    api.clearToken()
    navigate("/login")
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className={cn(
          "hover:bg-sidebar-accent/60 flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left font-medium transition-colors",
          "focus-visible:ring-ring focus-visible:ring-2 focus-visible:outline-none",
          compact && "w-auto"
        )}
      >
        {!compact && <span className="truncate">Account</span>}
        <ChevronsUpDown className="text-muted-foreground ml-auto size-3.5 shrink-0" aria-hidden />
      </DropdownMenuTrigger>
      <DropdownMenuContent side="top" align="start" className="w-56">
        <DropdownMenuLabel>Appearance</DropdownMenuLabel>
        {(["light", "dark", "system"] as const).map((t) => (
          <DropdownMenuItem key={t} onClick={() => setTheme(t)}>
            <span className="capitalize">{t}</span>
            {theme === t && <Check className="ml-auto size-3.5" aria-hidden />}
          </DropdownMenuItem>
        ))}
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={signOut}>
          <LogOut aria-hidden />
          Sign out
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
