// The livewire runtime: the REST client, the socket connection and the library's wire types.
// The generator copies this file next to the generated api.ts, types.ts and filters.ts; it has
// no global state: an application builds one Client and one LiveConnection and passes them on.

/** The closed set of error codes (design section 3). */
export type ErrorCode =
  | 'invalid_request'
  | 'invalid_filter'
  | 'invalid_sort'
  | 'invalid_page'
  | 'invalid_cursor'
  | 'forbidden_field'
  | 'unauthenticated'
  | 'forbidden'
  | 'not_found'
  | 'conflict'
  | 'in_progress'
  | 'internal'

/** The one error body of every REST response and socket error. */
export interface ErrorBody {
  code: ErrorCode
  message: string
  fields?: Record<string, string>
}

/** The body of every list read (D14); `next` is the cursor of the next page in cursor mode (D38). */
export interface ListResponse<Item> {
  limit: number
  offset: number
  total: number
  items: Item[]
  next?: string
}

/** A list change, the wire's `diff` push. */
export interface ListChange<Item> {
  upserts: Item[]
  removes: string[]
  order?: string[]
  total?: number
}

export type ChangeOperation = 'insert' | 'update' | 'delete'

/** One version of an item of a versioned resource (D34, D56); `id` is the version number. */
export interface Version<Item> {
  id: number
  operation: ChangeOperation
  validFrom: string
  validTo: string | null
  actor: string
  realActor: string
  mutation: string
  requestId: string
  data: Item | null
}

/** One value of a distinct read (D15) and how many rows carry it. */
export interface DistinctValue {
  value: unknown
  count: number
}

/** One socket's presence on a record (D37): the socket, its display name and its state. */
export interface PresenceItem<State = unknown> {
  id: string
  display: string
  state: State
}

/** A stored filter (D13). */
export interface FilterItem<Document = unknown> {
  id: string
  resource: string
  context?: string
  name?: string
  filter: Document
  createdAt: string
  lastUsedAt: string
}

/** The body of POST /filters: an unnamed filter is a registration, a named one is listed. */
export interface FilterRequest<Document = unknown> {
  resource: string
  context?: string
  name?: string
  filter: Document
}

/** One call of a batch (D47); build it with batchCall. */
export interface BatchCall {
  target: string
  params?: Record<string, string>
  body?: unknown
}

/** The answer of one call of a committed batch, in the order of the calls. */
export interface BatchResult {
  status: number
  payload?: unknown
}

/** A time relative to the database clock of the read: "-7d", "-1h30m", "+2w" (s, m, h, d, w). */
export interface RelativeTime {
  relative: string
}

/** The parameters of a declaration without any. */
export type NoParameters = Record<string, never>

/** A read as the generated api.ts declares it: its socket target (D46), its REST route and kind. */
export interface ReadTarget<Parameters, Data> {
  readonly target: string
  readonly path: string
  readonly pathParameters: readonly string[]
  readonly list: boolean
  readonly live: boolean
  /** Carries the types only; never set. */
  readonly types?: (parameters: Parameters) => Data
}

/** A read a socket can subscribe to: its declaration has Tables, or it is a presence read. */
export interface LiveTarget<Parameters, Data> extends ReadTarget<Parameters, Data> {
  readonly live: true
}

/** The optimistic effect of a mutation on the live lists of a target (D36). */
export interface Effect {
  readonly kind: 'upserts' | 'removes'
  readonly target: string
}

export interface MutationTarget<Parameters, Body, Response> {
  readonly name: string
  readonly method: 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  readonly path: string
  readonly pathParameters: readonly string[]
  readonly body: boolean
  readonly content: boolean
  readonly effect?: Effect
  /** The UUID version of a client-generated id a create accepts (D18). */
  readonly clientID?: 4 | 7
  /** Carries the types only; never set. */
  readonly types?: (parameters: Parameters, body: Body) => Response
}

export interface UploadTarget<Parameters, Response, ContentType extends string> {
  readonly name: string
  readonly path: string
  readonly pathParameters: readonly string[]
  readonly contentTypes: readonly ContentType[]
  /** Carries the types only; never set. */
  readonly types?: (parameters: Parameters) => Response
}

export interface ExportTarget<Query> {
  readonly target: string
  readonly path: string
  /** Carries the types only; never set. */
  readonly types?: (query: Query) => void
}

type ReadDescriptor = Omit<ReadTarget<unknown, unknown>, 'types' | 'live'>

/** A read the socket cannot subscribe to (its declaration has no Tables). */
export function read<Parameters, Data>(descriptor: ReadDescriptor): ReadTarget<Parameters, Data> {
  return { ...descriptor, live: false }
}

/** A read the socket can subscribe to. */
export function liveRead<Parameters, Data>(descriptor: ReadDescriptor): LiveTarget<Parameters, Data> {
  return { ...descriptor, live: true }
}

export function mutation<Parameters, Body, Response>(
  descriptor: Omit<MutationTarget<Parameters, Body, Response>, 'types'>,
): MutationTarget<Parameters, Body, Response> {
  return descriptor
}

