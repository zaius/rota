import * as React from "react"
import { Eye, EyeOff } from "lucide-react"
import { toast } from "@/lib/toast"
import { Label } from "@/components/ui/label"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { PageHeader, Section, LoadingLine } from "@/components/page-header"
import { api } from "@/lib/api"
import type { Settings } from "@/lib/types"
import { cn, errorMessage } from "@/lib/utils"

/** Label + control + one-line hint, in a form grid. */
function Field({
  id,
  label,
  hint,
  children,
  className,
}: {
  id?: string
  label: React.ReactNode
  hint?: React.ReactNode
  children: React.ReactNode
  className?: string
}) {
  return (
    <div className={cn("space-y-1.5", className)}>
      <Label htmlFor={id}>{label}</Label>
      {children}
      {hint && <p className="text-muted-foreground text-[0.6875rem] leading-4">{hint}</p>}
    </div>
  )
}

/** Boolean setting as a row: what it does on the left, the switch on the right. */
function SwitchRow({
  id,
  label,
  hint,
  checked,
  onChange,
}: {
  id: string
  label: React.ReactNode
  hint?: React.ReactNode
  checked: boolean
  onChange: (v: boolean) => void
}) {
  return (
    <div className="border-border flex items-start justify-between gap-6 border-b py-3 last:border-b-0">
      <div className="min-w-0">
        <Label htmlFor={id} className="text-foreground cursor-pointer">
          {label}
        </Label>
        {hint && <p className="text-muted-foreground mt-1 text-[0.6875rem] leading-4">{hint}</p>}
      </div>
      <Switch id={id} checked={checked} onCheckedChange={onChange} />
    </div>
  )
}

const grid = "grid gap-x-8 gap-y-4 sm:grid-cols-2 lg:grid-cols-3"

// num parses a numeric input, keeping fallback while the field is empty or
// mid-edit.
const num = (v: string, fallback = 0) => {
  const n = parseInt(v)
  return Number.isNaN(n) ? fallback : n
}

