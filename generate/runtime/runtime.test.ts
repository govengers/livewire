import { describe, expect, test } from 'bun:test'
import { FakeClock, fakeFetch, fakeSockets, json, settle } from './fakes'
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
  prepareCall,
  read,
  routeOf,
  upload,
  uuidv4,
  uuidv7,
  wireParameters,
  type ListResponse,
  type LiveOptions,
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
const notes = read<PartsQuery, ListResponse<Part>>({ target: 'notes', path: '/notes', pathParameters: [], list: true })
const createPart = mutation<Record<string, never>, { id?: string; name: string }, Part>({
  name: 'createPart', method: 'POST', path: '/parts', pathParameters: [], body: true, content: true,
  effect: { kind: 'upserts', target: 'parts' }, clientID: 7,
})
const deletePart = mutation<{ id: string }, undefined, void>({
  name: 'deletePart', method: 'DELETE', path: '/parts/{id}', pathParameters: ['id'], body: false, content: false,
  effect: { kind: 'removes', target: 'parts' },
})
const attach = upload<{ id: string }, { size: number }, 'image/png'>({ name: 'attach', path: '/parts/{id}/files', pathParameters: ['id'], contentTypes: ['image/png'] })
const partsExport = exportTarget<{ sort?: readonly string[]; filterId?: string }>({ target: 'parts', path: '/parts/export' })

