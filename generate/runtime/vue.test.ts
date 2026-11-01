import { describe, expect, test } from 'bun:test'
import { createApp, effectScope, nextTick, reactive, ref, shallowRef } from 'vue'
import { FakeClock, fakeSockets, settle } from './fakes'
import { LiveConnection, createLivePair, liveRead, mutation, type ListResponse, type LivePair, type PresenceItem } from './runtime'
import { livePairKey, livePlugin, useLive, useLiveConnection, useLiveList, useLivePair, useLivePresence, useLiveStatus, type SnapshotStorage } from './vue'

interface Part {
  id: string
  name: string
  price?: number
}

interface PartsQuery {
  filter?: { search?: string }
  sort?: readonly string[]
  limit?: number
  offset?: number
  after?: string
  board?: string
}

const parts = liveRead<PartsQuery, ListResponse<Part>>({ target: 'parts', path: '/parts', pathParameters: [], list: true })
const dashboard = liveRead<{ board: string }, { title: string }>({ target: 'dashboard', path: '/dashboard/{board}', pathParameters: ['board'], list: false })
const labels = liveRead<Record<string, never>, ListResponse<string>>({ target: 'labels', path: '/labels', pathParameters: [], list: true })
const presence = liveRead<{ key: string }, ListResponse<PresenceItem>>({ target: 'parts.presence', path: '/parts/presence', pathParameters: [], list: true })
const deletePart = mutation<{ id: string }, undefined, void>({
  name: 'deletePart', method: 'DELETE', path: '/parts/{id}', pathParameters: ['id'], body: false, content: false, effect: { kind: 'removes', target: 'parts' },
})

/** A Storage over a Map. */
class MemoryStorage implements SnapshotStorage {
  readonly values = new Map<string, string>()
  get length(): number {
    return this.values.size
  }
  key(index: number): string | null {
    return [...this.values.keys()][index] ?? null
  }
  getItem(key: string): string | null {
    return this.values.get(key) ?? null
  }
  setItem(key: string, value: string): void {
    this.values.set(key, value)
  }
  removeItem(key: string): void {
    this.values.delete(key)
  }
}

function connect(identity: string | null = 'password:alice') {
  const fakes = fakeSockets()
  const user = ref(identity ?? '')
  const clock = new FakeClock()
  const options = { url: 'wss://example.com/socket', WebSocket: fakes.Socket, reconnectDelay: 1, clock }
  const connection = new LiveConnection(identity === null ? options : { ...options, identity: () => user.value })
  return { connection, user, clock, ...fakes }
}

function list(items: Part[], extra: Partial<ListResponse<Part>> = {}): ListResponse<Part> {
  return { limit: 2, offset: 0, total: items.length, items, ...extra }
}

const subscribes = (frames: { op: string }[]) => frames.filter((frame) => frame.op === 'subscribe').length

