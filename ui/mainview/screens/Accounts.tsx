import { useMemo, useState } from "react"
import {
  ChevronDown,
  ChevronsUpDown,
  ChevronUp,
  Info,
  ShieldCheck,
} from "lucide-react"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { cn } from "@/lib/utils"
import { ConnectionBanner } from "@/components/connection-banner"
import { PasteCookiesDialog } from "@/components/paste-cookies-dialog"
import { appStore } from "@/state/app-state"
import { getDaemonClient, type PlatformLoginView } from "@/lib/daemon"
import { platformDisplayName } from "@/data/supported-platforms"

type Row = {
  platform: string
  displayName: string
  account: PlatformLoginView | null // null = not signed in
}

type SortCol = "platform" | "status" | "expires"
type SortState = { col: SortCol; dir: "asc" | "desc" }

// statusRank orders rows so the ones needing attention (expired) surface first
// on an ascending status sort: expired < signed-out < active.
function statusRank(a: PlatformLoginView | null): number {
  if (!a) return 1
  return a.healthy ? 2 : 0
}

function formatExpiry(a: PlatformLoginView | null): string {
  if (!a) return "—"
  if (!a.healthy) return "Expired"
  if (!a.expiresHint) return "—"
  const sec = a.expiresHint - Math.floor(Date.now() / 1000)
  if (sec <= 0) return "Expiring"
  const d = Math.floor(sec / 86400)
  if (d >= 1) return `in ${d}d`
  const h = Math.floor(sec / 3600)
  if (h >= 1) return `in ${h}h`
  return `in ${Math.max(1, Math.floor(sec / 60))}m`
}

function SortHeader({
  label,
  col,
  sort,
  setSort,
  className,
}: {
  label: string
  col: SortCol
  sort: SortState
  setSort: (s: SortState) => void
  className?: string
}) {
  const active = sort.col === col
  return (
    <button
      type="button"
      className={cn(
        "inline-flex items-center gap-1 hover:text-foreground",
        active ? "text-foreground" : "text-muted-foreground",
        className,
      )}
      onClick={() =>
        setSort({
          col,
          dir: active && sort.dir === "asc" ? "desc" : "asc",
        })
      }
    >
      {label}
      {active ? (
        sort.dir === "asc" ? (
          <ChevronUp className="h-3.5 w-3.5" />
        ) : (
          <ChevronDown className="h-3.5 w-3.5" />
        )
      ) : (
        <ChevronsUpDown className="h-3.5 w-3.5 opacity-40" />
      )}
    </button>
  )
}

function StatusCell({ account }: { account: PlatformLoginView | null }) {
  const { color, label } = !account
    ? { color: "bg-slate-500", label: "Not signed in" }
    : account.healthy
      ? { color: "bg-green-500", label: "Active" }
      : { color: "bg-red-500", label: "Expired" }
  return (
    <span className="flex items-center gap-2">
      <span className={cn("inline-flex h-2.5 w-2.5 rounded-full", color)} />
      <span className="text-sm">{label}</span>
    </span>
  )
}

