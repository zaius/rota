// Line-format templates for proxy lists — the TypeScript mirror of
// core/internal/lineformat, used for live validation and preview in the
// dashboard. The Go side stays authoritative; keep the two in sync.
//
// A format is a template of field names separated by literal characters, with
// optional parts in brackets, e.g. "host:port:user:pass" or
// "[protocol://][user[:pass]@]host:port".

import { Protocol, PROTOCOLS } from "./types"

// The standard URL line format — scheme and auth optional. New sources and
// imports default to it.
export const FORMAT_URL = "[protocol://][user[:pass]@]host:port"

// Built-in formats offered in the picker; these are never saved to history.
export const FORMAT_PRESETS: { value: string; label: string; hint: string }[] = [
  { value: FORMAT_URL, label: "URL", hint: "protocol://user:pass@host:port — scheme and auth optional" },
  { value: "host:port:user:pass", label: "host:port:user:pass", hint: "e.g. Webshare downloads" },
  { value: "user:pass:host:port", label: "user:pass:host:port", hint: "credentials first" },
  { value: "host:port@user:pass", label: "host:port@user:pass", hint: "reversed @ notation" },
]

export const isPresetFormat = (format: string) =>
  FORMAT_PRESETS.some(p => p.value === format.trim())

export interface ParsedProxyLine {
  address: string // host:port
  protocol?: Protocol // undefined means "use the default"
  username?: string
  password?: string
}

export interface CompiledLineFormat {
  // User-facing template error, or null when the format is usable.
  error: string | null
  // Canonical fields the template captures (empty for auto).
  fields: string[]
  // Parses one line; null for blanks, comments and non-matching lines.
  parse: (line: string) => ParsedProxyLine | null
}

type Canonical = "host" | "port" | "user" | "pass" | "protocol"

const FIELD_ALIASES: Record<string, Canonical> = {
  host: "host", ip: "host",
  port: "port",
  user: "user", username: "user", login: "user",
  pass: "pass", password: "pass", pwd: "pass",
  protocol: "protocol", scheme: "protocol", proto: "protocol",
}

type Token =
  | { kind: "field"; name: Canonical }
  | { kind: "skip" }
  | { kind: "literal"; text: string }
  | { kind: "open" }
  | { kind: "close" }

const isAlpha = (c: string) => /[a-zA-Z]/.test(c)

function tokenize(s: string): Token[] | string {
  const toks: Token[] = []
  let i = 0
  while (i < s.length) {
    const c = s[i]
    if (c === "[") { toks.push({ kind: "open" }); i++ }
    else if (c === "]") { toks.push({ kind: "close" }); i++ }
    else if (c === "*") { toks.push({ kind: "skip" }); i++ }
    else if (isAlpha(c)) {
      let j = i
      while (j < s.length && isAlpha(s[j])) j++
      const word = s.slice(i, j)
      const canon = FIELD_ALIASES[word.toLowerCase()]
      if (!canon) {
        return `unknown field "${word}" — valid fields are host, port, user, pass, protocol (use * to skip a field)`
      }
      toks.push({ kind: "field", name: canon })
      i = j
    } else {
      let j = i
      while (j < s.length && !isAlpha(s[j]) && !"[]*".includes(s[j])) j++
      toks.push({ kind: "literal", text: s.slice(i, j) })
      i = j
    }
  }
  return toks
}

function validateTokens(toks: Token[]): string | null {
  let depth = 0
  let prevWasField = false
  const seen = new Set<string>()
  for (const t of toks) {
    if (t.kind === "open") depth++
    else if (t.kind === "close") {
      if (--depth < 0) return "unmatched ']'"
    } else if (t.kind === "literal") prevWasField = false
    else {
      if (prevWasField) return "two fields need a separator between them (e.g. host:port, not hostport)"
      prevWasField = true
      if (t.kind === "field") {
        if (seen.has(t.name)) return `field "${t.name}" appears twice`
        seen.add(t.name)
        if ((t.name === "host" || t.name === "port") && depth > 0) {
          return `${t.name} is required and cannot be inside optional [...] brackets`
        }
      }
    }
  }
  if (depth !== 0) return "unmatched '['"
  if (!seen.has("host") || !seen.has("port")) return "format must contain both host and port"
  return null
}

const escapeRegex = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")

// nextLiteralChar finds the first character of the next literal after token i,
// looking through bracket markers (but not past another field).
function nextLiteralChar(toks: Token[], i: number): string | null {
  for (let j = i + 1; j < toks.length; j++) {
    const t = toks[j]
    if (t.kind === "literal") return t.text[0]
    if (t.kind === "field" || t.kind === "skip") return null
  }
  return null
}

