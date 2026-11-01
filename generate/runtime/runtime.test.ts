import { describe, expect, test } from 'bun:test'
import { fakeFetch, fakeSockets, json, settle, waitFor } from './fakes'
import {
  ApiError,
  Client,
  LiveConnection,
  NetworkError,
  applyListChange,
  batchCall,
  exportTarget,
  liveRead,
  mutation,
  read,
  routeOf,
  upload,
  uuidv4,
  uuidv7,
  wireParameters,
  type ListResponse,
  type LiveState,
  type PresenceItem,
} from './runtime'

interface Part {
  id: string
  name: string
}

interface PartsQuery {
  filter?: unknown
  filterId?: string
  sort?: readonly string[]
  limit?: number
}

const parts = liveRead<PartsQuery, ListResponse<Part>>({ target: 'parts', path: '/parts', pathParameters: [], list: true })
const partItem = liveRead<{ id: string }, Part>({ target: 'parts.item', path: '/parts/{id}', pathParameters: ['id'], list: false })
const presence = liveRead<{ key: string }, ListResponse<PresenceItem>>({ target: 'parts.presence', path: '/parts/presence', pathParameters: [], list: true })
const notes = read<Record<string, never>, ListResponse<Part>>({ target: 'notes', path: '/notes', pathParameters: [], list: true })
const createPart = mutation<Record<string, never>, { id?: string; name: string }, Part>({
  name: 'createPart', method: 'POST', path: '/parts', pathParameters: [], body: true, content: true,
  effect: { kind: 'upserts', target: 'parts' }, clientID: 7,
})
const deletePart = mutation<{ id: string }, undefined, void>({
  name: 'deletePart', method: 'DELETE', path: '/parts/{id}', pathParameters: ['id'], body: false, content: false,
  effect: { kind: 'removes', target: 'parts' },
})
const attach = upload<{ id: string }, { size: number }, 'image/png'>({ name: 'attach', path: '/parts/{id}/files', pathParameters: ['id'], contentTypes: ['image/png'] })
const partsExport = exportTarget<{ sort?: readonly string[] }>({ target: 'parts', path: '/parts/export' })