export function Accounts() {
  const connection = appStore((s) => s.connection)
  const connected = connection.state === "connected"
  const accounts = appStore((s) => s.accounts)
  const platforms = appStore((s) => s.platforms)

  const [sort, setSort] = useState<SortState>({ col: "platform", dir: "asc" })
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [pastePlatform, setPastePlatform] = useState<string | null>(null)

  const client = getDaemonClient()
  const canWebviewLogin = client.capabilities.webviewLogin

  // One row per login-capable platform, merged with its signed-in status.
  const rows = useMemo<Row[]>(() => {
    const byPlatform = new Map(accounts.map((a) => [a.platform, a]))
    const out = platforms
      .filter((p) => p.login)
      .map<Row>((p) => ({
        platform: p.name,
        displayName: platformDisplayName(p.name),
        account: byPlatform.get(p.name) ?? null,
      }))
    out.sort((a, b) => {
      let cmp = 0
      if (sort.col === "platform") cmp = a.displayName.localeCompare(b.displayName)
      else if (sort.col === "status")
        cmp = statusRank(a.account) - statusRank(b.account)
      else cmp = (a.account?.expiresHint ?? 0) - (b.account?.expiresHint ?? 0)
      if (cmp === 0) cmp = a.displayName.localeCompare(b.displayName)
      return sort.dir === "asc" ? cmp : -cmp
    })
    return out
  }, [accounts, platforms, sort])

  const run = async (platform: string, fn: () => Promise<unknown>) => {
    setBusy(platform)
    setError(null)
    try {
      await fn()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  const signIn = (platform: string) =>
    run(platform, () => client.platformLogin(platform))
  const signOut = (platform: string) =>
    run(platform, () => client.signOutPlatform(platform))

  return (
    <>
      <ConnectionBanner />
      <div className="mx-auto my-4 flex w-full max-w-table flex-col gap-4 px-4">
        <div className="flex items-start gap-2">
          <ShieldCheck className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" />
          <p className="text-sm text-muted-foreground">
            Sign in to streaming platforms to record private, 18+ live streams, 
            and high-quality live streams. Some platforms require a login to 
            monitor and record (all streamers are restricted until you sign in). 
            Your logins stay on this device and are never uploaded to vodlabs.
          </p>
        </div>

        {!canWebviewLogin && (
          <div className="flex items-start gap-2 rounded-md border border-border bg-muted/40 p-3">
            <Info className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" />
            <div className="text-sm text-muted-foreground">
              <p>
                This recorder runs without the desktop app (in Docker, say), so
                logins are added by pasting a cookie header from a browser where
                you are signed in. They are not renewed for you — re-paste one
                when it expires. Its status stays unknown until then.
              </p>
              <p className="mt-1">
                Age-restricted TikTok rooms also need the desktop app, and are
                skipped here. Everything else records normally.
              </p>
            </div>
          </div>
        )}

        {error && <p className="text-xs text-destructive">{error}</p>}

        <Card>
          <CardContent className="p-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>
                    <SortHeader label="Platform" col="platform" sort={sort} setSort={setSort} />
                  </TableHead>
                  <TableHead>Account</TableHead>
                  <TableHead>
                    <SortHeader label="Status" col="status" sort={sort} setSort={setSort} />
                  </TableHead>
                  <TableHead>
                    <SortHeader label="Login expires" col="expires" sort={sort} setSort={setSort} />
                  </TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.length === 0 ? (
                  <TableRow>
                    <TableCell colSpan={5} className="text-center text-sm text-muted-foreground">
                      {connected
                        ? "No platforms support login yet."
                        : "Daemon not connected."}
                    </TableCell>
                  </TableRow>
                ) : (
                  rows.map((row) => {
                    const signedIn = row.account !== null
                    const rowBusy = busy === row.platform
                    return (
                      <TableRow key={row.platform}>
                        <TableCell className="font-medium">{row.displayName}</TableCell>
                        <TableCell className="text-sm text-muted-foreground">
                          {row.account?.account || (signedIn ? "signed in" : "—")}
                        </TableCell>
                        <TableCell>
                          <StatusCell account={row.account} />
                        </TableCell>
                        <TableCell className="text-sm tabular-nums">
                          {formatExpiry(row.account)}
                        </TableCell>
                        <TableCell className="text-right">
                          <div className="flex justify-end gap-2">
                            {!signedIn ? (
                              <>
                                {canWebviewLogin && (
                                  <Button
                                    size="sm"
                                    disabled={rowBusy || !connected}
                                    onClick={() => signIn(row.platform)}
                                  >
                                    {rowBusy ? "Opening…" : "Sign in"}
                                  </Button>
                                )}
                                <Button
                                  size="sm"
                                  variant={canWebviewLogin ? "outline" : "default"}
                                  disabled={rowBusy || !connected}
                                  onClick={() => setPastePlatform(row.platform)}
                                >
                                  {canWebviewLogin ? "Paste cookies" : "Add cookies"}
                                </Button>
                              </>
                            ) : (
                              <>
                                {row.account && !row.account.healthy && canWebviewLogin && (
                                  <Button
                                    size="sm"
                                    disabled={rowBusy || !connected}
                                    onClick={() => signIn(row.platform)}
                                  >
                                    {rowBusy ? "Opening…" : "Sign in again"}
                                  </Button>
                                )}
                                <Button
                                  size="sm"
                                  variant="outline"
                                  disabled={rowBusy || !connected}
                                  onClick={() => signOut(row.platform)}
                                >
                                  {rowBusy ? "…" : "Sign out"}
                                </Button>
                              </>
                            )}
                          </div>
                        </TableCell>
                      </TableRow>
                    )
                  })
                )}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      </div>

      <PasteCookiesDialog
        platform={pastePlatform}
        open={pastePlatform !== null}
        onOpenChange={(open) => {
          if (!open) setPastePlatform(null)
        }}
      />
    </>
  )
}

export default Accounts