export function upload<Parameters, Response, ContentType extends string>(
  descriptor: Omit<UploadTarget<Parameters, Response, ContentType>, 'types'>,
): UploadTarget<Parameters, Response, ContentType> {
  return descriptor
}

export function exportTarget<Query>(descriptor: Omit<ExportTarget<Query>, 'types'>): ExportTarget<Query> {
  return descriptor
}

const errorCodes: readonly ErrorCode[] = [
  'invalid_request', 'invalid_filter', 'invalid_sort', 'invalid_page', 'invalid_cursor', 'forbidden_field',
  'unauthenticated', 'forbidden', 'not_found', 'conflict', 'in_progress', 'internal',
]

const statusOfCode: Readonly<Record<ErrorCode, number>> = {
  invalid_request: 400, invalid_filter: 400, invalid_sort: 400, invalid_page: 400, invalid_cursor: 400,
  forbidden_field: 403, unauthenticated: 401, forbidden: 403, not_found: 404, conflict: 409, in_progress: 409, internal: 500,
}

function codeOfStatus(status: number): ErrorCode {
  switch (status) {
    case 400: return 'invalid_request'
    case 401: return 'unauthenticated'
    case 403: return 'forbidden'
    case 404: return 'not_found'
    case 409: return 'conflict'
    default: return 'internal'
  }
}

function isErrorCode(value: unknown): value is ErrorCode {
  return typeof value === 'string' && (errorCodes as readonly string[]).includes(value)
}

/**
 * The server refused or failed a request. `code` is from the closed set: a body that is not
 * livewire's (a proxy's error page) gets the code of its status; a socket push, which carries no
 * status, gets the status of its code. `body` is the body as it arrived.
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: ErrorCode
  readonly fields: Readonly<Record<string, string>>
  readonly body: unknown

  constructor(status: number, body: unknown) {
    const candidate = (typeof body === 'object' && body !== null ? body : {}) as Partial<Record<keyof ErrorBody, unknown>>
    const code = isErrorCode(candidate.code) ? candidate.code : codeOfStatus(status)
    super(typeof candidate.message === 'string' ? candidate.message : code)
    this.name = 'ApiError'
    this.status = status === 0 ? statusOfCode[code] : status
    this.code = code
    this.fields = isStringRecord(candidate.fields) ? candidate.fields : {}
    this.body = body
  }
}

function isStringRecord(value: unknown): value is Record<string, string> {
  return typeof value === 'object' && value !== null && Object.values(value).every((entry) => typeof entry === 'string')
}

/** The request never got an answer: offline, refused, aborted, or the socket closed under it. */
export class NetworkError extends Error {
  constructor(message: string, options?: { cause: unknown }) {
    super(message, options)
    this.name = 'NetworkError'
  }
}

/**
 * Writes parameters the way both transports read them: a string as it is, a number or a boolean
 * as text, a list as one comma-separated value, an object (a filter document) as JSON; undefined
 * and null are left out.
 */
export function wireParameters(parameters: object): Record<string, string> {
  const wire: Record<string, string> = {}
  for (const [name, value] of Object.entries(parameters)) {
    if (value === undefined || value === null) continue
    if (Array.isArray(value)) wire[name] = value.map(String).join(',')
    else if (typeof value === 'object') wire[name] = JSON.stringify(value)
    else wire[name] = String(value)
  }
  return wire
}

/** The REST path of a route with its placeholders filled and the query string of the rest. */
export function routeOf(path: string, pathParameters: readonly string[], wire: Record<string, string>): string {
  const query = new URLSearchParams()
  for (const [name, value] of Object.entries(wire)) if (!pathParameters.includes(name)) query.append(name, value)
  const filled = path.replace(/\{([^}]+)\}/g, (_, name: string) => {
    const value = wire[name]
    if (value === undefined || value === '') throw new TypeError(`livewire: the path parameter ${name} is missing`)
    return encodeURIComponent(value)
  })
  const search = query.toString()
  return search === '' ? filled : `${filled}?${search}`
}

/** A UUID version 7 (RFC 9562): a millisecond timestamp, then random bits. */
export function uuidv7(now: number = Date.now()): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  let timestamp = now
  for (let index = 5; index >= 0; index--) {
    bytes[index] = timestamp % 256
    timestamp = Math.floor(timestamp / 256)
  }
  return formatUUID(bytes, 0x70)
}

/** A random UUID version 4. */
export function uuidv4(): string {
  return formatUUID(crypto.getRandomValues(new Uint8Array(16)), 0x40)
}

