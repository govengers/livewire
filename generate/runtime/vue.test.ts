import { describe, expect, test } from 'bun:test'
import { createApp, effectScope, nextTick, ref } from 'vue'
import { fakeSockets, settle } from './fakes'
import { LiveConnection, liveRead, mutation, type ListResponse, type PresenceItem } from './runtime'
import { livePlugin, liveConnectionKey, useLive, useLiveConnection, useLiveList, useLivePresence, useLiveStatus, type SnapshotStorage } from './vue'

interface Part {
  id: string
  name: string
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

function connect(identity = 'password:alice') {
  const fakes = fakeSockets()
  const user = ref(identity)
  const connection = new LiveConnection({ url: 'wss://example.com/socket', WebSocket: fakes.Socket, identity: () => user.value, reconnectDelay: 1 })
  return { connection, user, ...fakes }
}

function list(items: Part[], extra: Partial<ListResponse<Part>> = {}): ListResponse<Part> {
  return { limit: 2, offset: 0, total: items.length, items, ...extra }
}

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
    expect([live.error.value?.code, live.loading.value]).toEqual(['forbidden', false])
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
  test('is provided by the plugin, given by options, or missing', () => {
    const { connection } = connect()
    const app = createApp({})
    app.use(livePlugin(connection))
    expect(app.runWithContext(() => useLiveConnection())).toBe(connection)
    expect(app._context.provides[liveConnectionKey as symbol]).toBe(connection)
    expect(useLiveConnection(connection)).toBe(connection)
    expect(() => useLiveConnection()).toThrow('no LiveConnection')
  })

  test('useLiveStatus follows it', () => {
    const { connection, current } = connect()
    const scope = effectScope()
    const status = scope.run(() => useLiveStatus(connection))!
    expect(status.value).toBe('idle')
    connection.subscribe(parts, {}, { next: () => {}, error: () => {} })
    expect(status.value).toBe('connecting')
    current().open()
    expect(status.value).toBe('open')
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
  })

  test('an error of a page is the list\'s, and retry subscribes again', () => {
    const { connection, current } = connect()
    const live = effectScope().run(() => useLiveList(parts, { connection, parameters: { board: 'x' } }))!
    current().open()
    expect(current().last('subscribe').params).toEqual({ board: 'x' })
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'error', payload: { code: 'invalid_filter', message: 'bad' } })
    expect([live.error.value?.code, live.loading.value, live.pages.value]).toEqual(['invalid_filter', false, undefined])
    live.retry()
    expect(live.error.value).toBeUndefined()
    expect(current().sent.filter((frame) => frame.op === 'subscribe')).toHaveLength(2)
  })

  test('cursor mode tiles live pages and a tile whose rows changed moves the tile below (D38)', async () => {
    const { connection, current } = connect()
    const scope = effectScope()
    const live = scope.run(() => useLiveList(parts, { connection, mode: 'cursor', limit: 2 }))!
    current().open()
    current().answer('subscribe', { subscription: 't1' })
    current().receive({ sub: 't1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }], { next: 'after-b', total: 4 }) })
    expect(live.hasMore.value).toBe(true)
    live.loadMore()
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-b' })
    current().answer('subscribe', { subscription: 't2' })
    current().receive({ sub: 't2', seq: 1, kind: 'snapshot', payload: list([{ id: 'c', name: 'C' }, { id: 'd', name: 'D' }]) })
    expect(live.items.value.map((item) => item.id)).toEqual(['a', 'b', 'c', 'd'])
    expect(live.hasMore.value).toBe(false)
    live.loadMore()
    expect(current().sent.filter((frame) => frame.op === 'subscribe')).toHaveLength(2)

    // A new row on the first page pushes b out of it: the first page's next moves.
    current().receive({ sub: 't1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'n', name: 'N' }], removes: ['b'], order: ['n', 'a'], total: 5 } })
    expect(current().last('query')).toMatchObject({ target: 'parts', params: { limit: '2' } })
    current().answer('query', { payload: list([{ id: 'n', name: 'N' }, { id: 'a', name: 'A' }], { next: 'after-a' }) })
    await settle()
    expect(current().last('unsubscribe').target).toBe('t2')
    expect(current().last('subscribe').params).toEqual({ limit: '2', after: 'after-a' })
    current().answer('subscribe', { subscription: 't3' })
    current().receive({ sub: 't3', seq: 1, kind: 'snapshot', payload: list([{ id: 'b', name: 'B' }, { id: 'c', name: 'C' }], { next: 'after-c' }) })
    expect(live.items.value.map((item) => item.id)).toEqual(['n', 'a', 'b', 'c'])
    expect(live.total.value).toBe(5)

    // The first page loses a row and has no next any more: the tiles below go.
    current().receive({ sub: 't1', seq: 3, kind: 'diff', payload: { upserts: [], removes: ['a'], order: ['n'], total: 1 } })
    current().answer('query', { payload: list([{ id: 'n', name: 'N' }]) })
    await settle()
    expect(live.items.value.map((item) => item.id)).toEqual(['n'])
    expect(live.hasMore.value).toBe(false)
    scope.stop()
  })

  test('cursor mode: a filter change starts over at the first page', async () => {
    const { connection, current } = connect()
    const live = effectScope().run(() => useLiveList(parts, { connection, mode: 'cursor' }))!
    current().open()
    current().answer('subscribe', { subscription: 't1' })
    current().receive({ sub: 't1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }], { next: 'n1' }) })
    live.loadMore()
    current().answer('subscribe', { subscription: 't2' })
    live.filter.value = { search: 'x' }
    await nextTick()
    expect(current().sent.filter((frame) => frame.op === 'unsubscribe').map((frame) => frame.target)).toEqual(['t1', 't2'])
    expect(current().last('subscribe').params).toEqual({ filter: '{"search":"x"}' })
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
    const subscribe = current().last('subscribe')
    current().receive({ id: subscribe.id, ok: true, status: 200, subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: { limit: 1000, offset: 0, total: 1, items: [{ id: 'socket', display: 'Bob', state: { cursor: 4 } }] } })
    expect(live.presences.value[0]?.display).toBe('Bob')
    cursor.value = 2
    await nextTick()
    expect(current().last('join').body).toEqual({ cursor: 2 })
    current().answer('join')
    key.value = 'p2'
    await nextTick()
    expect(current().last('leave')).toMatchObject({ target: 'parts', params: { key: 'p1' } })
    const refused = current().last('join')
    expect(refused.params).toEqual({ key: 'p2' })
    current().receive({ id: refused.id, ok: false, status: 404, error: { code: 'not_found', message: 'gone' } })
    await settle()
    expect(live.error.value?.code).toBe('not_found')
    scope.stop()
    expect(current().last('leave').params).toEqual({ key: 'p2' })
  })
})
