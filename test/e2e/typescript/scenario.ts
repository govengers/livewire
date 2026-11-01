// The generated TypeScript client under bun against two engines on one database (D24): REST on
// engine A, the socket on engine B. The Go test generates ./api from the suite's registry, copies
// this file next to it and runs it with the engines' URLs in the environment; any failure
// throws, which exits non-zero.

import { calls, createNote, getNotes, live, saveFilter } from './api/api'
import { EndedError, createLivePair, type LiveConnection, type LivePair, type ListResponse } from './api/runtime'

const engineA = requireEnvironment('LIVEWIRE_ENGINE_A')
const engineB = requireEnvironment('LIVEWIRE_ENGINE_B')
const keys: Record<string, string> = { alice: 'alice-key', bob: 'bob-key' }

interface NoteLike {
  id: string
  title: string
}

/** Who the "session" acts as; a re-login changes it between two pairs (D66). */
let session = 'alice'

/** A WebSocket that sends the user's API key with its upgrade request (bun's headers option). */
function socketOf(key: string) {
  return class extends WebSocket {
    constructor(url: string) {
      super(url, { headers: { Authorization: `Bearer ${key}` } } as unknown as string[])
    }
  }
}

function pairOf(): LivePair {
  const key = keys[session] ?? ''
  return createLivePair({
    identity: () => `apikey:${session}`,
    client: { baseURL: `${engineA}/api`, headers: () => ({ Authorization: `Bearer ${key}` }) },
    connection: { url: `${engineB.replace(/^http/, 'ws')}/api/socket`, WebSocket: socketOf(key), reconnectDelay: 50 },
  })
}

/** A live list of notes on the connection, with the latest state and how it ended. */
function watchNotes(connection: LiveConnection, parameters: { filterId?: string } = {}) {
  const watched = { items: undefined as NoteLike[] | undefined, error: undefined as unknown, ended: false }
  const subscription = connection.subscribe(live.notes, parameters, {
    next: (state) => { watched.items = (state.data as ListResponse<NoteLike>).items },
    error: (error) => { watched.error = error },
    end: () => { watched.ended = true },
  })
  return { watched, subscription }
}

async function eventually(what: string, condition: () => boolean): Promise<void> {
  const deadline = Date.now() + 10_000
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting until ${what}`)
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
}

async function main(): Promise<void> {
  let pair = pairOf()

  // A REST create on engine A reaches a subscription on engine B through LISTEN.
  const { watched } = watchNotes(pair.connection)
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
  // D54 on the subscribe path: a fourth filter evicts the second, which a subscribe still names.
  const second = await saveFilter(pair.client, { resource: 'notes', filter: { search: 'pear' } })
  await saveFilter(pair.client, { resource: 'notes', filter: { search: 'quince' } })
  await saveFilter(pair.client, { resource: 'notes', filter: { search: 'figs' } })
  const filtered = watchNotes(pair.connection, { filterId: second.id }).watched
  await eventually('the evicted filter was subscribed inline', () => filtered.items?.length === 1 || filtered.error !== undefined)
  if (filtered.error !== undefined) throw new Error(`the subscribe with an evicted filterId failed: ${String(filtered.error)}`)

  // A re-login: the old pair ends before the "session" changes, the new one serves bob only.
  const old = pair
  old.end()
  session = 'bob'
  pair = pairOf()
  await eventually('the old subscription was told it ended', () => watched.ended)
  const refused = await getNotes(old.client).then(() => undefined, (error: unknown) => error)
  if (!(refused instanceof EndedError)) throw new Error(`the ended pair still answered: ${String(refused)}`)
  const bobs = watchNotes(pair.connection).watched
  await eventually('bob\'s snapshot arrived', () => bobs.items !== undefined)
  if (bobs.items?.length !== 0) throw new Error(`bob reads alice's notes: ${JSON.stringify(bobs.items)}`)
  await createNote(pair.client, { title: 'bob\'s' })
  await eventually('bob\'s own note arrived', () => bobs.items?.length === 1)
  if (watched.items !== undefined && watched.items.length > 2) throw new Error('the ended subscription took bob\'s note')
  pair.end()
  console.log('ok')
}

function requireEnvironment(name: string): string {
  const value = process.env[name]
  if (value === undefined || value === '') throw new Error(`${name} is not set`)
  return value
}

await main()