function formatUUID(bytes: Uint8Array, version: number): string {
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | version
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

/** A fresh Idempotency-Key (D16): a UUID v7. */
export function idempotencyKey(): string {
  return uuidv7()
}

/** The body of a ClientID create with an `id` filled in when it has none (D18). */
function withClientID<Body>(target: { readonly clientID?: 4 | 7 }, body: Body): Body {
  if (target.clientID === undefined || typeof body !== 'object' || body === null || 'id' in body) return body
  return { ...body, id: target.clientID === 7 ? uuidv7() : uuidv4() }
}

/** One call of a batch (D47) for the mutation's typed parameters and body. */
export function batchCall<Parameters extends object, Body, Response>(
  target: MutationTarget<Parameters, Body, Response>,
  parameters: NoInfer<Parameters>,
  body: NoInfer<Body>,
): BatchCall {
  const call: BatchCall = { target: target.name }
  const wire = wireParameters(parameters)
  if (Object.keys(wire).length > 0) call.params = wire
  if (target.body) call.body = withClientID(target, body)
  return call
}

/** The id an item is diffed by: a string or a number, compared as text. */
export function itemID(item: unknown): string {
  return String((item as { id?: unknown } | null)?.id)
}

/** Applies a diff push to the list it was taken against (section 4). */
export function applyListChange<Item>(list: ListResponse<Item>, change: ListChange<Item>): ListResponse<Item> {
  const byID = new Map(list.items.map((item) => [itemID(item), item]))
  for (const item of change.upserts) byID.set(itemID(item), item)
  for (const id of change.removes) byID.delete(id)
  let order = change.order
  if (order === undefined) {
    order = list.items.map(itemID).filter((id) => byID.has(id))
    for (const item of change.upserts) if (!order.includes(itemID(item))) order.push(itemID(item))
  }
  const items: Item[] = []
  for (const id of order) {
    const item = byID.get(id)
    if (item !== undefined) items.push(item)
  }
  return { ...list, items, total: change.total ?? list.total }
}

export interface ClientOptions {
  /** The registry's BasePath, absolute ("https://api.example.com/api/v1") or below the page's origin ("/api/v1"). */
  baseURL: string
  fetch?: typeof fetch
  /** Headers of every request, an API key for instance (D20). */
  headers?: () => Record<string, string> | Promise<Record<string, string>>
  /** Default 'same-origin': the browser sends the session cookie to its own host. */
  credentials?: RequestCredentials
  /** Called with every 401 before it is thrown. */
  onUnauthorized?: (error: ApiError) => void
}

export interface RequestOptions {
  signal?: AbortSignal
}

export interface CallOptions extends RequestOptions {
  /** Default: a fresh UUID v7, so a retried call with the same key runs once (D16). */
  idempotencyKey?: string
}

export interface UploadOptions<ContentType extends string> extends CallOptions {
  /** Default: the blob's own type. */
  contentType?: ContentType
  /** Send a multipart/form-data body with the file as its part "file" instead of the raw body. */
  multipart?: boolean
  filename?: string
}

/** A finished export: the CSV, and the rows of the Livewire-Export-Rows trailer where the platform exposes trailers. */
export interface ExportDownload {
  blob: Blob
  rows: number | null
}

const idempotencyHeader = 'Idempotency-Key'
const exportRowsTrailer = 'Livewire-Export-Rows'

/** The REST client: every route of the generated api.ts runs through one. */
export class Client {
  private readonly options: ClientOptions
  /** The unnamed filters this client stored, by id: a read whose filterId was evicted sends them inline (D54). */
  private readonly unnamedFilters = new Map<string, unknown>()

  constructor(options: ClientOptions) {
    this.options = { ...options }
  }

  async read<Parameters extends object, Data>(target: ReadTarget<Parameters, Data>, parameters: NoInfer<Parameters>, options: RequestOptions = {}): Promise<Data> {
    const wire = wireParameters(parameters)
    try {
      return (await this.request('GET', routeOf(target.path, target.pathParameters, wire), options)) as Data
    } catch (error) {
      const filterId = wire['filterId']
      const document = filterId === undefined ? undefined : this.unnamedFilters.get(filterId)
      if (!(error instanceof ApiError) || error.code !== 'not_found' || document === undefined) throw error
      const inline: Record<string, string> = { ...wire, filter: JSON.stringify(document) }
      delete inline['filterId']
      return (await this.request('GET', routeOf(target.path, target.pathParameters, inline), options)) as Data
    }
  }

  async call<Parameters extends object, Body, Response>(
    target: MutationTarget<Parameters, Body, Response>,
    parameters: NoInfer<Parameters>,
    body: NoInfer<Body>,
    options: CallOptions = {},
  ): Promise<Response> {
    const path = routeOf(target.path, target.pathParameters, wireParameters(parameters))
    const init: RequestInit = { headers: { [idempotencyHeader]: options.idempotencyKey ?? idempotencyKey() } }
    if (target.body) {
      init.body = JSON.stringify(withClientID(target, body))
      init.headers = { ...init.headers, 'Content-Type': 'application/json' }
    }
    return (await this.request(target.method, path, options, init)) as Response
  }

  async upload<Parameters extends object, Response, ContentType extends string>(
    target: UploadTarget<Parameters, Response, ContentType>,
    parameters: NoInfer<Parameters>,
    file: Blob,
    options: UploadOptions<NoInfer<ContentType>> = {},
  ): Promise<Response> {
    const path = routeOf(target.path, target.pathParameters, wireParameters(parameters))
    const contentType = options.contentType ?? file.type
    const headers: Record<string, string> = { [idempotencyHeader]: options.idempotencyKey ?? idempotencyKey() }
    let body: BodyInit
    if (options.multipart === true) {
      const form = new FormData()
      form.append('file', new Blob([file], { type: contentType }), options.filename ?? 'file')
      body = form
    } else {
      headers['Content-Type'] = contentType
      body = file
    }
    return (await this.request('POST', path, options, { headers, body })) as Response
  }

  async batch(calls: readonly BatchCall[], options: CallOptions = {}): Promise<BatchResult[]> {
    const headers = { [idempotencyHeader]: options.idempotencyKey ?? idempotencyKey(), 'Content-Type': 'application/json' }
    return (await this.request('POST', '/batch', options, { headers, body: JSON.stringify(calls) })) as BatchResult[]
  }

  async saveFilter<Document>(request: FilterRequest<Document>, options: RequestOptions = {}): Promise<FilterItem<Document>> {
    const init = { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request) }
    const stored = (await this.request('POST', '/filters', options, init)) as FilterItem<Document>
    if (request.name === undefined || request.name === '') this.unnamedFilters.set(stored.id, request.filter)
    return stored
  }

  async listFilters(resource?: string, options: RequestOptions = {}): Promise<ListResponse<FilterItem>> {
    const path = resource === undefined ? '/filters' : `/filters?${new URLSearchParams({ resource }).toString()}`
    return (await this.request('GET', path, options)) as ListResponse<FilterItem>
  }

  async deleteFilter(id: string, options: RequestOptions = {}): Promise<void> {
    this.unnamedFilters.delete(id)
    await this.request('DELETE', `/filters/${encodeURIComponent(id)}`, options)
  }

  /** The URL of an export (D35), for a link the browser downloads with the session cookie. */
  exportURL<Query extends object>(target: ExportTarget<Query>, query: NoInfer<Query>): string {
    return this.url(routeOf(target.path, [], wireParameters(query))).toString()
  }

  /**
   * Downloads an export. A cut export fails the body's read (D60), a NetworkError then; where the
   * platform exposes trailers, a missing Livewire-Export-Rows trailer fails it as well.
   */
  async download<Query extends object>(target: ExportTarget<Query>, query: NoInfer<Query>, options: RequestOptions = {}): Promise<ExportDownload> {
    const response = await this.fetch('GET', routeOf(target.path, [], wireParameters(query)), options)
    let blob: Blob
    try {
      blob = await response.blob()
    } catch (cause) {
      throw new NetworkError('livewire: the export was cut off', { cause })
    }
    const trailer = (response as { trailer?: Promise<Headers> }).trailer
    if (trailer === undefined) return { blob, rows: null }
    const rows = (await trailer).get(exportRowsTrailer)
    if (rows === null) throw new NetworkError('livewire: the export ended without its row count')
    return { blob, rows: Number(rows) }
  }

  private url(path: string): URL {
    const origin = typeof location === 'undefined' ? undefined : location.origin
    return new URL(this.options.baseURL.replace(/\/$/, '') + path, origin)
  }

  private async request(method: string, path: string, options: RequestOptions, init: RequestInit = {}): Promise<unknown> {
    const response = await this.fetch(method, path, options, init)
    if (response.status === 204) return undefined
    let text: string
    try {
      text = await response.text()
    } catch (cause) {
      throw new NetworkError('livewire: the response was cut off', { cause })
    }
    return text === '' ? undefined : JSON.parse(text)
  }

  /** Sends one request and answers the response of a success; a refusal is thrown as an ApiError. */
  private async fetch(method: string, path: string, options: RequestOptions, init: RequestInit = {}): Promise<Response> {
    const headers = { ...(await this.options.headers?.()), ...(init.headers as Record<string, string> | undefined) }
    const request: RequestInit = { ...init, method, headers, credentials: this.options.credentials ?? 'same-origin' }
    if (options.signal !== undefined) request.signal = options.signal
    let response: Response
    try {
      response = await (this.options.fetch ?? fetch)(this.url(path), request)
    } catch (cause) {
      throw new NetworkError(`livewire: ${method} ${path} got no answer`, { cause })
    }
    if (response.ok) return response
    const text = await response.text().catch(() => '')
    let body: unknown = text
    try {
      body = JSON.parse(text)
    } catch {
      // a body that is not JSON (a proxy's page) stays text
    }
    const error = new ApiError(response.status, body)
    if (error.status === 401) this.options.onUnauthorized?.(error)
    throw error
  }
}