const uuidPattern = (version: number) => new RegExp(`^[0-9a-f]{8}-[0-9a-f]{4}-${version}[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

function list(items: Part[], total = items.length): ListResponse<Part> {
  return { limit: 100, offset: 0, total, items }
}

function connect(options: Partial<ConstructorParameters<typeof LiveConnection>[0]> = {}) {
  const fakes = fakeSockets()
  const connection = new LiveConnection({ url: 'wss://example.com/socket', WebSocket: fakes.Socket, reconnectDelay: 1, random: () => 0.5, ...options })
  return { connection, ...fakes }
}

/** Collects what a subscription delivers. */
function listener<Data>() {
  const states: LiveState<Data>[] = []
  const errors: ApiError[] = []
  return {
    states, errors,
    last: () => states.at(-1),
    next: (state: LiveState<Data>) => void states.push(state),
    error: (error: ApiError) => void errors.push(error),
  }
}

describe('wire helpers', () => {
  test('parameters are text, lists comma-separated, documents JSON', () => {
    expect(wireParameters({ name: 'a', limit: 5, open: false, tag: ['x', 'y'], filter: { search: 'b' }, gone: undefined, empty: null })).toEqual({
      name: 'a', limit: '5', open: 'false', tag: 'x,y', filter: '{"search":"b"}',
    })
  })

  test('a route fills its placeholders and puts the rest in the query', () => {
    expect(routeOf('/parts/{id}/files', ['id'], { id: 'a/b', sort: '-name' })).toBe('/parts/a%2Fb/files?sort=-name')
    expect(routeOf('/parts', [], {})).toBe('/parts')
    expect(() => routeOf('/parts/{id}', ['id'], {})).toThrow('the path parameter id is missing')
  })

  test('UUIDs carry their version and v7 its millisecond timestamp first', () => {
    const now = 0x0123456789ab
    const key = uuidv7(now)
    expect(key).toMatch(uuidPattern(7))
    expect(key.replace(/-/g, '').slice(0, 12)).toBe('0123456789ab')
    expect(uuidv4()).toMatch(uuidPattern(4))
    expect(uuidv7()).not.toBe(uuidv7())
  })

  test('a diff replaces, inserts and removes by id, re-sorts to its order and takes its total', () => {
    const before = list([{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }, { id: 'c', name: 'C' }])
    const after = applyListChange(before, { upserts: [{ id: 'b', name: 'B2' }, { id: 'd', name: 'D' }], removes: ['a'], order: ['d', 'c', 'b'], total: 7 })
    expect(after.items).toEqual([{ id: 'd', name: 'D' }, { id: 'c', name: 'C' }, { id: 'b', name: 'B2' }])
    expect(after.total).toBe(7)
    expect(before.items).toHaveLength(3)
    const inPlace = applyListChange(before, { upserts: [{ id: 'b', name: 'B3' }], removes: [] })
    expect(inPlace.items.map((item) => item.name)).toEqual(['A', 'B3', 'C'])
    expect(inPlace.total).toBe(3)
    const numeric = applyListChange({ limit: 1, offset: 0, total: 1, items: [{ id: 1 }] }, { upserts: [], removes: ['1'] })
    expect(numeric.items).toEqual([])
  })

  test('a batch call carries the parameters, the body and a client id', () => {
    const call = batchCall(createPart, {}, { name: 'a' })
    expect(call.target).toBe('createPart')
    expect(call.params).toBeUndefined()
    expect((call.body as { id: string }).id).toMatch(uuidPattern(7))
    expect(batchCall(deletePart, { id: 'p' }, undefined)).toEqual({ target: 'deletePart', params: { id: 'p' } })
  })

  test('an ApiError takes its code from the body, else from the status, and its status from the code', () => {
    const refusal = new ApiError(400, { code: 'invalid_filter', message: 'bad', fields: { 'where.field': 'unknown' } })
    expect([refusal.status, refusal.code, refusal.message, refusal.fields['where.field']]).toEqual([400, 'invalid_filter', 'bad', 'unknown'])
    expect(new ApiError(502, '<html>').code).toBe('internal')
    expect(new ApiError(401, { error: 'nope' }).code).toBe('unauthenticated')
    expect(new ApiError(0, { code: 'not_found', message: 'gone' }).status).toBe(404)
    expect(new ApiError(409, { code: 'something_else' }).code).toBe('conflict')
  })
})

describe('Client', () => {
  const base = 'https://example.com/api/v1'

  test('a read fills the route, sends the headers and answers the body', async () => {
    const { fetch, calls } = fakeFetch(json({ id: 'p', name: 'P' }))
    const client = new Client({ baseURL: base, fetch, headers: () => ({ Authorization: 'Bearer key' }) })
    expect(await client.read(partItem, { id: 'p 1' })).toEqual({ id: 'p', name: 'P' })
    expect(calls[0]?.url).toBe('https://example.com/api/v1/parts/p%201')
    expect(calls[0]?.init.method).toBe('GET')
    expect((calls[0]?.init.headers as Record<string, string>)['Authorization']).toBe('Bearer key')
    expect(calls[0]?.init.credentials).toBe('same-origin')
  })

  test('a call sends an Idempotency-Key, the body with a client id and answers nothing for 204', async () => {
    const { fetch, calls } = fakeFetch(json({ id: 'p', name: 'a' }, 201), new Response(null, { status: 204 }))
    const client = new Client({ baseURL: base, fetch })
    expect(await client.call(createPart, {}, { name: 'a' })).toEqual({ id: 'p', name: 'a' })
    const headers = calls[0]?.init.headers as Record<string, string>
    expect(headers['Idempotency-Key']).toMatch(uuidPattern(7))
    expect(headers['Content-Type']).toBe('application/json')
    const body = JSON.parse(String(calls[0]?.init.body)) as { id: string; name: string }
    expect(body.id).toMatch(uuidPattern(7))
    expect(await client.call(deletePart, { id: 'p' }, undefined, { idempotencyKey: 'key' })).toBeUndefined()
    expect(calls[1]?.url).toBe(`${base}/parts/p`)
    expect(calls[1]?.init.body).toBeUndefined()
    expect((calls[1]?.init.headers as Record<string, string>)['Idempotency-Key']).toBe('key')
  })

  test('a refusal is an ApiError, a 401 calls onUnauthorized, no answer is a NetworkError', async () => {
    const unauthorized: ApiError[] = []
    const { fetch } = fakeFetch(
      json({ code: 'forbidden_field', message: 'no', fields: { secret: 'guarded' } }, 403),
      json({ code: 'unauthenticated', message: 'login' }, 401),
      new Response('Bad Gateway', { status: 502 }),
      new TypeError('offline'),
    )
    const client = new Client({ baseURL: base, fetch, onUnauthorized: (error) => void unauthorized.push(error) })
    const forbidden = await client.read(notes, {}).catch((error: unknown) => error)
    expect(forbidden).toBeInstanceOf(ApiError)
    expect((forbidden as ApiError).fields).toEqual({ secret: 'guarded' })
    await expect(client.read(notes, {})).rejects.toThrow('login')
    expect(unauthorized).toHaveLength(1)
    const gateway = await client.read(notes, {}).catch((error: unknown) => error)
    expect([(gateway as ApiError).status, (gateway as ApiError).code, (gateway as ApiError).body]).toEqual([502, 'internal', 'Bad Gateway'])
    await expect(client.read(notes, {})).rejects.toBeInstanceOf(NetworkError)
  })

  test('a read whose unnamed filterId was evicted sends the filter inline once (D54)', async () => {
    const stored = { id: 'f1', resource: 'parts', filter: { search: 'bolt' }, createdAt: '', lastUsedAt: '' }
    const { fetch, calls } = fakeFetch(json(stored, 201), json({ code: 'not_found', message: 'gone' }, 404), json(list([])))
    const client = new Client({ baseURL: base, fetch })
    await client.saveFilter({ resource: 'parts', filter: { search: 'bolt' } })
    expect(await client.read(parts, { filterId: 'f1', limit: 5 })).toEqual(list([]))
    expect(calls[1]?.url).toBe(`${base}/parts?filterId=f1&limit=5`)
    const retried = new URL(String(calls[2]?.url))
    expect(retried.searchParams.get('filterId')).toBeNull()
    expect(retried.searchParams.get('filter')).toBe('{"search":"bolt"}')
    expect(retried.searchParams.get('limit')).toBe('5')

    const unknown = fakeFetch(json({ code: 'not_found', message: 'gone' }, 404))
    await expect(new Client({ baseURL: base, fetch: unknown.fetch }).read(parts, { filterId: 'other' })).rejects.toBeInstanceOf(ApiError)
  })

  test('uploads go raw with their content type or multipart with the part file', async () => {
    const { fetch, calls } = fakeFetch(json({ size: 1 }, 201), json({ size: 1 }, 201))
    const client = new Client({ baseURL: base, fetch })
    const file = new Blob(['x'], { type: 'image/png' })
    await client.upload(attach, { id: 'p' }, file)
    expect((calls[0]?.init.headers as Record<string, string>)['Content-Type']).toBe('image/png')
    expect(calls[0]?.init.body).toBe(file)
    await client.upload(attach, { id: 'p' }, file, { multipart: true, filename: 'a.png' })
    const form = calls[1]?.init.body as FormData
    expect((form.get('file') as File).name).toBe('a.png')
    expect((calls[1]?.init.headers as Record<string, string>)['Content-Type']).toBeUndefined()
  })

  test('the library routes: batch, stored filters, export', async () => {
    const { fetch, calls } = fakeFetch(json([{ status: 201, payload: {} }]), json(list([])), new Response(null, { status: 204 }), new Response('id\n1\n', { status: 200 }))
    const client = new Client({ baseURL: base, fetch })
    expect(await client.batch([batchCall(deletePart, { id: 'p' }, undefined)])).toEqual([{ status: 201, payload: {} }])
    expect(JSON.parse(String(calls[0]?.init.body))).toEqual([{ target: 'deletePart', params: { id: 'p' } }])
    await client.listFilters('parts.history')
    expect(calls[1]?.url).toBe(`${base}/filters?resource=parts.history`)
    await client.deleteFilter('f/1')
    expect([calls[2]?.url, calls[2]?.init.method]).toEqual([`${base}/filters/f%2F1`, 'DELETE'])
    expect(client.exportURL(partsExport, { sort: ['-name'] })).toBe(`${base}/parts/export?sort=-name`)
    const download = await client.download(partsExport, {})
    expect([await download.blob.text(), download.rows]).toEqual(['id\n1\n', null])
  })

  test('a download that is cut off, or ends without its trailer, fails', async () => {
    const broken = new Response('id\n')
    Object.defineProperty(broken, 'blob', { value: () => Promise.reject(new TypeError('terminated')) })
    const withoutTrailer = new Response('id\n')
    Object.defineProperty(withoutTrailer, 'trailer', { value: Promise.resolve(new Headers()) })
    const withTrailer = new Response('id\n')
    Object.defineProperty(withTrailer, 'trailer', { value: Promise.resolve(new Headers({ 'Livewire-Export-Rows': '1' })) })
    const { fetch } = fakeFetch(broken, withoutTrailer, withTrailer)
    const client = new Client({ baseURL: base, fetch })
    await expect(client.download(partsExport, {})).rejects.toThrow('cut off')
    await expect(client.download(partsExport, {})).rejects.toThrow('without its row count')
    expect((await client.download(partsExport, {})).rows).toBe(1)
  })
})

describe('LiveConnection', () => {
  test('requests are answered by id, out of order, and a refusal is an ApiError', async () => {
    const { connection, current } = connect()
    const first = connection.query(notes, {})
    const second = connection.query(partItem, { id: 'p' })
    current().open()
    const [firstFrame, secondFrame] = current().sent
    expect(firstFrame).toMatchObject({ op: 'query', target: 'notes' })
    expect(secondFrame).toMatchObject({ op: 'query', target: 'parts.item', params: { id: 'p' } })
    current().receive({ id: secondFrame?.id, ok: true, status: 200, payload: { id: 'p', name: 'P' } })
    current().receive({ id: firstFrame?.id, ok: false, status: 403, error: { code: 'forbidden', message: 'no' } })
    expect(await second).toEqual({ id: 'p', name: 'P' })
    const refusal = await first.catch((error: unknown) => error)
    expect([(refusal as ApiError).status, (refusal as ApiError).code]).toEqual([403, 'forbidden'])
    expect(connection.status).toBe('open')
  })

  test('a list subscription applies snapshot and diff pushes in sequence', () => {
    const { connection, current } = connect()
    const received = listener<ListResponse<Part>>()
    connection.subscribe(parts, { limit: 2 }, received)
    current().open()
    const subscribe = current().answer('subscribe', { subscription: 's1' })
    expect(subscribe).toMatchObject({ target: 'parts', params: { limit: '2' } })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }]) })
    current().receive({ sub: 's1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'b', name: 'B' }], removes: [], order: ['b', 'a'], total: 2 } })
    expect(received.last()?.data.items).toEqual([{ id: 'b', name: 'B' }, { id: 'a', name: 'A' }])
    expect(received.last()?.data.total).toBe(2)
    current().receive({ sub: 'other', seq: 1, kind: 'snapshot', payload: list([]) })
    expect(received.states).toHaveLength(2)
  })

  test('an object takes its replace pushes and an error push ends it until retry', () => {
    const { connection, current } = connect()
    const received = listener<Part>()
    const subscription = connection.subscribe(partItem, { id: 'p' }, received)
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'replace', payload: { id: 'p', name: 'P' } })
    current().receive({ sub: 's1', seq: 2, kind: 'replace', payload: { id: 'p', name: 'Q' } })
    expect(received.last()?.data.name).toBe('Q')
    current().receive({ sub: 's1', seq: 3, kind: 'error', payload: { code: 'not_found', message: 'gone' } })
    expect([received.errors[0]?.code, received.errors[0]?.status]).toEqual(['not_found', 404])
    current().receive({ sub: 's1', seq: 4, kind: 'replace', payload: { id: 'p', name: 'R' } })
    expect(received.last()?.data.name).toBe('Q')
    subscription.retry()
    expect(current().sent.filter((frame) => frame.op === 'subscribe')).toHaveLength(2)
  })

  test('a refused subscribe reports its error', () => {
    const { connection, current } = connect()
    const received = listener<Part>()
    connection.subscribe(partItem, { id: 'p' }, received)
    current().open()
    const frame = current().last('subscribe')
    current().receive({ id: frame.id, ok: false, status: 400, error: { code: 'invalid_request', message: 'bad', fields: { id: 'not a uuid' } } })
    expect(received.errors[0]?.fields).toEqual({ id: 'not a uuid' })
  })

  test('a lost push subscribes again for a fresh snapshot', () => {
    const { connection, current } = connect()
    const received = listener<ListResponse<Part>>()
    connection.subscribe(parts, {}, received)
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }]) })
    current().receive({ sub: 's1', seq: 3, kind: 'diff', payload: { upserts: [], removes: ['a'] } })
    expect(current().last('unsubscribe').target).toBe('s1')
    current().answer('subscribe', { subscription: 's2' })
    current().receive({ sub: 's2', seq: 1, kind: 'snapshot', payload: list([]) })
    expect(received.last()?.data.items).toEqual([])
  })

  test('unsubscribe tells the server, also when the subscribe is still on its way', () => {
    const { connection, current } = connect()
    const first = connection.subscribe(parts, {}, listener())
    const second = connection.subscribe(notes as never, {}, listener())
    current().open()
    current().answer('subscribe', { subscription: 's2' })
    const [firstSubscribe] = current().sent
    first.unsubscribe()
    current().receive({ id: firstSubscribe?.id, ok: true, status: 200, subscription: 's1' })
    expect(current().last('unsubscribe').target).toBe('s1')
    second.unsubscribe()
    expect(current().last('unsubscribe').target).toBe('s2')
  })

  test('a closed socket reconnects with backoff, restores subscriptions and presences, and fails what was in flight', async () => {
    const statuses: string[] = []
    const { connection, sockets, current } = connect()
    connection.onStatus((status) => void statuses.push(status))
    connection.subscribe(parts, { limit: 3 }, listener())
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    const joined = connection.join(presence, { key: 'k' }, { cursor: 1 })
    current().answer('join')
    await joined
    const query = connection.query(notes, {})
    current().end(1006)
    await expect(query).rejects.toBeInstanceOf(NetworkError)
    expect(connection.status).toBe('reconnecting')
    await waitFor(() => sockets.length === 2)
    current().open()
    expect(current().last('subscribe')).toMatchObject({ target: 'parts', params: { limit: '3' } })
    expect(current().last('join')).toMatchObject({ target: 'parts', params: { key: 'k' }, body: { cursor: 1 } })
    expect(statuses).toEqual(['idle', 'connecting', 'open', 'reconnecting', 'open'])
    await connection.leave(presence, { key: 'other' })
    const leaving = connection.leave(presence, { key: 'k' })
    current().answer('leave')
    await leaving
  })

  test('nothing to serve after a close leaves the socket idle', () => {
    const { connection, sockets, current } = connect()
    const subscription = connection.subscribe(parts, {}, listener())
    current().open()
    subscription.unsubscribe()
    current().end(1006)
    expect(connection.status).toBe('idle')
    expect(sockets).toHaveLength(1)
  })

  test('4401 means the identity is gone: no reconnect until reconnect()', async () => {
    let unauthorized = 0
    const { connection, sockets, current } = connect({ onUnauthorized: () => void unauthorized++ })
    connection.subscribe(parts, {}, listener())
    current().open()
    current().end(4401)
    expect([connection.status, unauthorized]).toEqual(['unauthorized', 1])
    await new Promise((resolve) => setTimeout(resolve, 20))
    expect(sockets).toHaveLength(1)
    await expect(connection.query(notes, {})).rejects.toThrow('unauthorized')
    connection.subscribe(parts, {}, listener())
    expect(sockets).toHaveLength(1)
    connection.reconnect()
    expect(sockets).toHaveLength(2)
    current().open()
    expect(current().sent.filter((frame) => frame.op === 'subscribe')).toHaveLength(2)
  })

  test('1001 reconnects at once with jitter, 1013 backs off', async () => {
    const { connection, sockets, current } = connect({ reconnectDelay: 40, random: () => 0 })
    connection.subscribe(parts, {}, listener())
    current().open()
    current().end(1001)
    await waitFor(() => sockets.length === 2)
    current().open()
    const before = Date.now()
    current().end(1013)
    await waitFor(() => sockets.length === 3)
    expect(Date.now() - before).toBeGreaterThanOrEqual(15)
  })

  test('pings go out, and a silent socket is closed and replaced', async () => {
    const { connection, sockets, current } = connect({ pingInterval: 5 })
    connection.subscribe(parts, {}, listener())
    current().open()
    await waitFor(() => current().sent.some((frame) => frame.op === 'ping'))
    current().answer('ping')
    await waitFor(() => sockets.length === 2)
    expect(sockets[0]?.closedWith).toBe(1005)
  })

  test('close() ends the socket for good until reconnect()', async () => {
    const { connection, sockets, current } = connect()
    connection.subscribe(parts, {}, listener())
    current().open()
    connection.close()
    expect([connection.status, sockets[0]?.closedWith]).toEqual(['closed', 1000])
    await expect(connection.query(notes, {})).rejects.toBeInstanceOf(NetworkError)
    connection.reconnect()
    expect(sockets).toHaveLength(2)
    new LiveConnection({ url: () => 'wss://x', WebSocket: fakeSockets().Socket }).close()
  })

  test('a removal shows at once, pending, and stays after the answer until the next push (D36)', async () => {
    const { connection, current } = connect()
    const received = listener<ListResponse<Part>>()
    connection.subscribe(parts, {}, received)
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }]) })
    const call = connection.call(deletePart, { id: 'a' }, undefined)
    expect(received.last()?.data.items.map((item) => item.id)).toEqual(['b'])
    expect(received.last()?.data.total).toBe(1)
    expect(received.last()?.pending).toEqual(['a'])
    current().answer('call', { status: 204 })
    await call
    expect(received.last()?.pending).toEqual([])
    expect(received.last()?.data.items.map((item) => item.id)).toEqual(['b'])
    current().receive({ sub: 's1', seq: 2, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }]) })
    expect(received.last()?.data.items.map((item) => item.id)).toEqual(['a', 'b'])
  })

  test('a refused removal rolls back', async () => {
    const { connection, current } = connect()
    const received = listener<ListResponse<Part>>()
    connection.subscribe(parts, {}, received)
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }]) })
    const call = connection.call(deletePart, { id: 'a' }, undefined)
    const frame = current().last('call')
    expect(frame).toMatchObject({ target: 'deletePart', params: { id: 'a' } })
    expect(frame.idempotencyKey).toMatch(uuidPattern(7))
    current().receive({ id: frame.id, ok: false, status: 409, error: { code: 'conflict', message: 'no' } })
    await expect(call).rejects.toBeInstanceOf(ApiError)
    expect(received.last()?.data.items.map((item) => item.id)).toEqual(['a'])
    expect(received.last()?.pending).toEqual([])
  })

  test('an upsert shows the optimistic item, then the answer, and the next push wins (D36)', async () => {
    const { connection, current } = connect()
    const received = listener<ListResponse<Part>>()
    connection.subscribe(parts, {}, received)
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }]) })
    const item = listener<Part>()
    connection.subscribe(partItem, { id: 'x' }, item) // not a list of the effect's target: untouched
    const call = connection.call(createPart, {}, { id: 'n', name: 'new' }, { optimistic: { id: 'n', name: 'new' } })
    expect(received.last()?.data.items).toEqual([{ id: 'a', name: 'A' }, { id: 'n', name: 'new' }])
    expect([received.last()?.pending, received.last()?.data.total]).toEqual([['n'], 2])
    expect(current().last('call').body).toEqual({ id: 'n', name: 'new' })
    current().answer('call', { status: 201, payload: { id: 'n', name: 'stored' } })
    expect(await call).toEqual({ id: 'n', name: 'stored' })
    expect(received.last()?.data.items).toEqual([{ id: 'a', name: 'A' }, { id: 'n', name: 'stored' }])
    expect(received.last()?.pending).toEqual([])
    current().receive({ sub: 's1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'a', name: 'A2' }], removes: [] } })
    expect(received.last()?.data.items).toEqual([{ id: 'a', name: 'A2' }])
    expect(item.states).toEqual([])
  })

  test('an upsert without an optimistic item applies the answer and replaces an item in place', async () => {
    const { connection, current } = connect()
    const received = listener<ListResponse<Part>>()
    connection.subscribe(parts, {}, received)
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }]) })
    const call = connection.call(createPart, {}, { name: 'renamed' })
    expect(received.states).toHaveLength(1)
    const body = current().last('call').body as { id: string }
    expect(body.id).toMatch(uuidPattern(7))
    current().answer('call', { payload: { id: 'a', name: 'renamed' } })
    await call
    expect(received.last()?.data.items).toEqual([{ id: 'a', name: 'renamed' }])
    expect(received.last()?.data.total).toBe(1)
    const failing = connection.call(createPart, {}, { name: 'x' }, { optimistic: { id: 'z', name: 'x' } })
    current().end(1006)
    await expect(failing).rejects.toBeInstanceOf(NetworkError)
    expect(received.last()?.data.items.map((item) => item.id)).toEqual(['a'])
  })

  test('a batch and a ping go over the socket', async () => {
    const { connection, current } = connect()
    const batch = connection.batch([batchCall(deletePart, { id: 'a' }, undefined)], { idempotencyKey: 'k' })
    current().open()
    expect(current().last('batch')).toMatchObject({ body: [{ target: 'deletePart', params: { id: 'a' } }], idempotencyKey: 'k' })
    current().answer('batch', { payload: [{ status: 204 }] })
    expect(await batch).toEqual([{ status: 204 }])
    const ping = connection.ping()
    current().answer('ping')
    await ping
  })

  test('a subscription whose stored filter was evicted registers its inline filter again, once (D54)', () => {
    const { connection, current } = connect()
    const inline = listener<ListResponse<Part>>()
    connection.subscribe(parts, { filter: { search: 'bolt' } }, inline)
    current().open()
    current().answer('subscribe', { subscription: 's1', filterId: 'f1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([]) })
    expect(inline.last()?.filterId).toBe('f1')

    const linked = listener<ListResponse<Part>>()
    connection.subscribe(parts, { filterId: 'f1' }, linked)
    const byID = current().last('subscribe')
    expect(byID.params).toEqual({ filterId: 'f1' })
    current().receive({ id: byID.id, ok: false, status: 404, error: { code: 'not_found', message: 'gone' } })
    const again = current().last('subscribe')
    expect(again.params).toEqual({ filter: '{"search":"bolt"}' })
    current().receive({ id: again.id, ok: false, status: 404, error: { code: 'not_found', message: 'gone' } })
    expect(linked.errors[0]?.code).toBe('not_found')
    expect(current().sent.filter((frame) => frame.op === 'subscribe')).toHaveLength(3)

    const unknown = listener<ListResponse<Part>>()
    connection.subscribe(parts, { filterId: 'f9' }, unknown)
    current().receive({ id: current().last('subscribe').id, ok: false, status: 404, error: { code: 'not_found', message: 'gone' } })
    expect(unknown.errors).toHaveLength(1)
  })

  test('the socket URL and the identity come from the options', async () => {
    const { Socket, current } = fakeSockets()
    const connection = new LiveConnection({ url: () => 'wss://dynamic/socket', WebSocket: Socket, identity: () => 'password:user' })
    expect(connection.identity()).toBe('password:user')
    expect(new LiveConnection({ url: 'x' }).identity()).toBe('')
    connection.subscribe(parts, {}, listener())
    expect(current().url).toBe('wss://dynamic/socket')
    await settle()
  })
})