describe('useLive', () => {
  test('subscribes, follows the pushes, resubscribes on a parameter change and ends with its scope', async () => {
    const { connection, current } = connect()
    const board = ref('a')
    const scope = effectScope()
    const live = scope.run(() => useLive(dashboard, () => ({ board: board.value }), { connection }))!
    expect([live.loading.value, live.stale.value]).toEqual([true, true])
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { title: 'A' } })
    expect([live.data.value?.title, live.stale.value, live.loading.value]).toEqual(['A', false, false])
    board.value = 'b'
    await nextTick()
    expect(current().last('unsubscribe').target).toBe('s1')
    expect(current().last('subscribe').params).toEqual({ board: 'b' })
    expect([live.data.value?.title, live.stale.value]).toEqual(['A', true])
    current().answer('subscribe', { subscription: 's2' })
    current().receive({ sub: 's2', seq: 1, kind: 'error', payload: { code: 'forbidden', message: 'no' } })
    expect([live.error.value?.message, live.loading.value]).toEqual(['no', false])
    live.retry()
    expect(live.error.value).toBeUndefined()
    current().answer('subscribe', { subscription: 's3' })
    scope.stop()
    expect(current().last('unsubscribe').target).toBe('s3')
  })

  test('a read without parameters needs none, and a closed socket makes the data stale', () => {
    const { connection, current } = connect()
    const live = effectScope().run(() => useLive(labels, undefined, { connection }))!
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: { limit: 1, offset: 0, total: 1, items: ['x'] } })
    expect(live.stale.value).toBe(false)
    current().end(1006)
    expect(live.stale.value).toBe(true)
  })

  test('the snapshot is kept per identity and an identity change clears every snapshot (D21)', () => {
    const storage = new MemoryStorage()
    const first = connect('password:alice')
    const alice = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection: first.connection, cache: true, storage }))!
    first.current().open()
    first.current().answer('subscribe', { subscription: 's1' })
    first.current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { title: 'secret of alice' } })
    expect(alice.data.value?.title).toBe('secret of alice')
    expect([...storage.values.keys()].some((key) => key.includes('password:alice'))).toBe(true)

    const again = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection: connect('password:alice').connection, cache: true, storage }))!
    expect([again.data.value?.title, again.stale.value]).toEqual(['secret of alice', true])

    const bob = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection: connect('password:bob').connection, cache: true, storage }))!
    expect(bob.data.value).toBeUndefined()
    expect([...storage.values.keys()]).toEqual(['livewire:identity'])
    expect(storage.getItem('livewire:identity')).toBe('password:bob')

    storage.setItem('livewire:password:bob:dashboard:[["board","z"]]', '{broken')
    const broken = effectScope().run(() => useLive(dashboard, { board: 'z' }, { connection: connect('password:bob').connection, cache: true, storage }))!
    expect(broken.data.value).toBeUndefined()
  })

  test('without an identity nothing is cached, and a cache of somebody before is cleared (D21)', () => {
    const storage = new MemoryStorage()
    storage.setItem('livewire:identity', 'password:alice')
    storage.setItem('livewire:password:alice:dashboard:[["board","a"]]', '{"title":"secret of alice"}')
    const nobody = connect(null)
    const live = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection: nobody.connection, cache: true, storage }))!
    expect(live.data.value).toBeUndefined()
    nobody.current().open()
    nobody.current().answer('subscribe', { subscription: 's1' })
    nobody.current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { title: 'public' } })
    expect([...storage.values.entries()]).toEqual([['livewire:identity', '']])
  })

  test('a new connection (a new pair) drops the old data and pending changes and subscribes on it (D66)', async () => {
    const storage = new MemoryStorage()
    const alice = connect('password:alice')
    const current = shallowRef(alice.connection)
    const live = effectScope().run(() => useLive(parts, {}, { connection: current, cache: true, storage }))!
    alice.current().open()
    alice.current().answer('subscribe', { subscription: 's1' })
    alice.current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'secret of alice' }]) })
    void alice.connection.call(deletePart, { id: 'a' }, undefined).catch(() => {})
    expect(live.pending.value).toEqual(['a'])
    const bob = connect('password:bob')
    alice.connection.end()
    current.value = bob.connection
    await nextTick()
    expect([live.data.value, live.pending.value, live.stale.value]).toEqual([undefined, [], true])
    expect([...storage.values.keys()]).toEqual(['livewire:identity'])
    bob.current().open()
    expect(bob.current().sent.map((frame) => frame.op)).toEqual(['subscribe'])
    bob.current().answer('subscribe', { subscription: 's2' })
    bob.current().receive({ sub: 's2', seq: 1, kind: 'snapshot', payload: list([{ id: 'b', name: 'of bob' }]) })
    expect(live.data.value?.items[0]?.name).toBe('of bob')
    expect([...storage.values.keys()].some((key) => key.includes('password:bob'))).toBe(true)
  })

  test('a full storage keeps the old snapshot', () => {
    const storage = new MemoryStorage()
    storage.setItem = (key: string, value: string) => {
      if (key !== 'livewire:identity') throw new Error(`quota exceeded for ${value.length} bytes`)
      storage.values.set(key, value)
    }
    const { connection, current } = connect()
    const live = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection, cache: true, storage }))!
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { title: 'A' } })
    expect(live.data.value?.title).toBe('A')
  })

  test('the pending marker of an optimistic removal reaches the view (D36)', async () => {
    const { connection, current } = connect()
    const live = effectScope().run(() => useLive(parts, {}, { connection }))!
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }]) })
    const call = connection.call(deletePart, { id: 'a' }, undefined)
    expect([live.pending.value, live.data.value?.items]).toEqual([['a'], []])
    current().answer('call', { status: 204 })
    await call
    expect(live.pending.value).toEqual([])
  })
})