/** The state of the socket: idle without anything to serve, unauthorized after a 4401 (D7). */
export type LiveStatus = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'unauthorized' | 'closed'

/** What a subscription delivers: the data, the ids an optimistic call has not settled yet (D36), the filterId an inline filter was registered as (D13). */
export interface LiveState<Data> {
  data: Data
  pending: readonly string[]
  filterId?: string
}

export interface LiveListener<Data> {
  next(state: LiveState<Data>): void
  /** The subscription ended with an error (an error push or a refused subscribe); retry() starts it again. */
  error(error: ApiError): void
}

export interface LiveSubscription {
  unsubscribe(): void
  /** Subscribes again after an error ended the subscription. */
  retry(): void
}

/** The part of a WebSocket the connection uses, so tests and Node can bring their own. */
export interface WebSocketLike {
  readonly readyState: number
  onopen: ((event: unknown) => void) | null
  onmessage: ((event: { data: unknown }) => void) | null
  onclose: ((event: { code: number; reason: string }) => void) | null
  onerror: ((event: unknown) => void) | null
  send(data: string): void
  close(code?: number, reason?: string): void
}

export interface LiveOptions {
  /** The socket's URL, "wss://example.com/api/v1/socket", or a function that answers it on every connect. */
  url: string | (() => string)
  WebSocket?: new (url: string) => WebSocketLike
  /** Who the socket acts as, "provider:subject" or any stable key: the Vue snapshots are kept per identity (D21). */
  identity?: () => string
  /** Called on close code 4401: the identity is gone; the connection waits for reconnect(). */
  onUnauthorized?: () => void
  /** The first reconnect delay in ms (default 1000), doubling up to maximumReconnectDelay (default 30000), with jitter. */
  reconnectDelay?: number
  maximumReconnectDelay?: number
  /** How often a ping goes out in ms (default 25000); a socket silent for two intervals is closed and reconnected. */
  pingInterval?: number
  random?: () => number
}

