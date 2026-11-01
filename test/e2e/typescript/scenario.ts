// The generated TypeScript client under bun against two engines on one database (D24): REST on
// engine A, the socket on engine B, both authenticated by the session cookie of a real password
// login; the engines share the session backend, as two replicas do. The Go test generates ./api
// from the suite's registry, copies this file next to it and runs it with the engines' URLs and
// the password in the environment; any failure throws, which exits non-zero.

import { calls, createNote, getNotes, live, saveFilter } from './api/api'
import { EndedError, createLivePair, type LiveConnection, type LivePair, type ListResponse } from './api/runtime'

const engineA = requireEnvironment('LIVEWIRE_ENGINE_A')
const engineB = requireEnvironment('LIVEWIRE_ENGINE_B')
const password = requireEnvironment('LIVEWIRE_PASSWORD')

interface NoteLike {
  id: string
  title: string
}

/** A session: its cookie, which bun's fetch keeps no jar for, and whom the server says it is. */
interface Session {
  cookie: string
  identity: string
}

/** Logs in with the password route on an engine and answers the session it started. */
async function login(engine: string, identifier: string): Promise<Session> {
  const response = await fetch(`${engine}/auth/password/login`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ identifier, password }),
  })
  if (!response.ok) throw new Error(`the login of ${identifier} answered ${response.status}`)
  const cookie = response.headers.getSetCookie().map((setCookie) => setCookie.split(';')[0]).join('; ')
  const identity = await response.json() as { provider: string; subject: string }
  return { cookie, identity: `${identity.provider}:${identity.subject}` }
}

/** Logs a session out on an engine. */
async function logout(engine: string, session: Session): Promise<void> {
  const response = await fetch(`${engine}/auth/password/logout`, { method: 'POST', headers: { Cookie: session.cookie } })
  if (!response.ok) throw new Error(`the logout answered ${response.status}`)
}

/** A WebSocket that sends the session cookie with its upgrade request (bun's headers option). */
function socketOf(cookie: string) {
  return class extends WebSocket {
    constructor(url: string) {
      super(url, { headers: { Cookie: cookie, Origin: engineB } } as unknown as string[]) // a cookie socket needs an allowed Origin (D9)
    }
  }
}

/** The pair of a session: REST on engine A, the socket on engine B. */
function pairOf(session: Session): LivePair {
  return createLivePair({
    identity: () => session.identity,
    client: { baseURL: `${engineA}/api`, headers: () => ({ Cookie: session.cookie }) },
    connection: { url: `${engineB.replace(/^http/, 'ws')}/api/socket`, WebSocket: socketOf(session.cookie), reconnectDelay: 50 },
  })
}

/** A live list of notes on the connection: the latest state, how it ended, and what came after. */
function watchNotes(connection: LiveConnection, parameters: { filterId?: string } = {}) {
  const watched = { items: undefined as NoteLike[] | undefined, error: undefined as unknown, ended: false, afterEnd: 0 }
  connection.subscribe(live.notes, parameters, {
    next: (state) => {
      if (watched.ended) watched.afterEnd++
      watched.items = (state.data as ListResponse<NoteLike>).items
    },
    error: (error) => { watched.error = error },
    end: () => { watched.ended = true },
  })
  return watched
}

async function eventually(what: string, condition: () => boolean): Promise<void> {
  const deadline = Date.now() + 10_000
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting until ${what}`)
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
}

async function main(): Promise<void> {
  const alice = await login(engineA, 'alice')
  let pair = pairOf(alice)

  // A REST create on engine A reaches a subscription on engine B through LISTEN.
  const watched = watchNotes(pair.connection)
  await eventually('the first snapshot arrived', () => watched.items?.length === 0)
  await createNote(pair.client, { title: 'apples' })
  await eventually('engine B pushed the note engine A wrote', () => watched.items?.length === 1)

  // A socket call with its optimistic effect (D36) settles into the list.
  const created = await pair.connection.call(calls.createNote, {}, { title: 'pears' })
  await eventually('the socket call reached the list', () => watched.items?.some((item) => item.id === created.id) === true)

  // D54 on the REST path: the store keeps two unnamed filters per owner, the third evicts the first.
  const first = await saveFilter(pair.client, { resource: 'notes', filter: { search: 'apples' } })
  await saveFilter(pair.client, { resource: 'notes', filter: { search: 'pears' } })
  await saveFilter(pair.client, { resource: 'notes', filter: { search: 'plums' } })
  const recovered = await getNotes(pair.client, { filterId: first.id })
  if (recovered.items.length !== 1 || recovered.items[0]?.title !== 'apples') {
    throw new Error(`the evicted filter was not sent inline: ${JSON.stringify(recovered)}`)
  }
  // D54 on the subscribe path: three more filters evict the one a subscribe still names.
  const second = await saveFilter(pair.client, { resource: 'notes', filter: { search: 'pear' } })
  await saveFilter(pair.client, { resource: 'notes', filter: { search: 'quince' } })
  await saveFilter(pair.client, { resource: 'notes', filter: { search: 'figs' } })
  const filtered = watchNotes(pair.connection, { filterId: second.id })
  await eventually('the evicted filter was subscribed inline', () => filtered.items?.length === 1 || filtered.error !== undefined)
  if (filtered.error !== undefined) throw new Error(`the subscribe with an evicted filterId failed: ${String(filtered.error)}`)

  // A re-login (D66): the pair ends before the request that changes the cookie, then alice logs
  // out on engine B and bob logs in there; the next pair serves bob's session only.
  const old = pair
  old.end()
  await eventually('the old subscription was told it ended', () => watched.ended)
  await logout(engineB, alice)
  const stale = await fetch(`${engineA}/api/notes`, { headers: { Cookie: alice.cookie } })
  if (stale.status !== 401) throw new Error(`alice's cookie still reads on engine A after her logout on B: ${stale.status}`)
  const bob = await login(engineB, 'bob')
  if (bob.identity === alice.identity || bob.cookie === alice.cookie) throw new Error('the re-login kept alice\'s session')
  pair = pairOf(bob)
  const refused = await getNotes(old.client).then(() => undefined, (error: unknown) => error)
  if (!(refused instanceof EndedError)) throw new Error(`the ended pair still answered: ${String(refused)}`)
  const bobs = watchNotes(pair.connection)
  await eventually('bob\'s snapshot arrived', () => bobs.items !== undefined)
  if (bobs.items?.length !== 0) throw new Error(`bob reads alice's notes: ${JSON.stringify(bobs.items)}`)
  const bobsNote = await createNote(pair.client, { title: 'bob\'s' })
  await eventually('bob\'s own note arrived', () => bobs.items?.length === 1)
  if (bobs.items?.[0]?.id !== bobsNote.id) throw new Error(`bob reads another note: ${JSON.stringify(bobs.items)}`)
  if (watched.afterEnd !== 0 || watched.items?.length !== 2) {
    throw new Error(`the ended subscription received ${watched.afterEnd} states after its end: ${JSON.stringify(watched.items)}`)
  }
  pair.end()
  console.log('ok')
}

function requireEnvironment(name: string): string {
  const value = process.env[name]
  if (value === undefined || value === '') throw new Error(`${name} is not set`)
  return value
}

await main()