export default function SettingsPage() {
  const [settings, setSettings] = React.useState<Settings | null>(null)
  const [isLoading, setIsLoading] = React.useState(true)
  const [isSaving, setIsSaving] = React.useState(false)
  const [resetOpen, setResetOpen] = React.useState(false)
  // Raw textarea text; the header list parses from it on change so a trailing
  // newline survives typing.
  const [headersText, setHeadersText] = React.useState<string | null>(null)

  // Admin account
  const [adminUsername, setAdminUsername] = React.useState("")
  const [newUsername, setNewUsername] = React.useState("")
  const [currentPass, setCurrentPass] = React.useState("")
  const [newPass, setNewPass] = React.useState("")
  const [confirmPass, setConfirmPass] = React.useState("")
  const [showPass, setShowPass] = React.useState(false)
  const [changingPass, setChangingPass] = React.useState(false)

  React.useEffect(() => {
    const fetchSettings = async () => {
      try {
        const [data, adminInfo] = await Promise.all([api.getSettings(), api.getAdminInfo()])
        setSettings(data)
        setAdminUsername(adminInfo.username)
        setNewUsername(adminInfo.username)
      } catch (error) {
        toast.error("Failed to load settings", errorMessage(error, "Unknown error"))
      } finally {
        setIsLoading(false)
      }
    }
    fetchSettings()
  }, [])

  const patch = <K extends keyof Settings>(key: K, value: Partial<Settings[K]>) =>
    setSettings((s) => (s ? { ...s, [key]: { ...s[key], ...value } } : s))

  const handleChangePassword = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!currentPass) return toast.error("Enter your current password")
    if (!newPass) return toast.error("Enter a new password")
    if (newPass.length < 6) return toast.error("New password must be at least 6 characters")
    if (newPass !== confirmPass) return toast.error("Passwords don't match")

    setChangingPass(true)
    try {
      const opts: Parameters<typeof api.changePassword>[0] = { current_password: currentPass, new_password: newPass }
      if (newUsername && newUsername !== adminUsername) opts.new_username = newUsername
      const res = await api.changePassword(opts)
      setAdminUsername(res.username)
      setNewUsername(res.username)
      setCurrentPass("")
      setNewPass("")
      setConfirmPass("")
      toast.success("Credentials updated")
    } catch (err) {
      toast.error("Failed to change password", errorMessage(err, "Unknown error"))
    } finally {
      setChangingPass(false)
    }
  }

  const handleSave = async () => {
    if (!settings) return
    try {
      setIsSaving(true)
      await api.updateSettings(settings)
      toast.success("Settings saved")
    } catch (err) {
      toast.error("Failed to save settings", errorMessage(err, "Unknown error"))
    } finally {
      setIsSaving(false)
    }
  }

  const handleReset = async () => {
    try {
      setIsSaving(true)
      const response = await api.resetSettings()
      setSettings(response.config)
      setHeadersText(null)
      toast.success("Settings reset to defaults")
    } catch (err) {
      toast.error("Failed to reset settings", errorMessage(err, "Unknown error"))
    } finally {
      setIsSaving(false)
      setResetOpen(false)
    }
  }

  if (isLoading) return <LoadingLine />
  if (!settings) return <LoadingLine>Settings could not be loaded. Check that the core API is reachable, then reload.</LoadingLine>

  return (
    <>
      <PageHeader
        title="Settings"
        description="Runtime configuration of the core. Saving applies everything below at once; the admin account section saves on its own."
      >
        <Button variant="outline" onClick={() => setResetOpen(true)} disabled={isSaving}>
          Reset to defaults
        </Button>
        <Button onClick={handleSave} disabled={isSaving}>
          {isSaving ? "Saving…" : "Save settings"}
        </Button>
      </PageHeader>

      {/* Admin account */}
      <Section
        title="Admin account"
        description={
          <>
            Dashboard sign-in. Signed in as <span className="font-mono">{adminUsername}</span>.
          </>
        }
      >
        <form onSubmit={handleChangePassword} className="max-w-2xl">
          <div className={grid}>
            <Field id="admin-username" label="Username" hint="Leave unchanged to keep the current one.">
              <Input
                id="admin-username"
                className="font-mono"
                value={newUsername}
                onChange={(e) => setNewUsername(e.target.value)}
                autoComplete="username"
              />
            </Field>
            <Field id="admin-current" label="Current password" hint="Required to confirm any change.">
              <div className="relative">
                <Input
                  id="admin-current"
                  type={showPass ? "text" : "password"}
                  value={currentPass}
                  onChange={(e) => setCurrentPass(e.target.value)}
                  className="pr-8"
                  autoComplete="current-password"
                />
                <button
                  type="button"
                  className="text-muted-foreground hover:text-foreground absolute top-1/2 right-2 -translate-y-1/2"
                  onClick={() => setShowPass((v) => !v)}
                  aria-label={showPass ? "Hide passwords" : "Show passwords"}
                >
                  {showPass ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
                </button>
              </div>
            </Field>
            <div className="hidden lg:block" />
            <Field id="admin-new" label="New password" hint="At least 6 characters.">
              <Input id="admin-new" type={showPass ? "text" : "password"} value={newPass} onChange={(e) => setNewPass(e.target.value)} autoComplete="new-password" />
            </Field>
            <Field id="admin-confirm" label="Confirm new password">
              <Input
                id="admin-confirm"
                type={showPass ? "text" : "password"}
                value={confirmPass}
                onChange={(e) => setConfirmPass(e.target.value)}
                autoComplete="new-password"
              />
            </Field>
          </div>
          <Button type="submit" variant="outline" className="mt-4" disabled={changingPass || !currentPass || !newPass}>
            {changingPass ? "Updating…" : "Update credentials"}
          </Button>
        </form>
      </Section>

      {/* Upstream requests: rotation strategy itself is per pool */}
      <Section title="Upstream requests" description="Request handling applied to every pool. Rotation strategy is set per pool on the Pools page.">
        <div className={grid}>
          <Field id="rotation-timeout" label="Timeout (seconds)" hint="Per upstream attempt.">
            <Input
              id="rotation-timeout"
              type="number"
              min={1}
              value={settings.rotation.timeout}
              onChange={(e) => patch("rotation", { timeout: num(e.target.value) })}
            />
          </Field>
        </div>
        <div className="mt-4 max-w-2xl">
          <SwitchRow
            id="follow-redirect"
            label="Follow redirects"
            hint="Resolve 3xx responses upstream instead of passing them to the client."
            checked={settings.rotation.follow_redirect}
            onChange={(v) => patch("rotation", { follow_redirect: v })}
          />
        </div>
      </Section>

      {/* Health check */}
      <Section title="Health check" description="The GET each proxy must pass to count as active.">
        <div className={grid}>
          <Field id="healthcheck-url" label="URL" hint="GET only." className="sm:col-span-2">
            <Input
              id="healthcheck-url"
              type="url"
              className="font-mono"
              value={settings.healthcheck.url}
              onChange={(e) => patch("healthcheck", { url: e.target.value })}
            />
          </Field>
          <Field id="healthcheck-status" label="Expected status">
            <Input
              id="healthcheck-status"
              type="number"
              min={100}
              max={599}
              value={settings.healthcheck.status}
              onChange={(e) => patch("healthcheck", { status: num(e.target.value, 200) })}
            />
          </Field>
          <Field id="healthcheck-timeout" label="Timeout (seconds)">
            <Input
              id="healthcheck-timeout"
              type="number"
              min={1}
              value={settings.healthcheck.timeout}
              onChange={(e) => patch("healthcheck", { timeout: num(e.target.value) })}
            />
          </Field>
          <Field id="healthcheck-workers" label="Workers" hint="Concurrent checks.">
            <Input
              id="healthcheck-workers"
              type="number"
              min={1}
              value={settings.healthcheck.workers}
              onChange={(e) => patch("healthcheck", { workers: num(e.target.value) })}
            />
          </Field>
          <Field id="healthcheck-headers" label="Headers" hint="One per line, as Key: Value." className="sm:col-span-2 lg:col-span-3">
            <Textarea
              id="healthcheck-headers"
              rows={3}
              className="max-w-2xl font-mono text-[0.75rem]"
              placeholder="User-Agent: Rota/1.0"
              value={headersText ?? settings.healthcheck.headers.join("\n")}
              onChange={(e) => {
                setHeadersText(e.target.value)
                patch("healthcheck", { headers: e.target.value.split("\n").filter((h) => h.trim()) })
              }}
            />
          </Field>
        </div>
        <div className="mt-4 max-w-2xl">
          <SwitchRow
            id="healthcheck-strict-tls"
            label="Strict TLS"
            hint="Fail proxies that present an expired or untrusted certificate for the check URL."
            checked={settings.healthcheck.strict_tls ?? false}
            onChange={(v) => patch("healthcheck", { strict_tls: v })}
          />
        </div>
      </Section>

      {/* Proxy cleanup */}
      <Section
        title="Proxy cleanup"
        description="Periodic removal of proxies that stay dead or keep failing. Deleted proxies leave every pool and come back only if a source lists them again."
        className="border-b-0"
      >
        <div className="max-w-2xl">
          <SwitchRow
            id="cleanup-enabled"
            label="Remove dead proxies automatically"
            hint="Off, the inventory only shrinks when you delete proxies yourself or a source's own cleanup runs."
            checked={settings.proxy_cleanup.enabled}
            onChange={(v) => patch("proxy_cleanup", { enabled: v })}
          />
        </div>
        {settings.proxy_cleanup.enabled && (
          <div className={cn(grid, "mt-4")}>
            <Field id="cleanup-failed-days" label="Failed for (days)" hint="Delete failed proxies not checked for this long. 0 disables.">
              <Input
                id="cleanup-failed-days"
                type="number"
                min={0}
                value={settings.proxy_cleanup.max_failed_days}
                onChange={(e) => patch("proxy_cleanup", { max_failed_days: num(e.target.value) })}
              />
            </Field>
            <Field
              id="cleanup-min-success"
              label="Min success rate (%)"
              hint="Delete proxies below this rate over the last 7 days, with at least 10 requests. 0 disables."
            >
              <Input
                id="cleanup-min-success"
                type="number"
                min={0}
                max={100}
                value={settings.proxy_cleanup.min_success_rate}
                onChange={(e) => patch("proxy_cleanup", { min_success_rate: parseFloat(e.target.value) || 0 })}
              />
            </Field>
            <Field id="cleanup-interval" label="Run every (hours)" hint="0 uses the default of 24 hours.">
              <Input
                id="cleanup-interval"
                type="number"
                min={0}
                value={settings.proxy_cleanup.cleanup_interval_hours}
                onChange={(e) => patch("proxy_cleanup", { cleanup_interval_hours: num(e.target.value) })}
              />
            </Field>
          </div>
        )}
      </Section>

      <AlertDialog open={resetOpen} onOpenChange={setResetOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Reset every setting to its default?</AlertDialogTitle>
            <AlertDialogDescription>The core switches to defaults immediately. The admin account and proxy users are not touched.</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={handleReset}>Reset</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