/** The socket close codes (section 4). */
export const CloseCode = { normal: 1000, goingAway: 1001, tooSlow: 1013, identityGone: 4401 } as const

const socketOpen = 1

interface RequestFrame {
  id: string
  op: string
  target?: string
  params?: Record<string, string>
  body?: unknown
  idempotencyKey?: string
}

interface ResponseFrame {
  id: string
  ok: boolean
  status: number
  payload?: unknown
  error?: ErrorBody
  subscription?: string
  filterId?: string
}

interface PushFrame {
  sub: string
  seq: number
  kind: 'snapshot' | 'diff' | 'replace' | 'error'
  payload: unknown
}

interface PendingRequest {
  frame: RequestFrame
  resolve(frame: ResponseFrame): void
  reject(error: Error): void
}

/** An optimistic change one call made to a live list (D36). */
interface OverlayEntry {
  call: number
  id: string
  item?: unknown
  pending: boolean
}

interface Subscription {
  target: { readonly target: string; readonly list: boolean }
  parameters: Record<string, string>
  listener: LiveListener<unknown>
  serverID?: string
  filterId?: string
  sequence: number
  data?: unknown
  overlay: OverlayEntry[]
  ended: boolean
  retriedInline: boolean
}

interface Presence {
  declaration: string
  parameters: Record<string, string>
  state: unknown
}

export interface LiveCallOptions<Response> {
  idempotencyKey?: string
  /** The item an Upserts mutation shows at once, before the server answers (it needs its id). */
  optimistic?: Response
}

/**
 * One socket for the application (section 4): requests answered by id, subscriptions with their
 * pushes applied, reconnects with backoff and every subscription and presence restored after
 * one, no reconnect after 4401 until reconnect(). Nothing connects before the first request.
 */
export class LiveConnection {
  private readonly options: LiveOptions
  private socket: WebSocketLike | undefined
  private currentStatus: LiveStatus = 'idle'
  private readonly statusListeners = new Set<(status: LiveStatus) => void>()
  private readonly subscriptions = new Set<Subscription>()
  private readonly byServerID = new Map<string, Subscription>()
  private readonly presences = new Map<string, Presence>()
  private readonly pending = new Map<string, PendingRequest>()
  private readonly outbox: string[] = []
  /** The documents of the filters this connection registered inline, by filterId (D54). */
  private readonly registeredFilters = new Map<string, string>()
  private lastID = 0
  private lastCall = 0
  private attempt = 0
  private closedByClient = false
  private reconnectTimer: ReturnType<typeof setTimeout> | undefined
  private pingTimer: ReturnType<typeof setInterval> | undefined
  private lastMessage = 0

  constructor(options: LiveOptions) {
    this.options = { ...options }
  }

  get status(): LiveStatus {
    return this.currentStatus
  }

  /** The identity the snapshots of this connection are kept under, "" without one. */
  identity(): string {
    return this.options.identity?.() ?? ''
  }

  /** Calls listener with the status now and on every change; the result removes it. */
  onStatus(listener: (status: LiveStatus) => void): () => void {
    this.statusListeners.add(listener)
    listener(this.currentStatus)
    return () => this.statusListeners.delete(listener)
  }

  subscribe<Parameters extends object, Data>(
    target: LiveTarget<Parameters, Data>,
    parameters: NoInfer<Parameters>,
    listener: LiveListener<Data>,
  ): LiveSubscription {
    const subscription: Subscription = {
      target, parameters: wireParameters(parameters), listener: listener as LiveListener<unknown>,
      sequence: 0, overlay: [], ended: false, retriedInline: false,
    }
    this.subscriptions.add(subscription)
    this.startSubscription(subscription)
    return {
      unsubscribe: () => this.endSubscription(subscription),
      retry: () => {
        if (!this.subscriptions.has(subscription) || !subscription.ended) return
        subscription.ended = false
        subscription.retriedInline = false
        this.startSubscription(subscription)
      },
    }
  }