describe('the connection', () => {
  test('is the provided pair\'s, the given one, or missing', async () => {
    const first = createLivePair({ identity: () => 'password:alice', client: { baseURL: 'https://x' }, connection: { url: 'wss://x', WebSocket: fakeSockets().Socket } })
    const pair = shallowRef<LivePair>(first)
    const app = createApp({})
    app.use(livePlugin(pair))
    expect(app._context.provides[livePairKey as symbol]).toBe(pair)
    const followed = app.runWithContext(() => useLiveConnection())
    expect(followed.value).toBe(first.connection)
    const second = createLivePair({ identity: () => 'password:bob', client: { baseURL: 'https://x' }, connection: { url: 'wss://x', WebSocket: fakeSockets().Socket } })
    pair.value = second
    expect(followed.value).toBe(second.connection)
    expect(app.runWithContext(() => useLivePair())).toBe(pair)
    const { connection } = connect()
    expect(useLiveConnection(connection).value).toBe(connection)
    expect(() => useLiveConnection()).toThrow('no pair provided')
  })

  test('useLiveStatus follows it, and a new connection', async () => {
    const { connection, current } = connect()
    const followed = shallowRef(connection)
    const scope = effectScope()
    const status = scope.run(() => useLiveStatus(followed))!
    expect(status.value).toBe('idle')
    connection.subscribe(parts, {}, { next: () => {}, error: () => {} })
    expect(status.value).toBe('connecting')
    current().open()
    expect(status.value).toBe('open')
    connection.end()
    expect(status.value).toBe('ended')
    followed.value = connect().connection
    await nextTick()
    expect(status.value).toBe('idle')
    scope.stop()
  })
})

