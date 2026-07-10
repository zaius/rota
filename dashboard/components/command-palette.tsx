
import * as React from "react"
import { useNavigate } from "react-router-dom"
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command"
import {
  LayoutDashboard,
  Network,
  FileText,
  Settings,
  LogOut,
  Activity
} from "lucide-react"
import { api } from "@/lib/api"

export function CommandPalette() {
  const [open, setOpen] = React.useState(false)
  const navigate = useNavigate()

  React.useEffect(() => {
    const down = (e: KeyboardEvent) => {
      if (e.key === "k" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault()
        setOpen((open) => !open)
      }
    }

    document.addEventListener("keydown", down)
    return () => document.removeEventListener("keydown", down)
  }, [])

  const runCommand = React.useCallback((command: () => void) => {
    setOpen(false)
    command()
  }, [])

  return (
    <CommandDialog open={open} onOpenChange={setOpen}>
      <CommandInput placeholder="Type a command or search..." />
      <CommandList>
        <CommandEmpty>No results found.</CommandEmpty>
        <CommandGroup heading="Navigation">
          <CommandItem
            onSelect={() => runCommand(() => navigate("/dashboard"))}
          >
            <LayoutDashboard className="mr-2 h-4 w-4" />
            <span>Dashboard</span>
          </CommandItem>
          <CommandItem
            onSelect={() => runCommand(() => navigate("/dashboard/proxies"))}
          >
            <Network className="mr-2 h-4 w-4" />
            <span>Proxy Management</span>
          </CommandItem>
          <CommandItem
            onSelect={() => runCommand(() => navigate("/dashboard/metrics"))}
          >
            <Activity className="mr-2 h-4 w-4" />
            <span>System Metrics</span>
          </CommandItem>
          <CommandItem
            onSelect={() => runCommand(() => navigate("/dashboard/logs"))}
          >
            <FileText className="mr-2 h-4 w-4" />
            <span>Proxy Logs</span>
          </CommandItem>
          <CommandItem
            onSelect={() => runCommand(() => navigate("/dashboard/settings"))}
          >
            <Settings className="mr-2 h-4 w-4" />
            <span>Settings</span>
          </CommandItem>
        </CommandGroup>
        <CommandGroup heading="Actions">
          <CommandItem
            onSelect={() =>
              runCommand(() => {
                // Navigating alone left the token in localStorage, so the next
                // visit to /dashboard walked straight back in.
                api.clearToken()
                navigate("/login")
              })
            }
          >
            <LogOut className="mr-2 h-4 w-4" />
            <span>Logout</span>
          </CommandItem>
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  )
}
