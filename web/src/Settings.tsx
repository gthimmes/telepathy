import { useState } from 'react'
import { ago, api, useFetch, type Session } from './api'

const ROLES: Record<string, string> = {
  ingest: 'Send telemetry and change events. Give this to services, collectors and CI.',
  read: 'Query everything, change nothing. For scripts, reports and AI agents.',
  admin: 'Full access, including dashboards, monitors, channels and tokens.',
}

export function Login({ onDone }: { onDone: () => void }) {
  const [pw, setPw] = useState('')
  const [err, setErr] = useState('')
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    try { await api.login(pw); onDone() } catch (e) { setErr(String((e as Error).message)) }
  }
  return (
    <div className="card login">
      <h1>Sign in to Telepathy</h1>
      <form onSubmit={submit}>
        {err && <div className="error">{err}</div>}
        <input type="password" autoFocus required autoComplete="current-password" aria-label="Admin password" placeholder="Admin password" value={pw} onChange={e => setPw(e.target.value)} />
        <button className="btn primary">Sign in</button>
      </form>
    </div>
  )
}

export default function Settings({ session, onSignOut }: { session: Session; onSignOut: () => void }) {
  const [n, setN] = useState(0)
  const { data, error } = useFetch(() => api.tokens(), [n])
  const [name, setName] = useState('')
  const [role, setRole] = useState('ingest')
  const [secret, setSecret] = useState<{ name: string; value: string }>()
  const [err, setErr] = useState('')
  const origin = window.location.origin
  const create = async (e: React.FormEvent) => {
    e.preventDefault()
    try { const r = await api.createToken(name, role); setSecret({ name, value: r.secret }); setName(''); setErr(''); setN(x => x + 1) }
    catch (e) { setErr(String((e as Error).message)) }
  }
  return (
    <>
      <div className="page-head">
        <h1>Settings</h1><span className="spacer" />
        {session.authRequired && <button className="btn" onClick={async () => { await api.logout(); onSignOut() }}>Sign out</button>}
      </div>
      {!session.authRequired && (
        <div className="card">
          <h2>Authentication is off</h2>
          <p>Anyone who can reach this server can read and change everything. Start it with an admin password to require sign-in and API tokens:</p>
          <pre>TELEPATHY_ADMIN_PASSWORD=… telepathy serve</pre>
        </div>
      )}
      <div className="card">
        <h2>API tokens</h2>
        <p className="muted">Tokens authenticate programs. Send one as <code>Authorization: Bearer &lt;token&gt;</code> — over OTLP/HTTP, OTLP/gRPC or the API.
          {!session.authRequired && ' They are only enforced once authentication is on.'}</p>
        {(error || err) && <div className="error">{error || err}</div>}
        {secret && (
          <div className="secret">
            <b>Token “{secret.name}” created.</b> Copy it now — it is stored hashed and cannot be shown again.
            <div><code>{secret.value}</code></div>
          </div>
        )}
        {data?.tokens.map(t => (
          <div key={t.id} className="list-row">
            <span className="kind">{t.role}</span>
            <div className="grow"><b>{t.name}</b> <span className="muted mono">{t.prefix}…</span></div>
            <span className="muted">{t.lastUsedMs ? `used ${ago(t.lastUsedMs)}` : 'never used'}</span>
            <button className="btn danger" onClick={async () => { await api.deleteToken(t.id); setN(x => x + 1) }}>Revoke</button>
          </div>
        ))}
        <form className="inline-form" onSubmit={create}>
          <label className="grow">Name<input required value={name} onChange={e => setName(e.target.value)} placeholder="checkout service, CI pipeline, …" /></label>
          <label>Role<select value={role} onChange={e => setRole(e.target.value)}>{Object.keys(ROLES).map(r => <option key={r}>{r}</option>)}</select></label>
          <button className="btn primary">Create token</button>
        </form>
        <p className="muted">{ROLES[role]}</p>
      </div>
      <div className="card">
        <h2>Sending telemetry</h2>
        <pre>{`# OTLP/HTTP
OTEL_EXPORTER_OTLP_ENDPOINT=${origin}
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
# or OTLP/gRPC on port 4317
OTEL_EXPORTER_OTLP_ENDPOINT=${window.location.protocol}//${window.location.hostname}:4317
OTEL_EXPORTER_OTLP_PROTOCOL=grpc

OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20<ingest token>
OTEL_RESOURCE_ATTRIBUTES=telepathy.location=store-042`}</pre>
      </div>
      <div className="card">
        <h2>AI agents (MCP)</h2>
        <p className="muted">Telepathy is a Model Context Protocol server: an agent can read health, alerts and SLOs, run Explain, and query traces, logs and metrics. Every tool is read-only{session.authRequired ? '; use a token with the read role' : ''}.</p>
        <pre>{`# Claude Code
claude mcp add --transport http telepathy ${origin}/api/v1/mcp${session.authRequired ? ` \\
  --header "Authorization: Bearer <read token>"` : ''}

# Any MCP client (Streamable HTTP)
${origin}/api/v1/mcp`}</pre>
      </div>
    </>
  )
}