describe('useLiveList', () => {
  test('offset mode: a page moves the offset, a filter or sort change goes back to page 1', async () => {
    const { connection, current } = connect()
    const live = effectScope().run(() => useLiveList(parts, { connection, limit: 2, sort: ['-name'] }))!
    current().open()
    expect(current().last('subscribe').params).toEqual({ sort: '-name', limit: '2' })
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }], { total: 5 }) })
    expect([live.items.value.length, live.total.value, live.pages.value, live.stale.value, live.loading.value, live.hasMore.value]).toEqual([2, 5, 3, false, false, false])
    live.page.value = 3
    await nextTick()
    expect(current().last('subscribe').params).toEqual({ sort: '-name', limit: '2', offset: '4' })
    live.filter.value = { search: 'bolt' }
    await nextTick()
    await nextTick()
    expect(live.page.value).toBe(1)
    expect(current().last('subscribe').params).toEqual({ filter: '{"search":"bolt"}', sort: '-name', limit: '2' })
    live.sort.value = []
    await nextTick()
    expect(current().last('subscribe').params).toEqual({ filter: '{"search":"bolt"}', limit: '2' })
    live.loadMore()
    expect(live.pending.value).toEqual([])
    const typeOnly = effectScope()
    typeOnly.run(() => {
      // @ts-expect-error a list keeps no snapshot: cache is no option of it
      useLiveList(parts, { connection, cache: true })
    })
    typeOnly.stop()
  })

  test('an error of a page is the list\'s, and retry subscribes again', () => {
    const { connection, current } = connect()
    const live = effectScope().run(() => useLiveList(parts, { connection, parameters: { board: 'x' } }))!
    current().open()
    expect(current().last('subscribe').params).toEqual({ board: 'x' })
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'error', payload: { code: 'invalid_filter', message: 'bad' } })
    expect([live.error.value instanceof Error, live.loading.value, live.pages.value]).toEqual([true, false, undefined])
    live.retry()
    expect(live.error.value).toBeUndefined()
    expect(subscribes(current().sent)).toBe(2)
  })

  test('the end of the connection drops the pages, and a new connection starts the list over (D66)', async () => {
    const alice = connect()
    const followed = shallowRef(alice.connection)
    const live = effectScope().run(() => useLiveList(parts, { connection: followed }))!
    alice.current().open()
    alice.current().answer('subscribe', { subscription: 's1' })
    alice.current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }]) })
    expect(live.items.value).toHaveLength(1)
    alice.connection.end()
    expect([live.items.value, live.total.value]).toEqual([[], undefined])
    const bob = connect('password:bob')
    followed.value = bob.connection
    await nextTick()
    bob.current().open()
    expect(bob.current().last('subscribe').target).toBe('parts')
  })

  /** A cursor list with two loaded tiles: t1 holds a, b (next after-b), t2 c, d (next after-d). */
  function twoTiles() {
    const connected = connect()
    const scope = effectScope()
    const live = scope.run(() => useLiveList(parts, { connection: connected.connection, mode: 'cursor', limit: 2 }))!
    connected.current().open()
    connected.current().answer('subscribe', { subscription: 't1' })
    connected.current().receive({ sub: 't1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A', price: 10 }, { id: 'b', name: 'B', price: 20 }], { next: 'after-b', total: 4 }) })
    live.loadMore()
    connected.current().answer('subscribe', { subscription: 't2' })
    connected.current().receive({ sub: 't2', seq: 1, kind: 'snapshot', payload: list([{ id: 'c', name: 'C' }, { id: 'd', name: 'D' }], { next: 'after-d' }) })
    const ids = () => live.items.value.map((item) => item.id)
    return { ...connected, scope, live, ids }
  }

  test('cursor mode tiles live pages and a tile whose rows changed moves the tile below (D38, D64)', async () => {
    const { live, current, ids, scope } = twoTiles()
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-b' })
    expect([ids(), live.hasMore.value]).toEqual([['a', 'b', 'c', 'd'], true])

    current().receive({ sub: 't1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'n', name: 'N' }], removes: ['b'], order: ['n', 'a'], total: 5 } })
    expect(current().last('query')).toMatchObject({ target: 'parts', params: { limit: '2' } })
    current().answer('query', { payload: list([{ id: 'n', name: 'N' }, { id: 'a', name: 'A' }], { next: 'after-a' }) })
    await settle()
    expect(current().last('unsubscribe').target).toBe('t2')
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-a' })
    current().answer('subscribe', { subscription: 't3' })
    current().receive({ sub: 't3', seq: 1, kind: 'snapshot', payload: list([{ id: 'b', name: 'B' }, { id: 'c', name: 'C' }], { next: 'after-c' }) })
    expect([ids(), live.total.value]).toEqual([['n', 'a', 'b', 'c'], 5])

    // The first page loses a row and has no next any more: the tiles below go.
    current().receive({ sub: 't1', seq: 3, kind: 'diff', payload: { upserts: [], removes: ['a'], order: ['n'], total: 1 } })
    current().answer('query', { payload: list([{ id: 'n', name: 'N' }]) })
    await settle()
    expect([ids(), live.hasMore.value]).toEqual([['n'], false])
    live.loadMore()
    expect(subscribes(current().sent)).toBe(3)
    scope.stop()
  })

  test('two reads of one tile\'s next answered out of order keep the newer one (D64)', async () => {
    const { current } = twoTiles()
    current().receive({ sub: 't1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'x', name: 'X' }], removes: ['b'], order: ['x', 'a'] } })
    const older = current().last('query')
    current().receive({ sub: 't1', seq: 3, kind: 'diff', payload: { upserts: [{ id: 'n', name: 'N' }], removes: ['a'], order: ['x', 'n'] } })
    const newer = current().last('query')
    current().receive({ id: newer.id, ok: true, status: 200, payload: list([{ id: 'x', name: 'X' }, { id: 'n', name: 'N' }], { next: 'after-n' }) })
    current().receive({ id: older.id, ok: true, status: 200, payload: list([{ id: 'x', name: 'X' }, { id: 'a', name: 'A' }], { next: 'after-a' }) })
    await settle()
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-n' })
  })

  test('a read of a tile\'s next that answers after the view ended subscribes nothing (D64)', async () => {
    const { current, scope } = twoTiles()
    current().receive({ sub: 't1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'n', name: 'N' }], removes: ['b'], order: ['n', 'a'] } })
    scope.stop()
    current().answer('query', { payload: list([{ id: 'n', name: 'N' }, { id: 'a', name: 'A' }], { next: 'after-a' }) })
    await settle()
    expect(subscribes(current().sent)).toBe(2)
  })

  test('a moved sort value of the boundary row and a snapshot\'s new next move the tile below (D64)', async () => {
    const { current } = twoTiles()
    current().receive({ sub: 't1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'b', name: 'B', price: 15 }], removes: [] } })
    current().answer('query', { payload: list([{ id: 'a', name: 'A', price: 10 }, { id: 'b', name: 'B', price: 15 }], { next: 'after-b-15' }) })
    await settle()
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-b-15' })
    current().answer('subscribe', { subscription: 't3' })
    current().receive({ sub: 't3', seq: 1, kind: 'snapshot', payload: list([{ id: 'c', name: 'C' }], { next: 'after-c' }) })

    current().receive({ sub: 't1', seq: 3, kind: 'snapshot', payload: list([{ id: 'a', name: 'A', price: 10 }, { id: 'b', name: 'B', price: 15 }], { next: 'after-b-resync' }) })
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-b-resync' })
  })

  test('a refused read of a tile\'s next is the list\'s error, and retry reads again (D64)', async () => {
    const { live, current } = twoTiles()
    current().receive({ sub: 't1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'n', name: 'N' }], removes: ['b'], order: ['n', 'a'] } })
    const refused = current().last('query')
    current().receive({ id: refused.id, ok: false, status: 500, error: { code: 'internal', message: 'x' } })
    await settle()
    expect((live.error.value as { code?: string } | undefined)?.code).toBe('internal')
    live.retry()
    expect(live.error.value).toBeUndefined()
    expect(current().last('query').id).not.toBe(refused.id)
    current().answer('query', { payload: list([{ id: 'n', name: 'N' }, { id: 'a', name: 'A' }], { next: 'after-a' }) })
    await settle()
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-a' })
  })

  test('cursor mode: a filter change starts over at the first page', async () => {
    const { live, current } = twoTiles()
    live.filter.value = { search: 'x' }
    await nextTick()
    expect(current().sent.filter((frame) => frame.op === 'unsubscribe').map((frame) => frame.target)).toEqual(['t1', 't2'])
    expect(current().last('subscribe').params).toEqual({ filter: '{"search":"x"}', limit: '2' })
  })
})

describe('useLivePresence', () => {
  test('joins with the state, joins again on a change, moves records and leaves with its scope (D37)', async () => {
    const { connection, current } = connect()
    const key = ref('p1')
    const cursor = ref(1)
    const scope = effectScope()
    const live = scope.run(() => useLivePresence(presence, () => ({ key: key.value }), () => ({ cursor: cursor.value }), { connection }))!
    current().open()
    expect(current().last('join')).toMatchObject({ target: 'parts', params: { key: 'p1' }, body: { cursor: 1 } })
    current().answer('join')
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: { limit: 1000, offset: 0, total: 1, items: [{ id: 'socket', display: 'Bob', state: { cursor: 4 } }] } })
    expect(live.presences.value[0]?.display).toBe('Bob')
    await settle()
    cursor.value = 2
    await nextTick()
    expect(current().last('join').body).toEqual({ cursor: 2 })
    current().answer('join')
    await settle()
    key.value = 'p2'
    await nextTick()
    expect(current().last('leave')).toMatchObject({ target: 'parts', params: { key: 'p1' } })
    const refused = current().last('join')
    expect(refused.params).toEqual({ key: 'p2' })
    current().receive({ id: refused.id, ok: false, status: 404, error: { code: 'not_found', message: 'gone' } })
    await settle()
    expect(live.error.value?.message).toBe('gone')
    scope.stop()
    expect(current().last('leave').params).toEqual({ key: 'p2' })
  })

  test('a mutated parameters object leaves the record it joined (D37)', async () => {
    const { connection, current } = connect()
    const parameters = reactive({ key: 'a' })
    effectScope().run(() => useLivePresence(presence, parameters, { cursor: 1 }, { connection }))
    current().open()
    current().answer('join')
    await settle()
    parameters.key = 'b'
    await nextTick()
    expect(current().last('leave').params).toEqual({ key: 'a' })
    expect(current().last('join').params).toEqual({ key: 'b' })
  })

  test('two views on one record share it; the socket leaves with the last (D37)', async () => {
    const { connection, current } = connect()
    const first = effectScope()
    first.run(() => useLivePresence(presence, { key: 'k' }, { cursor: 1 }, { connection }))
    const second = effectScope()
    second.run(() => useLivePresence(presence, { key: 'k' }, { cursor: 2 }, { connection }))
    current().open()
    current().answer('join')
    await settle()
    first.stop()
    expect(current().sent.some((frame) => frame.op === 'leave')).toBe(false)
    second.stop()
    expect(current().last('leave').params).toEqual({ key: 'k' })
  })
})

describe('one identity per pair in the views (D66)', () => {
  test('a non-reactive switch through checkIdentity ends the view\'s data at once; the new pair\'s data is never shown to the old identity (D66)', async () => {
    const storage = new MemoryStorage()
    let identity = 'password:admin'
    const pairOf = () => createLivePair({ identity: () => identity, client: { baseURL: 'https://x' }, connection: { url: 'wss://x', WebSocket: fakes.Socket, clock: new FakeClock() } })
    const fakes = fakeSockets()
    const pair = shallowRef<LivePair>(pairOf())
    const view = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection: () => pair.value.connection, cache: true, storage }))!
    fakes.current().open()
    fakes.current().answer('subscribe', { subscription: 's1' })
    fakes.current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { title: 'guarded column of the admin' } })
    identity = 'password:reader'
    pair.value.connection.checkIdentity()
    expect([view.data.value, view.stale.value, pair.value.client.ended]).toEqual([undefined, true, true])

    pair.value = pairOf()
    await nextTick()
    fakes.current().open()
    fakes.current().answer('subscribe', { subscription: 's2' })
    fakes.current().receive({ sub: 's2', seq: 1, kind: 'replace', payload: { title: 'secret of the reader' } })
    expect(view.data.value?.title).toBe('secret of the reader')

    identity = 'password:admin'
    const remount = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection: connect('password:admin').connection, cache: true, storage }))!
    expect(remount.data.value).toBeUndefined()
  })

  test('a storage that fails is a cache miss: the view still subscribes', () => {
    const storage = new MemoryStorage()
    storage.setItem = () => {
      throw new Error('private mode')
    }
    storage.getItem = () => {
      throw new Error('private mode')
    }
    const { connection, current } = connect()
    const live = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection, cache: true, storage }))!
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { title: 'A' } })
    expect(live.data.value?.title).toBe('A')
  })
})