const uuidPattern = (version: number) => new RegExp(`^[0-9a-f]{8}-[0-9a-f]{4}-${version}[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

function list(items: Part[], total = items.length): ListResponse<Part> {
  return { limit: 100, offset: 0, total, items }
}

function connect(options: Partial<LiveOptions> = {}) {
  const fakes = fakeSockets()
  const clock = new FakeClock()
  const connection = new LiveConnection({ url: 'wss://example.com/socket', WebSocket: fakes.Socket, reconnectDelay: 10, random: () => 0.5, clock, ...options })
  return { connection, clock, ...fakes }
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

/** A connection with an open socket and one parts list subscription that got its snapshot. */
function subscribedList(items: Part[], options: Partial<LiveOptions> = {}) {
  const connected = connect(options)
  const received = listener<ListResponse<Part>>()
  connected.connection.subscribe(parts, {}, received)
  connected.current().open()
  connected.current().answer('subscribe', { subscription: 's1' })
  connected.current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list(items) })
  return { ...connected, received, ids: () => received.last()?.data.items.map((item) => item.id) }
}

const notFound = { code: 'not_found', message: 'gone' }

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
    const key = uuidv7(0x0123456789ab)
    expect(key).toMatch(uuidPattern(7))
    expect(key.replace(/-/g, '').slice(0, 12)).toBe('0123456789ab')
    expect(uuidv4()).toMatch(uuidPattern(4))
    expect(uuidv7()).not.toBe(uuidv7())
  })

  test('UUIDs v7 of one millisecond, and of a clock that steps back, sort in the order they were made', () => {
    const burst = Array.from({ length: 5000 }, () => uuidv7(0x0200000000ab))
    expect([...burst].sort()).toEqual(burst)
    const later = uuidv7(0x0100000000ab)
    expect(later > (burst.at(-1) ?? '')).toBe(true)
    for (const key of burst) expect(key).toMatch(uuidPattern(7))
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
    expect(new ApiError(0, notFound).status).toBe(404)
    expect(new ApiError(409, { code: 'something_else' }).code).toBe('conflict')
  })

  test('the constructors validate their options', () => {
    expect(() => new Client({ baseURL: '' })).toThrow('ClientOptions.baseURL is required')
    expect(() => new LiveConnection({ url: '' })).toThrow('LiveOptions.url is required')
    expect(() => new LiveConnection({ url: 'x', reconnectDelay: -1 })).toThrow('reconnectDelay')
    expect(() => new LiveConnection({ url: 'x', reconnectDelay: 100, maximumReconnectDelay: 50 })).toThrow('maximumReconnectDelay')
    expect(() => new LiveConnection({ url: 'x', pingInterval: Number.NaN })).toThrow('pingInterval')
    expect(() => new LiveConnection({ url: 'x', requestTimeout: 0 })).toThrow('requestTimeout')
    expect(() => new LiveConnection({ url: 'x', reconcileWindow: Number.POSITIVE_INFINITY })).toThrow('reconcileWindow')
    expect(new LiveConnection({ url: () => '' }).status).toBe('idle')
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

  test('a prepared call retries with the same client id and key (D16, D18)', async () => {
    const { fetch, calls } = fakeFetch(new TypeError('offline'), json({ id: 'p', name: 'a' }, 201))
    const client = new Client({ baseURL: base, fetch })
    const prepared = prepareCall(createPart, { name: 'a' })
    await expect(client.call(createPart, {}, prepared.body, { idempotencyKey: prepared.idempotencyKey })).rejects.toBeInstanceOf(NetworkError)
    await client.call(createPart, {}, prepared.body, { idempotencyKey: prepared.idempotencyKey })
    const [first, second] = calls.map((call) => [JSON.parse(String(call.init.body)) as { id: string }, (call.init.headers as Record<string, string>)['Idempotency-Key']] as const)
    expect(first?.[0].id).toBe(prepared.body.id ?? '')
    expect(second).toEqual(first ?? [{ id: '' }, ''])
    expect(prepareCall(deletePart, undefined, { idempotencyKey: 'k' })).toEqual({ body: undefined, idempotencyKey: 'k' })
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

  test('a read whose unnamed filterId was evicted sends the filter it saved, as saved, inline once (D54)', async () => {
    const stored = { id: 'f1', resource: 'parts', filter: { search: 'bolt' }, createdAt: '', lastUsedAt: '' }
    const { fetch, calls } = fakeFetch(json(stored, 201), json(notFound, 404), json(list([])))
    const client = new Client({ baseURL: base, fetch })
    const document = { search: 'bolt' }
    await client.saveFilter({ resource: 'parts', filter: document })
    document.search = 'edited'
    expect(await client.read(parts, { filterId: 'f1', limit: 5 })).toEqual(list([]))
    expect(calls[1]?.url).toBe(`${base}/parts?filterId=f1&limit=5`)
    const retried = new URL(String(calls[2]?.url))
    expect(retried.searchParams.get('filterId')).toBeNull()
    expect(retried.searchParams.get('filter')).toBe('{"search":"bolt"}')
    expect(retried.searchParams.get('limit')).toBe('5')
  })

  test('D54 recovers only a 404, only once and only an unnamed filter this client saved', async () => {
    const stored = (id: string) => json({ id, resource: 'parts', filter: {}, createdAt: '', lastUsedAt: '' }, 201)
    const { fetch, calls } = fakeFetch(
      stored('unnamed'), stored('named'),
      json({ code: 'invalid_filter', message: 'bad' }, 400),
      json(notFound, 404), json(notFound, 404),
      json(notFound, 404),
      json(notFound, 404),
    )
    const client = new Client({ baseURL: base, fetch })
    await client.saveFilter({ resource: 'parts', filter: {} })
    await client.saveFilter({ resource: 'parts', filter: {}, name: 'mine' })
    await expect(client.read(parts, { filterId: 'unnamed' })).rejects.toMatchObject({ code: 'invalid_filter' })
    await expect(client.read(parts, { filterId: 'unnamed' })).rejects.toMatchObject({ code: 'not_found' })
    await expect(client.read(parts, { filterId: 'named' })).rejects.toMatchObject({ code: 'not_found' })
    await expect(client.read(parts, { filterId: 'other' })).rejects.toMatchObject({ code: 'not_found' })
    expect(calls).toHaveLength(7)
    await client.deleteFilter('unnamed').catch(() => {})
  })

  test('a download of an evicted unnamed filterId sends the filter inline once (D54)', async () => {
    const stored = { id: 'f1', resource: 'parts', filter: { search: 'x' }, createdAt: '', lastUsedAt: '' }
    const { fetch, calls } = fakeFetch(json(stored, 201), json(notFound, 404), new Response('id\n'))
    const client = new Client({ baseURL: base, fetch })
    await client.saveFilter({ resource: 'parts', filter: { search: 'x' } })
    expect(await (await client.download(partsExport, { filterId: 'f1' })).blob.text()).toBe('id\n')
    expect(new URL(String(calls[2]?.url)).searchParams.get('filter')).toBe('{"search":"x"}')
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

  test('an unanswered request fails at its deadline while pings keep the socket, and an aborted one at once', async () => {
    const { connection, clock, current } = connect({ requestTimeout: 100, pingInterval: 30 })
    connection.subscribe(parts, {}, listener())
    current().open()
    const query = connection.query(notes, {})
    for (let step = 0; step < 3; step++) {
      clock.advance(30)
      current().answer('ping')
    }
    clock.advance(10)
    await expect(query).rejects.toThrow('got no answer within 100 ms')
    const late = current().last('query')
    current().receive({ id: late.id, ok: true, status: 200, payload: list([]) })
    expect(connection.status).toBe('open')

    const abort = new AbortController()
    const aborted = connection.call(deletePart, { id: 'a' }, undefined, { signal: abort.signal })
    abort.abort()
    await expect(aborted).rejects.toThrow('aborted')
    const before = new AbortController()
    before.abort()
    await expect(connection.batch([], { signal: before.signal })).rejects.toThrow('aborted')
  })

  test('an aborted request queued before the socket opened is never sent', async () => {
    const { connection, current } = connect()
    const abort = new AbortController()
    const query = connection.query(notes, {}, { signal: abort.signal })
    abort.abort()
    await expect(query).rejects.toBeInstanceOf(NetworkError)
    connection.subscribe(parts, {}, listener())
    current().open()
    expect(current().sent.map((frame) => frame.op)).toEqual(['subscribe'])
  })

  test('a list subscription applies snapshot and diff pushes in sequence', () => {
    const { received, current } = subscribedList([{ id: 'a', name: 'A' }])
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
    current().receive({ sub: 's1', seq: 3, kind: 'error', payload: notFound })
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
    const { received, current } = subscribedList([{ id: 'a', name: 'A' }])
    current().receive({ sub: 's1', seq: 3, kind: 'diff', payload: { upserts: [], removes: ['a'] } })
    expect(current().last('unsubscribe').target).toBe('s1')
    current().answer('subscribe', { subscription: 's2' })
    current().receive({ sub: 's2', seq: 1, kind: 'snapshot', payload: list([]) })
    expect(received.last()?.data.items).toEqual([])
  })

  test('unsubscribe tells the server, also when the subscribe is still on its way', () => {
    const { connection, current } = connect()
    const first = connection.subscribe(parts, {}, listener())
    const second = connection.subscribe(partItem, { id: 'x' }, listener())
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
    const { connection, clock, sockets, current } = connect()
    connection.onStatus((status) => void statuses.push(status))
    connection.subscribe(parts, { limit: 3 }, listener())
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    const registration = connection.join(presence, { key: 'k' }, { cursor: 1 })
    current().answer('join')
    await registration.ready
    const query = connection.query(notes, {})
    current().end(1006)
    await expect(query).rejects.toBeInstanceOf(NetworkError)
    expect(connection.status).toBe('reconnecting')
    clock.advance(4)
    expect(sockets).toHaveLength(1)
    clock.advance(10)
    expect(sockets).toHaveLength(2)
    current().open()
    expect(current().last('subscribe')).toMatchObject({ target: 'parts', params: { limit: '3' } })
    expect(current().last('join')).toMatchObject({ target: 'parts', params: { key: 'k' }, body: { cursor: 1 } })
    expect(statuses).toEqual(['idle', 'connecting', 'open', 'reconnecting', 'open'])
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
    const { connection, clock, sockets, current } = connect({ onUnauthorized: () => void unauthorized++ })
    connection.subscribe(parts, {}, listener())
    current().open()
    current().end(4401)
    expect([connection.status, unauthorized]).toEqual(['unauthorized', 1])
    clock.advance(60_000)
    expect(sockets).toHaveLength(1)
    await expect(connection.query(notes, {})).rejects.toThrow('unauthorized')
    connection.subscribe(parts, {}, listener())
    expect(sockets).toHaveLength(1)
    connection.reconnect()
    expect(sockets).toHaveLength(2)
    current().open()
    expect(current().sent.filter((frame) => frame.op === 'subscribe')).toHaveLength(2)
  })

  test('1001 reconnects at once with jitter, 1013 backs off and doubles', () => {
    const { connection, clock, sockets, current } = connect({ reconnectDelay: 40, random: () => 0 })
    connection.subscribe(parts, {}, listener())
    current().open()
    current().end(1001)
    clock.advance(0)
    expect(sockets).toHaveLength(2)
    current().end(1013)
    clock.advance(19)
    expect(sockets).toHaveLength(2)
    clock.advance(1)
    expect(sockets).toHaveLength(3)
    current().end(1013)
    clock.advance(39)
    expect(sockets).toHaveLength(3)
    clock.advance(1)
    expect(sockets).toHaveLength(4)
  })

  test('pings go out, and a silent socket is closed and replaced', () => {
    const { connection, clock, sockets, current } = connect({ pingInterval: 5, reconnectDelay: 0 })
    connection.subscribe(parts, {}, listener())
    current().open()
    clock.advance(5)
    current().answer('ping')
    clock.advance(10)
    expect(sockets).toHaveLength(1)
    clock.advance(5)
    expect(sockets[0]?.closedWith).toBe(1005)
    expect(sockets).toHaveLength(2)
  })

  test('a changed identity replaces the socket, which the upgrade request authorized (D6)', async () => {
    let identity = 'password:alice'
    const { connection, clock, sockets, current } = connect({ identity: () => identity })
    const received = listener<ListResponse<Part>>()
    connection.subscribe(parts, {}, received)
    current().open()
    current().answer('subscribe', { subscription: 's1' })
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([{ id: 'a', name: 'secret of alice' }]) })
    const owed = connection.query(notes, {})
    identity = 'password:bob'
    connection.checkIdentity()
    expect(sockets).toHaveLength(2)
    expect(sockets[0]?.closedWith).toBe(1000)
    await expect(owed).rejects.toThrow('the identity changed')
    current().open()
    expect(current().last('subscribe').target).toBe('parts')
    current().receive({ sub: 's1', seq: 1, kind: 'snapshot', payload: list([]) })
    expect(received.states).toHaveLength(1)

    identity = 'password:carol'
    clock.advance(25_000)
    expect(sockets).toHaveLength(3)
    identity = 'password:dave'
    void connection.ping().catch(() => {})
    expect(sockets).toHaveLength(4)
    connection.checkIdentity()
    expect(sockets).toHaveLength(4)
  })

  test('after a 4401 another identity connects again', () => {
    let identity = 'password:alice'
    const { connection, sockets, current } = connect({ identity: () => identity })
    connection.subscribe(parts, {}, listener())
    current().open()
    current().end(4401)
    identity = 'password:bob'
    connection.checkIdentity()
    expect([sockets.length, connection.status]).toEqual([2, 'connecting'])
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

  test('a removal shows at once, pending, and the next push decides (D36)', async () => {
    const { connection, current, received, ids } = subscribedList([{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }])
    const call = connection.call(deletePart, { id: 'a' }, undefined)
    expect([ids(), received.last()?.data.total, received.last()?.pending]).toEqual([['b'], 1, ['a']])
    current().answer('call', { status: 204 })
    await call
    expect([ids(), received.last()?.pending]).toEqual([['b'], []])
    current().receive({ sub: 's1', seq: 2, kind: 'snapshot', payload: list([{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }]) })
    expect(ids()).toEqual(['a', 'b'])
  })

  test('a refused removal rolls back, also after a push arrived meanwhile', async () => {
    const { connection, current, received, ids } = subscribedList([{ id: 'a', name: 'A' }])
    const call = connection.call(deletePart, { id: 'a' }, undefined)
    const frame = current().last('call')
    expect(frame).toMatchObject({ target: 'deletePart', params: { id: 'a' } })
    expect(frame.idempotencyKey).toMatch(uuidPattern(7))
    current().receive({ sub: 's1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'c', name: 'C' }], removes: [], order: ['a', 'c'], total: 2 } })
    expect([ids(), received.last()?.pending]).toEqual([['c'], ['a']])
    current().receive({ id: frame.id, ok: false, status: 409, error: { code: 'conflict', message: 'no' } })
    await expect(call).rejects.toBeInstanceOf(ApiError)
    expect([ids(), received.last()?.pending]).toEqual([['a', 'c'], []])
  })

  test('an upsert shows the optimistic item, then the answer, and the next push wins (D36)', async () => {
    const { connection, current, received } = subscribedList([{ id: 'a', name: 'A' }])
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

  test('a settled upsert of a row the list does not hold ends with the reconcile window: an unchanged list pushes nothing (D36)', async () => {
    const { connection, clock, current, received, ids } = subscribedList([{ id: 'a', name: 'A' }], { reconcileWindow: 500 })
    const call = connection.call(createPart, {}, { name: 'filtered out' })
    current().answer('call', { status: 201, payload: { id: 'z', name: 'filtered out' } })
    await call
    expect([ids(), received.last()?.data.total]).toEqual([['a', 'z'], 2])
    clock.advance(499)
    expect(ids()).toEqual(['a', 'z'])
    clock.advance(1)
    expect([ids(), received.last()?.data.total]).toEqual([['a'], 1])
  })

  test('an answer that arrives after a push of the list never overrides the push (D36)', async () => {
    const { connection, current, received } = subscribedList([{ id: 'a', name: 'A' }])
    const call = connection.call(createPart, {}, { id: 'a', name: 'mine' }, { optimistic: { id: 'a', name: 'mine' } })
    const frame = current().last('call')
    current().receive({ sub: 's1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'a', name: 'newer' }], removes: [] } })
    current().receive({ id: frame.id, ok: true, status: 201, payload: { id: 'a', name: 'older' } })
    await call
    expect(received.last()?.data.items).toEqual([{ id: 'a', name: 'newer' }])
    expect(received.last()?.pending).toEqual([])
  })

  test('two calls on one row settle on their own', async () => {
    const { connection, clock, current, received } = subscribedList([{ id: 'a', name: 'A' }])
    const first = connection.call(createPart, {}, { id: 'a', name: 'one' }, { optimistic: { id: 'a', name: 'one' } })
    const firstFrame = current().last('call')
    const second = connection.call(createPart, {}, { id: 'a', name: 'two' }, { optimistic: { id: 'a', name: 'two' } })
    const secondFrame = current().last('call')
    current().receive({ id: secondFrame.id, ok: true, status: 201, payload: { id: 'a', name: 'two' } })
    current().receive({ sub: 's1', seq: 2, kind: 'diff', payload: { upserts: [{ id: 'a', name: 'two' }], removes: [] } })
    current().receive({ id: firstFrame.id, ok: true, status: 201, payload: { id: 'a', name: 'one' } })
    await Promise.all([first, second])
    clock.advance(0)
    expect(received.last()?.data.items).toEqual([{ id: 'a', name: 'two' }])
  })

  test('an upsert without an optimistic item applies the answer and a lost socket takes a pending one back', async () => {
    const { connection, current, received, ids } = subscribedList([{ id: 'a', name: 'A' }])
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
    expect(ids()).toEqual(['a'])
  })

  test('a prepared socket call retries with the same client id and key', async () => {
    const { connection, current } = connect()
    const prepared = prepareCall(createPart, { name: 'a' })
    const first = connection.call(createPart, {}, prepared.body, { idempotencyKey: prepared.idempotencyKey })
    current().open()
    const sent = current().last('call')
    current().end(1006)
    await expect(first).rejects.toBeInstanceOf(NetworkError)
    const retry = connection.call(createPart, {}, prepared.body, { idempotencyKey: prepared.idempotencyKey })
    current().open()
    expect(current().last('call')).toMatchObject({ body: sent.body, idempotencyKey: sent.idempotencyKey })
    current().answer('call', { payload: { id: 'p', name: 'a' } })
    await retry
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
    current().receive({ id: byID.id, ok: false, status: 404, error: notFound })
    const again = current().last('subscribe')
    expect(again.params).toEqual({ filter: '{"search":"bolt"}' })
    current().receive({ id: again.id, ok: false, status: 404, error: notFound })
    expect(linked.errors[0]?.code).toBe('not_found')
    expect(current().sent.filter((frame) => frame.op === 'subscribe')).toHaveLength(3)

    const unknown = listener<ListResponse<Part>>()
    connection.subscribe(parts, { filterId: 'f9' }, unknown)
    current().receive({ id: current().last('subscribe').id, ok: false, status: 404, error: notFound })
    expect(unknown.errors).toHaveLength(1)
  })

  test('a socket query of an evicted registered filterId sends the filter inline once (D54)', async () => {
    const { connection, current } = connect()
    connection.subscribe(parts, { filter: { search: 'bolt' } }, listener())
    current().open()
    current().answer('subscribe', { subscription: 's1', filterId: 'f1' })
    const query = connection.query(parts, { filterId: 'f1' })
    const first = current().last('query')
    current().receive({ id: first.id, ok: false, status: 404, error: notFound })
    await settle()
    const second = current().last('query')
    expect([first.params, second.params]).toEqual([{ filterId: 'f1' }, { filter: '{"search":"bolt"}' }])
    current().answer('query', { payload: list([]) })
    expect(await query).toEqual(list([]))
  })

  test('presence: a registration that leaves before the socket opens never joins (D37)', () => {
    const { connection, current } = connect()
    connection.subscribe(parts, {}, listener())
    const registration = connection.join(presence, { key: 'k' }, { cursor: 1 })
    registration.leave()
    registration.update({ cursor: 2 })
    current().open()
    expect(current().sent.map((frame) => frame.op)).toEqual(['subscribe'])
  })

  test('presence: registrations on one record share it, and the last to leave leaves (D37)', async () => {
    const { connection, current } = connect()
    const first = connection.join(presence, { key: 'k' }, { cursor: 1 })
    const second = connection.join(presence, { key: 'k' }, { cursor: 2 })
    current().open()
    expect(current().sent.filter((frame) => frame.op === 'join')).toHaveLength(1)
    expect(current().last('join').body).toEqual({ cursor: 2 })
    current().answer('join')
    await Promise.all([first.ready, second.ready])
    first.leave()
    first.leave()
    expect(current().sent.filter((frame) => frame.op === 'leave')).toHaveLength(0)
    second.update({ cursor: 3 })
    expect(current().last('join').body).toEqual({ cursor: 3 })
    current().answer('join')
    await settle()
    second.leave()
    expect(current().last('leave')).toMatchObject({ target: 'parts', params: { key: 'k' } })
  })

  test('presence: a state change while a join is in flight follows its answer, and a leave in flight waits for it', async () => {
    const { connection, current } = connect()
    const registration = connection.join(presence, { key: 'k' }, { cursor: 1 })
    current().open()
    registration.update({ cursor: 2 })
    expect(current().sent.filter((frame) => frame.op === 'join')).toHaveLength(1)
    current().answer('join')
    await settle()
    expect(current().last('join').body).toEqual({ cursor: 2 })
    registration.leave()
    expect(current().sent.filter((frame) => frame.op === 'leave')).toHaveLength(0)
    current().answer('join')
    await settle()
    expect(current().sent.filter((frame) => frame.op === 'leave')).toHaveLength(1)
  })

  test('presence: a refused join reaches ready and onError, a refused leave the registrations left', async () => {
    const { connection, current } = connect()
    const errors: Error[] = []
    const refused = connection.join(presence, { key: 'k' }, {}, { onError: (error) => void errors.push(error) })
    current().open()
    current().receive({ id: current().last('join').id, ok: false, status: 403, error: { code: 'forbidden', message: 'no' } })
    await expect(refused.ready).rejects.toBeInstanceOf(ApiError)
    expect(errors).toHaveLength(1)
    refused.leave()

    const held = connection.join(presence, { key: 'other' }, {})
    current().answer('join')
    await held.ready
    held.leave()
    current().receive({ id: current().last('leave').id, ok: false, status: 500, error: { code: 'internal', message: 'x' } })
    await settle()
  })

  test('the socket URL and the identity come from the options', () => {
    const { Socket, current } = fakeSockets()
    const connection = new LiveConnection({ url: () => 'wss://dynamic/socket', WebSocket: Socket, identity: () => 'password:user' })
    expect(connection.identity()).toBe('password:user')
    expect(new LiveConnection({ url: 'x' }).identity()).toBe('')
    connection.subscribe(parts, {}, listener())
    expect(current().url).toBe('wss://dynamic/socket')
    connection.close()
  })
})