// fieldPattern picks the regex for a field: the port is digits, the protocol
// is scheme-shaped, and every other field matches up to the next literal
// separator. When that separator is "@" the field is greedy instead, so a
// password containing "@" splits at the last one like a URL parser does.
function fieldPattern(toks: Token[], i: number, field: Canonical | null): string {
  if (field === "port") return "\\d{1,5}"
  if (field === "protocol") return "[a-zA-Z][a-zA-Z0-9+.-]*"
  const next = nextLiteralChar(toks, i)
  if (next === null || next === "@") return ".+"
  return `[^${escapeRegex(next)}]+`
}

const KNOWN_PROTOCOLS = new Set<string>(PROTOCOLS)

// stripScheme removes a leading scheme:// and returns it when it is a known
// proxy protocol. Unknown schemes are stripped but yield no protocol.
function stripScheme(line: string): { rest: string; protocol?: Protocol } {
  const idx = line.indexOf("://")
  if (idx === -1) return { rest: line }
  const scheme = line.slice(0, idx).toLowerCase()
  return {
    rest: line.slice(idx + 3),
    protocol: KNOWN_PROTOCOLS.has(scheme) ? (scheme as Protocol) : undefined,
  }
}

const validPort = (port: string) => {
  const n = Number(port)
  return Number.isInteger(n) && n >= 1 && n <= 65535
}

// %-decode credentials when the encoding is valid, like url.Parse does.
function decodeField(s: string | undefined): string | undefined {
  if (!s) return undefined
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

// parseBare accepts host:port with a valid numeric port and nothing else.
function parseBare(line: string, protocol?: Protocol): ParsedProxyLine | null {
  const stripped = stripScheme(line)
  const proto = stripped.protocol ?? protocol
  const colon = stripped.rest.indexOf(":")
  if (colon === -1) return null
  const host = stripped.rest.slice(0, colon)
  const port = stripped.rest.slice(colon + 1)
  if (!host || /[@ \t]/.test(host) || port.includes(":") || !validPort(port)) return null
  return { address: stripped.rest, protocol: proto }
}

export function compileLineFormat(format: string): CompiledLineFormat {
  const trimmed = format.trim()
  const fail = (error: string): CompiledLineFormat =>
    ({ error, fields: [], parse: () => null })

  if (trimmed.length > 200) return fail("format is too long (max 200 characters)")

  const toks = tokenize(trimmed)
  if (typeof toks === "string") return fail(toks)
  const invalid = validateTokens(toks)
  if (invalid) return fail(invalid)

  let pattern = "^"
  const fields: string[] = []
  let hasProtocol = false
  toks.forEach((t, i) => {
    if (t.kind === "open") pattern += "(?:"
    else if (t.kind === "close") pattern += ")?"
    else if (t.kind === "literal") pattern += escapeRegex(t.text)
    else if (t.kind === "skip") pattern += `(?:${fieldPattern(toks, i, null)})`
    else {
      if (t.name === "protocol") hasProtocol = true
      fields.push(t.name)
      pattern += `(?<${t.name}>${fieldPattern(toks, i, t.name)})`
    }
  })
  pattern += "$"

  let re: RegExp
  try {
    re = new RegExp(pattern)
  } catch {
    return fail("format does not compile")
  }

  return {
    error: null,
    fields,
    parse: line => {
      line = line.trim()
      if (!line || line.startsWith("#")) return null

      let attempt = line
      let defaultProto: Protocol | undefined
      // The template reads the protocol from an optional scheme:// prefix
      // when it has no protocol field of its own.
      if (!hasProtocol) {
        const stripped = stripScheme(line)
        attempt = stripped.rest
        defaultProto = stripped.protocol
      }

      const m = attempt.match(re)
      if (m?.groups) {
        const { host, port, user, pass, protocol } = m.groups
        if (host && validPort(port)) {
          let proto = defaultProto
          if (protocol) {
            const scheme = protocol.toLowerCase()
            if (!KNOWN_PROTOCOLS.has(scheme)) return parseBare(attempt, defaultProto)
            proto = scheme as Protocol
          }
          return {
            address: `${host}:${port}`,
            protocol: proto,
            username: decodeField(user),
            password: decodeField(pass),
          }
        }
      }
      // Bare host:port always parses, so mixed lists don't break.
      return parseBare(attempt, defaultProto)
    },
  }
}