describe('useLiveList limits and cursors', () => {
  test('offset paging uses the limit the server applied (D14)', async () => {
    const { connection, current } = connect()
    const live = effectScope().run(() => useLiveList(parts, { connection, limit: 500 }))!
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }], { limit: 100, total: 250 }) })
    expect(live.pages.value).toBe(3)
    live.page.value = 2
    await nextTick()
    expect(current().last('subscribe').params).toEqual({ limit: '500', offset: '100' })
    live.limit.value = 50
    await nextTick()
    expect(live.page.value).toBe(1)
  })

  test('a read of a tile\'s next started before a snapshot cannot override the snapshot\'s next (D64)', async () => {
    const connected = connect()
    const live = effectScope().run(() => useLiveList(parts, { connection: connected.connection, mode: 'cursor', limit: 2 }))!
    const { current } = connected
    current().open()
    current().answer('subscribe', { subscription: 't1' })
    current().receive({ sub: 't1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }], { next: 'after-b' }) })
    live.loadMore()
    current().answer('subscribe', { subscription: 't2' })
    current().receive({ sub: 't1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'n', name: 'N' }], removes: ['b'], order: ['n', 'a'] } })
    const refresh = current().last('query')
    current().receive({ sub: 't1', seq: 3, kind: 'snapshot', payload: list([{ id: 'n', name: 'N' }, { id: 'x', name: 'X' }], { next: 'after-x' }) })
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-x' })
    current().receive({ id: refresh.id, ok: true, status: 200, payload: list([{ id: 'n', name: 'N' }, { id: 'a', name: 'A' }], { next: 'after-a' }) })
    await settle()
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-x' })
  })

  test('a snapshot with an unchanged next still outdates a read on its way, and clears a refused one (D64)', async () => {
    const connected = connect()
    const live = effectScope().run(() => useLiveList(parts, { connection: connected.connection, mode: 'cursor', limit: 2 }))!
    const { current } = connected
    current().open()
    current().answer('subscribe', { subscription: 't1' })
    current().receive({ sub: 't1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }], { next: 'after-b' }) })
    live.loadMore()
    current().answer('subscribe', { subscription: 't2' })
    const subscribes = current().sent.filter((frame) => frame.op === 'subscribe').length
    current().receive({ sub: 't1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'c', name: 'C' }], removes: ['a'], order: ['c', 'b'] } })
    const outdated = current().last('query')
    current().receive({ sub: 't1', seq: 3, kind: 'snapshot', payload: list([{ id: 'c', name: 'C' }, { id: 'b', name: 'B' }], { next: 'after-b' }) })
    current().receive({ id: outdated.id, ok: true, status: 200, payload: list([{ id: 'c', name: 'C' }, { id: 'y', name: 'Y' }], { next: 'after-y' }) })
    await settle()
    expect(current().sent.filter((frame) => frame.op === 'subscribe')).toHaveLength(subscribes)

    current().receive({ sub: 't1', seq: 4, kind: 'diff', payload: { upserts: [{ id: 'd', name: 'D' }], removes: ['c'], order: ['d', 'b'] } })
    current().receive({ id: current().last('query').id, ok: false, status: 500, error: { code: 'internal', message: 'x' } })
    await settle()
    expect(live.error.value).toBeDefined()
    current().receive({ sub: 't1', seq: 5, kind: 'snapshot', payload: list([{ id: 'd', name: 'D' }, { id: 'b', name: 'B' }], { next: 'after-b' }) })
    expect(live.error.value).toBeUndefined()
  })
})

describe('the end in the views (D66)', () => {
  test('a cached view mounted on an ended connection reads no snapshot, and the end removes that identity\'s snapshots', () => {
    const storage = new MemoryStorage()
    const root = connect('password:root')
    effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection: root.connection, cache: true, storage }))
    root.current().open()
    root.current().answer('subscribe', { subscription: 's1' })
    root.current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { title: 'secret of root' } })
    expect([...storage.values.keys()].some((key) => key.startsWith('livewire:password:root:'))).toBe(true)
    root.connection.end()
    expect([...storage.values.keys()].some((key) => key.startsWith('livewire:password:root:'))).toBe(false)

    storage.setItem('livewire:password:root:dashboard:[["board","a"]]', '{"title":"secret of root"}')
    const gap = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection: root.connection, cache: true, storage }))!
    expect(gap.data.value).toBeUndefined()
    expect(storage.getItem('livewire:password:root:dashboard:[["board","a"]]')).toBeNull()
  })

  test('a pair replaced without ending the old one drops the old data before the new push', async () => {
    const alice = connect('password:alice')
    const followed = shallowRef(alice.connection)
    const live = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection: followed }))!
    alice.current().open()
    alice.current().answer('subscribe', { subscription: 's1' })
    alice.current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { title: 'of alice' } })
    followed.value = connect('password:bob').connection
    await nextTick()
    expect(live.data.value).toBeUndefined()
  })

  test('a browser that blocks storage (the getter throws) is a cache miss', () => {
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
    Object.defineProperty(globalThis, 'localStorage', { configurable: true, get: () => { throw new Error('SecurityError') } })
    try {
      const { connection, current } = connect()
      const live = effectScope().run(() => useLive(dashboard, { board: 'a' }, { connection, cache: true }))!
      current().open()
      current().answer('subscribe', { subscription: 's1' })
      current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { title: 'A' } })
      expect(live.data.value?.title).toBe('A')
    } finally {
      if (descriptor === undefined) Reflect.deleteProperty(globalThis, 'localStorage')
      else Object.defineProperty(globalThis, 'localStorage', descriptor)
    }
  })
})