  /** One read over the socket. */
  async query<Parameters extends object, Data>(target: ReadTarget<Parameters, Data>, parameters: NoInfer<Parameters>): Promise<Data> {
    const frame = await this.request({ op: 'query', target: target.target, params: wireParameters(parameters) })
    return frame.payload as Data
  }

  /**
   * Runs a mutation over the socket. A mutation with an Upserts effect shows the optimistic
   * item (if given) and then the response in the live lists of its target at once, one with a
   * Removes effect hides the item named by the `id` parameter at once; both are pending until
   * the server answers, the next push of the list replaces them (the server wins), an error
   * takes them back (D36).
   */
  async call<Parameters extends object, Body, Response>(
    target: MutationTarget<Parameters, Body, Response>,
    parameters: NoInfer<Parameters>,
    body: NoInfer<Body>,
    options: LiveCallOptions<NoInfer<Response>> = {},
  ): Promise<Response> {
    const call = ++this.lastCall
    const wire = wireParameters(parameters)
    const effect = target.effect
    if (effect?.kind === 'removes' && wire['id'] !== undefined) this.applyEffect(effect, { call, id: wire['id'], pending: true })
    if (effect?.kind === 'upserts' && options.optimistic !== undefined) {
      this.applyEffect(effect, { call, id: itemID(options.optimistic), item: options.optimistic, pending: true })
    }
    const frame: Omit<RequestFrame, 'id'> = { op: 'call', target: target.name, params: wire, idempotencyKey: options.idempotencyKey ?? idempotencyKey() }
    if (target.body) frame.body = withClientID(target, body)
    let response: ResponseFrame
    try {
      response = await this.request(frame)
    } catch (error) {
      if (effect !== undefined) this.rollBack(effect, call)
      throw error
    }
    if (effect !== undefined) this.settle(effect, call, response.payload)
    return response.payload as Response
  }

  async batch(calls: readonly BatchCall[], options: { idempotencyKey?: string } = {}): Promise<BatchResult[]> {
    const frame = await this.request({ op: 'batch', body: calls, idempotencyKey: options.idempotencyKey ?? idempotencyKey() })
    return frame.payload as BatchResult[]
  }

  /** Announces this socket on the record of a presence read (D37); joining again updates the state. It is restored after every reconnect. */
  async join<Parameters extends object>(target: LiveTarget<Parameters, ListResponse<PresenceItem>>, parameters: NoInfer<Parameters>, state: unknown): Promise<void> {
    const presence: Presence = { declaration: declarationOf(target), parameters: wireParameters(parameters), state }
    this.presences.set(presenceKey(presence), presence)
    await this.request(joinFrame(presence))
  }

  async leave<Parameters extends object>(target: LiveTarget<Parameters, ListResponse<PresenceItem>>, parameters: NoInfer<Parameters>): Promise<void> {
    const presence: Presence = { declaration: declarationOf(target), parameters: wireParameters(parameters), state: undefined }
    if (!this.presences.delete(presenceKey(presence)) || this.socket?.readyState !== socketOpen) return
    await this.request({ op: 'leave', target: presence.declaration, params: presence.parameters })
  }

  async ping(): Promise<void> {
    await this.request({ op: 'ping' })
  }

  /** Connects again after a 4401 or close(), once the application has a session again. */
  reconnect(): void {
    this.closedByClient = false
    this.attempt = 0
    clearTimeout(this.reconnectTimer)
    this.reconnectTimer = undefined
    if (this.socket === undefined && this.needed()) this.connect()
  }

  /** Closes the socket for good; reconnect() opens it again. */
  close(): void {
    this.closedByClient = true
    clearTimeout(this.reconnectTimer)
    this.reconnectTimer = undefined
    const socket = this.socket
    if (socket === undefined) {
      this.setStatus('closed')
      return
    }
    this.detach(socket)
    socket.close(CloseCode.normal)
    this.closed(CloseCode.normal)
  }

  private setStatus(status: LiveStatus): void {
    if (status === this.currentStatus) return
    this.currentStatus = status
    for (const listener of this.statusListeners) listener(status)
  }

  private needed(): boolean {
    return this.outbox.length > 0 || this.presences.size > 0 || [...this.subscriptions].some((subscription) => !subscription.ended)
  }

  private request(frame: Omit<RequestFrame, 'id'>): Promise<ResponseFrame> {
    const identified: RequestFrame = { ...frame, id: String(++this.lastID) }
    return new Promise((resolve, reject) => {
      if (this.currentStatus === 'unauthorized' || this.closedByClient) {
        reject(new NetworkError(`livewire: the socket is ${this.currentStatus}; reconnect() opens it again`))
        return
      }
      this.pending.set(identified.id, { frame: identified, resolve, reject })
      this.send(identified)
    }).then((response) => {
      const answer = response as ResponseFrame
      if (!answer.ok) throw new ApiError(answer.status, answer.error)
      return answer
    })
  }

  private send(frame: RequestFrame): void {
    const encoded = JSON.stringify(frame)
    if (this.socket?.readyState === socketOpen) {
      this.socket.send(encoded)
      return
    }
    this.outbox.push(encoded)
    if (this.socket === undefined && this.reconnectTimer === undefined) this.connect()
  }

  private connect(): void {
    this.setStatus(this.attempt === 0 ? 'connecting' : 'reconnecting')
    const url = typeof this.options.url === 'function' ? this.options.url() : this.options.url
    const SocketClass = this.options.WebSocket ?? (globalThis.WebSocket as unknown as new (url: string) => WebSocketLike)
    const socket = new SocketClass(url)
    this.socket = socket
    socket.onopen = () => this.opened(socket)
    socket.onmessage = (event) => {
      this.lastMessage = Date.now()
      this.receive(JSON.parse(String(event.data)) as ResponseFrame | PushFrame)
    }
    socket.onclose = (event) => {
      this.detach(socket)
      this.closed(event.code)
    }
  }

  private opened(socket: WebSocketLike): void {
    this.attempt = 0
    this.setStatus('open')
    this.lastMessage = Date.now()
    for (const encoded of this.outbox.splice(0)) socket.send(encoded)
    for (const subscription of this.subscriptions) if (!subscription.ended) this.sendSubscribe(subscription)
    for (const presence of this.presences.values()) this.request(joinFrame(presence)).catch(() => {})
    const interval = this.options.pingInterval ?? 25_000
    this.pingTimer = setInterval(() => {
      if (Date.now() - this.lastMessage > 2 * interval) {
        // A socket that answers nothing is half open (a phone that switched networks).
        this.detach(socket)
        socket.close()
        this.closed(1006)
        return
      }
      this.ping().catch(() => {})
    }, interval)
  }

  private detach(socket: WebSocketLike): void {
    socket.onopen = socket.onmessage = socket.onclose = socket.onerror = null
    if (this.socket === socket) this.socket = undefined
    clearInterval(this.pingTimer)
    this.pingTimer = undefined
  }

  /** Every answer the socket owed is lost; the subscriptions start over on the next socket. */
  private closed(code: number): void {
    const lost = [...this.pending.values()]
    this.pending.clear()
    this.outbox.length = 0
    this.byServerID.clear()
    for (const subscription of this.subscriptions) {
      delete subscription.serverID
      subscription.sequence = 0
    }
    for (const request of lost) request.reject(new NetworkError(`livewire: the socket closed (${code}) before ${request.frame.op} was answered`))
    if (this.closedByClient) {
      this.setStatus('closed')
      return
    }
    if (code === CloseCode.identityGone) {
      this.setStatus('unauthorized')
      this.options.onUnauthorized?.()
      return
    }
    if (!this.needed()) {
      this.attempt = 0
      this.setStatus('idle')
      return
    }
    this.setStatus('reconnecting')
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = undefined
      this.connect()
    }, this.reconnectDelay(code))
  }

  /** A going-away close (a deploy) reconnects at once with jitter, so the clients spread out; any other doubles the delay. */
  private reconnectDelay(code: number): number {
    const random = this.options.random ?? Math.random
    const initial = this.options.reconnectDelay ?? 1000
    if (code === CloseCode.goingAway) return initial * random()
    const exponential = Math.min(this.options.maximumReconnectDelay ?? 30_000, initial * 2 ** this.attempt++)
    return exponential * (0.5 + random() / 2)
  }

  private receive(frame: ResponseFrame | PushFrame): void {
    if ('sub' in frame) {
      this.push(frame)
      return
    }
    const request = this.pending.get(frame.id)
    if (request === undefined) return
    this.pending.delete(frame.id)
    request.resolve(frame)
  }

  /** Subscribes now on an open socket; otherwise the next open sends every subscription. */
  private startSubscription(subscription: Subscription): void {
    if (this.socket?.readyState === socketOpen) this.sendSubscribe(subscription)
    else if (this.socket === undefined && this.reconnectTimer === undefined && this.currentStatus !== 'unauthorized' && !this.closedByClient) this.connect()
  }

  /** Sends a subscribe on the open socket. */
  private sendSubscribe(subscription: Subscription): void {
    const frame: RequestFrame = { id: String(++this.lastID), op: 'subscribe', target: subscription.target.target, params: subscription.parameters }
    this.pending.set(frame.id, {
      frame,
      resolve: (response) => this.subscribed(subscription, response),
      reject: () => {},
    })
    this.send(frame)
  }

  private subscribed(subscription: Subscription, response: ResponseFrame): void {
    if (response.ok && response.subscription !== undefined) {
      if (!this.subscriptions.has(subscription)) {
        this.request({ op: 'unsubscribe', target: response.subscription }).catch(() => {})
        return
      }
      subscription.serverID = response.subscription
      subscription.sequence = 0
      this.byServerID.set(response.subscription, subscription)
      if (response.filterId !== undefined) {
        subscription.filterId = response.filterId
        const inline = subscription.parameters['filter']
        if (inline !== undefined) this.registeredFilters.set(response.filterId, inline)
      }
      return
    }
    const error = new ApiError(response.status, response.error)
    const filterId = subscription.parameters['filterId']
    const document = filterId === undefined ? undefined : this.registeredFilters.get(filterId)
    if (error.code === 'not_found' && document !== undefined && !subscription.retriedInline) {
      // The filter was evicted (D54): register it inline again, once.
      subscription.retriedInline = true
      subscription.parameters = { ...subscription.parameters, filter: document }
      delete subscription.parameters['filterId']
      this.sendSubscribe(subscription)
      return
    }
    this.fail(subscription, error)
  }

  private fail(subscription: Subscription, error: ApiError): void {
    if (!this.subscriptions.has(subscription)) return
    subscription.ended = true
    if (subscription.serverID !== undefined) this.byServerID.delete(subscription.serverID)
    delete subscription.serverID
    subscription.listener.error(error)
  }

  private endSubscription(subscription: Subscription): void {
    if (!this.subscriptions.delete(subscription)) return
    const serverID = subscription.serverID
    if (serverID === undefined) return
    this.byServerID.delete(serverID)
    if (this.socket?.readyState === socketOpen) this.request({ op: 'unsubscribe', target: serverID }).catch(() => {})
  }

  private push(frame: PushFrame): void {
    const subscription = this.byServerID.get(frame.sub)
    if (subscription === undefined) return
    if (frame.seq !== subscription.sequence + 1) {
      // A lost push: the list cannot be diffed any more; a new subscription starts with a snapshot.
      this.byServerID.delete(frame.sub)
      this.request({ op: 'unsubscribe', target: frame.sub }).catch(() => {})
      delete subscription.serverID
      this.sendSubscribe(subscription)
      return
    }
    subscription.sequence = frame.seq
    switch (frame.kind) {
      case 'error':
        this.fail(subscription, new ApiError(0, frame.payload))
        return
      case 'diff':
        if (subscription.data === undefined) return
        subscription.data = applyListChange(subscription.data as ListResponse<unknown>, frame.payload as ListChange<unknown>)
        break
      case 'snapshot':
      case 'replace':
        subscription.data = frame.payload
    }
    subscription.overlay = subscription.overlay.filter((entry) => entry.pending)
    this.deliver(subscription)
  }

  private deliver(subscription: Subscription): void {
    if (subscription.data === undefined) return
    const state: LiveState<unknown> = { data: overlaid(subscription), pending: subscription.overlay.filter((entry) => entry.pending).map((entry) => entry.id) }
    if (subscription.filterId !== undefined) state.filterId = subscription.filterId
    subscription.listener.next(state)
  }

  private affected(effect: Effect): Subscription[] {
    return [...this.subscriptions].filter((subscription) => subscription.target.target === effect.target && subscription.target.list)
  }

  private applyEffect(effect: Effect, entry: OverlayEntry): void {
    for (const subscription of this.affected(effect)) {
      subscription.overlay.push({ ...entry })
      this.deliver(subscription)
    }
  }

  private settle(effect: Effect, call: number, response: unknown): void {
    for (const subscription of this.affected(effect)) {
      const entries = subscription.overlay.filter((entry) => entry.call === call)
      if (effect.kind === 'upserts' && entries.length === 0) subscription.overlay.push({ call, id: itemID(response), item: response, pending: false })
      for (const entry of entries) {
        entry.pending = false
        if (effect.kind === 'upserts') {
          entry.id = itemID(response)
          entry.item = response
        }
      }
      this.deliver(subscription)
    }
  }

  private rollBack(effect: Effect, call: number): void {
    for (const subscription of this.affected(effect)) {
      subscription.overlay = subscription.overlay.filter((entry) => entry.call !== call)
      this.deliver(subscription)
    }
  }
}

/** The server's list with the optimistic changes of the calls not yet replaced by a push. */
function overlaid(subscription: Subscription): unknown {
  if (subscription.overlay.length === 0) return subscription.data
  const list = subscription.data as ListResponse<unknown>
  let items = list.items
  let total = list.total
  for (const entry of subscription.overlay) {
    const present = items.some((item) => itemID(item) === entry.id)
    if (entry.item === undefined) {
      if (present) total--
      items = items.filter((item) => itemID(item) !== entry.id)
    } else if (present) {
      items = items.map((item) => (itemID(item) === entry.id ? entry.item : item))
    } else {
      items = [...items, entry.item]
      total++
    }
  }
  return { ...list, items, total }
}

function declarationOf(target: { readonly target: string }): string {
  return target.target.split('.')[0] ?? target.target
}

function presenceKey(presence: Presence): string {
  return JSON.stringify([presence.declaration, Object.entries(presence.parameters).sort()])
}

function joinFrame(presence: Presence): Omit<RequestFrame, 'id'> {
  return { op: 'join', target: presence.declaration, params: presence.parameters, body: presence.state }
}
