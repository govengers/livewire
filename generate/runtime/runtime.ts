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
 * The one error of an ended instance (D66): a Client or LiveConnection serves one identity, and
 * once it ended (its identity changed, a 4401, end()) every request it had not answered and every
 * request after fails with it. Make a new pair for the next identity. `sent` says whether the
 * request had gone out: a mutation that was sent may have run on the server, so retry it with the
 * same Idempotency-Key (prepareCall) once a new pair serves the identity again.
 */
export class EndedError extends NetworkError {
  readonly sent: boolean

  constructor(message: string, sent = false) {
    super(message)
    this.name = 'EndedError'
    this.sent = sent
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

/** The last UUID v7 of this module: within one millisecond the next counts up from it (RFC 9562, method 1). */
const uuidv7State = { timestamp: -1, counter: 0 }

/**
 * A UUID version 7 (RFC 9562): a millisecond timestamp, a 12-bit counter that starts at a random
 * value each millisecond and counts up within it, then random bits; so the UUIDs of one module
 * sort in the order they were made, also within a millisecond and when the clock steps back.
 */
export function uuidv7(now: number = Date.now()): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  let timestamp = Math.max(Math.floor(now), uuidv7State.timestamp)
  if (timestamp === uuidv7State.timestamp) {
    uuidv7State.counter++
    if (uuidv7State.counter > 0xfff) {
      timestamp++
      uuidv7State.counter = 0
    }
  } else {
    uuidv7State.counter = (((bytes[6] ?? 0) << 8) | (bytes[7] ?? 0)) & 0x7ff // half the range left to count
  }
  uuidv7State.timestamp = timestamp
  let remaining = timestamp
  for (let index = 5; index >= 0; index--) {
    bytes[index] = remaining % 256
    remaining = Math.floor(remaining / 256)
  }
  bytes[6] = uuidv7State.counter >> 8
  bytes[7] = uuidv7State.counter & 0xff
  return formatUUID(bytes, 0x70)
}

/** A random UUID version 4. */
export function uuidv4(): string {
  return formatUUID(crypto.getRandomValues(new Uint8Array(16)), 0x40)
}

function formatUUID(bytes: Uint8Array, version: number): string {
  bytes[6] = ((bytes[6] ?? 0) & 0x0f) | version
  bytes[8] = ((bytes[8] ?? 0) & 0x3f) | 0x80
  const hexadecimal = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('')
  return `${hexadecimal.slice(0, 8)}-${hexadecimal.slice(8, 12)}-${hexadecimal.slice(12, 16)}-${hexadecimal.slice(16, 20)}-${hexadecimal.slice(20)}`
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

/** A call made ready to send and to send again: the body with its client id (D18) and the Idempotency-Key (D16). */
export interface PreparedCall<Body> {
  readonly body: Body
  readonly idempotencyKey: string
}

/**
 * Prepares a call once, so a retry sends the same id and key: pass `prepared.body` and
 * `{ idempotencyKey: prepared.idempotencyKey }` to every attempt, and `prepared.body` (its id) to
 * an optimistic item.
 */
export function prepareCall<Parameters, Body, Response>(
  target: MutationTarget<Parameters, Body, Response>,
  body: NoInfer<Body>,
  options: { idempotencyKey?: string } = {},
): PreparedCall<Body> {
  return { body: target.body ? withClientID(target, body) : body, idempotencyKey: options.idempotencyKey ?? idempotencyKey() }
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

/**
 * Runs a read with its wire parameters and, when its filterId is one the caller stored or
 * registered without a name and the read answers 404 (the filter was evicted, D54), once more
 * with that filter inline. Every read path that sends a filterId goes through it.
 */
async function recoveringEvictedFilter<Result>(wire: Record<string, string>, memory: FilterMemory, ended: () => boolean, run: (wire: Record<string, string>) => Promise<Result>): Promise<Result> {
  try {
    return await run(wire)
  } catch (error) {
    // The recovery belongs to the instance that started the read and dies with it (D66).
    if (ended()) throw error instanceof EndedError ? error : new EndedError('livewire: the read ended with its instance')
    const filterId = wire['filterId']
    const document = filterId === undefined ? undefined : memory.document(filterId)
    if (!(error instanceof ApiError) || error.code !== 'not_found' || document === undefined) throw error
    const inline: Record<string, string> = { ...wire, filter: document }
    delete inline['filterId']
    return run(inline)
  }
}

/**
 * The documents of the unnamed filters a Client stored and a LiveConnection registered, by
 * filterId, as they were sent (D54). One belongs to the pair of one identity: give it to both
 * (ClientOptions.filters, LiveOptions.filters), so a filter either of them stored is recovered on
 * every read path; it ends with the first of them to end, and remembers nothing after.
 */
export class FilterMemory {
  private readonly documents = new Map<string, string>()
  private ended = false

  remember(id: string, document: string): void {
    if (!this.ended) this.documents.set(id, document)
  }

  forget(id: string): void {
    this.documents.delete(id)
  }

  /** The document stored under id, as sent, or undefined. */
  document(id: string): string | undefined {
    return this.documents.get(id)
  }

  /** Forgets every document and remembers none after. */
  end(): void {
    this.ended = true
    this.documents.clear()
  }
}

function requireText(value: unknown, name: string): void {
  if (typeof value !== 'string' || value === '') throw new TypeError(`livewire: ${name} is required`)
}

function requireDuration(value: number | undefined, name: string, minimum: number): void {
  if (value !== undefined && (!Number.isFinite(value) || value < minimum)) throw new TypeError(`livewire: ${name} must be a finite number of at least ${minimum}`)
}

export interface ClientOptions {
  /**
   * Who the application's session acts as now, "provider:subject" or any stable key. The client
   * serves the identity it answers at construction, for its whole life (D66): when it answers
   * another before a request goes out, the client ends instead of sending it. It cannot see a
   * session cookie, so end the client before the request that changes the cookie.
   */
  identity?: () => string
  /** The registry's BasePath, absolute ("https://api.example.com/api/v1") or below the page's origin ("/api/v1"). */
  baseURL: string
  fetch?: typeof fetch
  /** Headers of every request, an API key for instance (D20). */
  headers?: () => Record<string, string> | Promise<Record<string, string>>
  /** Default 'same-origin': the browser sends the session cookie to its own host. */
  credentials?: RequestCredentials
  /** Called with every 401 before it is thrown. */
  onUnauthorized?: (error: ApiError) => void
  /** Where the unnamed filters this client stores are remembered (D54); share it with the LiveConnection. */
  filters?: FilterMemory
}

export interface RequestOptions {
  signal?: AbortSignal
}

export interface CallOptions extends RequestOptions {
  /** Default: a fresh UUID v7, so a retried call with the same key runs once (D16); prepareCall keeps it with the body. */
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

/**
 * The REST client of one identity (D66): every route of the generated api.ts runs through one.
 * end() ends it for good: every request it has not answered settles with an EndedError, whether
 * it still awaited its headers, was on its way or had its response half read, and so does every
 * request after; a D54 retry is not made, nothing is remembered after.
 */
export class Client {
  private readonly options: ClientOptions
  private readonly unnamedFilters: FilterMemory
  private readonly lifetime = new AbortController()
  private readonly servedIdentity: string

  /** Validates and copies the options: a baseURL is required. */
  constructor(options: ClientOptions) {
    requireText(options.baseURL, 'ClientOptions.baseURL')
    this.options = { ...options }
    this.unnamedFilters = options.filters ?? new FilterMemory()
    this.servedIdentity = options.identity?.() ?? ''
  }

  /** The identity the client serves, fixed at construction, "" for nobody. */
  get identity(): string {
    return this.servedIdentity
  }

  get ended(): boolean {
    return this.lifetime.signal.aborted
  }

  /**
   * Ends the client and its FilterMemory for good (logout, an identity change). Call it before
   * the request that changes the session cookie (login, logout, switch): the client cannot see
   * the cookie, so a request between the change and the end would go out with the new one.
   */
  end(): void {
    this.lifetime.abort()
    this.unnamedFilters.end()
  }

  /** Ends the client when the session's identity is no longer the one it serves. */
  private checkIdentity(): void {
    if (!this.ended && this.options.identity !== undefined && this.options.identity() !== this.servedIdentity) this.end()
  }

  /**
   * Runs one request's work and settles it with an EndedError the moment the client ends: the
   * end settles it first, so whatever the work answers later (a response, a cut body, headers
   * that resolved) is never delivered. progress.sent records whether the request went out.
   */
  private untilEnd<Result>(method: string, path: string, work: (progress: { sent: boolean }) => Promise<Result>): Promise<Result> {
    this.checkIdentity()
    const progress = { sent: false }
    const ended = (): EndedError => new EndedError(`livewire: ${method} ${path}: the client ended`, progress.sent)
    if (this.ended) return Promise.reject(ended())
    return new Promise<Result>((resolve, reject) => {
      const onEnd = (): void => reject(ended())
      this.lifetime.signal.addEventListener('abort', onEnd, { once: true })
      const done = (): void => this.lifetime.signal.removeEventListener('abort', onEnd)
      work(progress).then(
        (result) => {
          done()
          resolve(result)
        },
        (error: unknown) => {
          done()
          reject(error)
        },
      )
    })
  }

  async read<Parameters extends object, Data>(target: ReadTarget<Parameters, Data>, parameters: NoInfer<Parameters>, options: RequestOptions = {}): Promise<Data> {
    return recoveringEvictedFilter(wireParameters(parameters), this.unnamedFilters, () => this.ended, async (wire) =>
      (await this.request('GET', routeOf(target.path, target.pathParameters, wire), options)) as Data)
  }

  async call<Parameters extends object, Body, Response>(
    target: MutationTarget<Parameters, Body, Response>,
    parameters: NoInfer<Parameters>,
    body: NoInfer<Body>,
    options: CallOptions = {},
  ): Promise<Response> {
    const path = routeOf(target.path, target.pathParameters, wireParameters(parameters))
    const prepared = prepareCall(target, body, options)
    const headers: Record<string, string> = { [idempotencyHeader]: prepared.idempotencyKey }
    const outgoing: RequestInit = { headers }
    if (target.body) {
      outgoing.body = JSON.stringify(prepared.body)
      headers['Content-Type'] = 'application/json'
    }
    return (await this.request(target.method, path, options, outgoing)) as Response
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

  /** Stores a filter; an unnamed one is remembered as it was sent, so a read of its evicted id sends it inline (D54). */
  async saveFilter<Document>(request: FilterRequest<Document>, options: RequestOptions = {}): Promise<FilterItem<Document>> {
    const document = JSON.stringify(request.filter)
    const outgoing = { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request) }
    const stored = (await this.request('POST', '/filters', options, outgoing)) as FilterItem<Document>
    if (request.name === undefined || request.name === '') this.unnamedFilters.remember(stored.id, document)
    return stored
  }

  async listFilters(resource?: string, options: RequestOptions = {}): Promise<ListResponse<FilterItem>> {
    const path = resource === undefined ? '/filters' : `/filters?${new URLSearchParams({ resource }).toString()}`
    return (await this.request('GET', path, options)) as ListResponse<FilterItem>
  }

  async deleteFilter(id: string, options: RequestOptions = {}): Promise<void> {
    this.unnamedFilters.forget(id)
    await this.request('DELETE', `/filters/${encodeURIComponent(id)}`, options)
  }

  /** The URL of an export (D35), for a link the browser downloads with the session cookie. */
  exportURL<Query extends object>(target: ExportTarget<Query>, query: NoInfer<Query>): string {
    return this.url(routeOf(target.path, [], wireParameters(query))).toString()
  }

  /**
   * Downloads an export. A cut export fails the body's read (D60), a NetworkError then; where the
   * platform exposes trailers, a missing Livewire-Export-Rows trailer fails it as well. An
   * evicted unnamed filterId is sent inline once (D54).
   */
  async download<Query extends object>(target: ExportTarget<Query>, query: NoInfer<Query>, options: RequestOptions = {}): Promise<ExportDownload> {
    return recoveringEvictedFilter(wireParameters(query), this.unnamedFilters, () => this.ended, (wire) => {
      const path = routeOf(target.path, [], wire)
      return this.untilEnd('GET', path, async (progress) => {
        const response = await this.fetch('GET', path, options, {}, progress)
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
      })
    })
  }

  private url(path: string): URL {
    const origin = typeof location === 'undefined' ? undefined : location.origin
    return new URL(this.options.baseURL.replace(/\/$/, '') + path, origin)
  }

  private request(method: string, path: string, options: RequestOptions, outgoing: RequestInit = {}): Promise<unknown> {
    return this.untilEnd(method, path, async (progress) => {
      const response = await this.fetch(method, path, options, outgoing, progress)
      if (response.status === 204) return undefined
      let text: string
      try {
        text = await response.text()
      } catch (cause) {
        throw new NetworkError('livewire: the response was cut off', { cause })
      }
      return text === '' ? undefined : JSON.parse(text)
    })
  }

  /**
   * Sends one request and answers the response of a success; a refusal is thrown as an ApiError.
   * Nothing goes out once the client ended or the session's identity changed, not even a request
   * whose headers resolved after it (untilEnd settles the caller then).
   */
  private async fetch(method: string, path: string, options: RequestOptions, outgoing: RequestInit, progress: { sent: boolean }): Promise<Response> {
    const headers = { ...(await this.options.headers?.()), ...(outgoing.headers as Record<string, string> | undefined) }
    this.checkIdentity()
    if (this.ended) throw new EndedError(`livewire: ${method} ${path}: the client ended`)
    const signal = options.signal === undefined ? this.lifetime.signal : AbortSignal.any([options.signal, this.lifetime.signal])
    const request: RequestInit = { ...outgoing, method, headers, credentials: this.options.credentials ?? 'same-origin', signal }
    progress.sent = true
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

/** The state of the socket: idle without anything to serve, ended for good (D66). */
export type LiveStatus = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'ended'

/**
 * What a subscription delivers: the data, the ids an optimistic call has not settled yet (D36),
 * the filterId an inline filter was registered as (D13), the identity of the connection that
 * delivered it (D66: a snapshot of the data belongs to that identity), and its cause: a snapshot
 * or replace push, a diff push, or an optimistic change of a call.
 */
export interface LiveState<Data> {
  data: Data
  pending: readonly string[]
  filterId?: string
  identity: string
  cause: 'snapshot' | 'change' | 'optimistic'
}

export interface LiveListener<Data> {
  next(state: LiveState<Data>): void
  /** The subscription ended with an error (an error push, a refused or unanswered subscribe); retry() starts it again. */
  error(error: ApiError | NetworkError): void
  /** The connection ended (D66): drop the data and every optimistic change it delivered; the subscription is over. */
  end?(): void
}

export interface LiveSubscription {
  unsubscribe(): void
  /** Subscribes again after an error ended the subscription. */
  retry(): void
}

/** One component's presence on a record (D37), from LiveConnection.join. */
export interface PresenceRegistration {
  /** Settles with the first answer to the join: rejected when the server refuses it or the connection ends first. */
  readonly ready: Promise<void>
  /** Sends a new state for the record (a join again). */
  update(state: unknown): void
  /** Ends this registration; the socket leaves the record when the last registration on it ends. */
  leave(): void
}

export interface PresenceOptions {
  /** A join the server refused or did not answer; a join lost with its socket is sent again on the next one and reported by its answer. */
  onError?: (error: ApiError | NetworkError) => void
  /** Every join the server accepted, the first and every one after a reconnect or an update. */
  onJoined?: () => void
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

/** The time the connection runs on; tests bring a manual one. */
export interface Clock {
  now(): number
  setTimeout(callback: () => void, milliseconds: number): unknown
  clearTimeout(handle: unknown): void
}

const systemClock: Clock = {
  now: () => Date.now(),
  setTimeout: (callback, milliseconds) => setTimeout(callback, milliseconds),
  clearTimeout: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
}

export interface LiveOptions {
  /** The socket's URL, "wss://example.com/api/v1/socket", or a function that answers it on every connect. */
  url: string | (() => string)
  WebSocket?: new (url: string) => WebSocketLike
  /**
   * Who the application's session acts as now, "provider:subject" or any stable key; "" or none
   * for nobody. The connection serves the identity it answers at construction, for its whole
   * life (D66): when it answers another (checked by checkIdentity on every subscribe, request,
   * connect, opened socket and ping), the connection ends. The Vue snapshots are kept per
   * identity and never without one (D21).
   */
  identity?: () => string
  /** Called on close code 4401 (the identity is gone), right after the connection ended. */
  onUnauthorized?: () => void
  /** Gets what an application callback threw (a status listener, a listener's end, onUnauthorized); without it the error is thrown again in a microtask. */
  onError?: (error: unknown) => void
  /** The first reconnect delay in ms (default 1000), doubling up to maximumReconnectDelay (default 30000), with jitter. */
  reconnectDelay?: number
  maximumReconnectDelay?: number
  /** How often a ping goes out in ms (default 25000); a socket silent for two intervals is closed and reconnected. */
  pingInterval?: number
  /** How long a request, a subscribe included, waits for its answer in ms (default 30000). */
  requestTimeout?: number
  /** How long a settled optimistic change stays without a push in ms (default 1000): no push may come when the list did not change (D65). */
  reconcileWindow?: number
  /** Where the unnamed filters this connection registers are remembered (D54); share it with the pair's Client. */
  filters?: FilterMemory
  random?: () => number
  clock?: Clock
}

/** The socket close codes (section 4). */
export const CloseCode = { normal: 1000, goingAway: 1001, tooSlow: 1013, identityGone: 4401 } as const

const socketOpen = 1

/** A request on the socket (section 4). */
export interface RequestFrame {
  id: string
  op: 'query' | 'subscribe' | 'unsubscribe' | 'call' | 'batch' | 'join' | 'leave' | 'ping'
  target?: string
  params?: Record<string, string>
  body?: unknown
  idempotencyKey?: string
}

/** The answer to a request, by its id. */
export interface ResponseFrame {
  id: string
  ok: boolean
  status: number
  payload?: unknown
  error?: ErrorBody
  subscription?: string
  filterId?: string
}

/** One change of a subscription; seq counts from 1 per subscription. */
export interface PushFrame {
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

/** An optimistic change one call made to a live list (D36, D65); entries are in the order the calls started. */
interface OverlayEntry {
  call: number
  id: string
  item?: unknown
  pending: boolean
}

/** Where a list stood when a call started: a push since then answered the call (D65). */
interface CallStart {
  generation: number
  sequence: number
}

interface Subscription {
  target: { readonly target: string; readonly list: boolean }
  parameters: Record<string, string>
  listener: LiveListener<unknown>
  serverID?: string
  filterId?: string
  /** Counts the server subscriptions this one had; with sequence it orders every push it got. */
  generation: number
  sequence: number
  data?: unknown
  overlay: OverlayEntry[]
  ended: boolean
  retriedInline: boolean
}

interface PresenceHolder {
  options: PresenceOptions
  settle?: (error?: Error) => void
}

/** The socket's presence on one record, shared by every registration on it. */
interface PresenceRecord {
  key: string
  declaration: string
  parameters: Record<string, string>
  state: unknown
  holders: Set<PresenceHolder>
  /** Counts the joins sent; only the answer of the last one counts. */
  joins: number
  inFlight: boolean
  dirty: boolean
  /** Every registration left while a join was in flight: the leave follows its answer, unless a registration joins again first. */
  leaving: boolean
}

export interface LiveCallOptions<Response> extends RequestOptions {
  idempotencyKey?: string
  /** The item an Upserts mutation shows at once, before the server answers (it needs its id). */
  optimistic?: Response
}

/**
 * The socket of one identity (section 4, D66): requests answered by id within a deadline,
 * subscriptions with their pushes applied, reconnects with backoff and every subscription and
 * presence restored after one. It never changes identity: an identity change, a 4401 or end()
 * ends it for good, and the application makes a new one. Nothing connects before the first
 * request.
 */
export class LiveConnection {
  private readonly options: LiveOptions
  private readonly clock: Clock
  private readonly filters: FilterMemory
  private readonly servedIdentity: string
  private isEnded = false
  private socket: WebSocketLike | undefined
  private currentStatus: LiveStatus = 'idle'
  private readonly statusListeners = new Set<(status: LiveStatus) => void>()
  private readonly subscriptions = new Set<Subscription>()
  private readonly byServerID = new Map<string, Subscription>()
  private readonly presences = new Map<string, PresenceRecord>()
  private readonly pending = new Map<string, PendingRequest>()
  private readonly outbox: { id: string; encoded: string }[] = []
  /** The ids of subscribes that timed out: a late answer's server subscription is ended. */
  private readonly abandoned = new Set<string>()
  private lastID = 0
  private lastCall = 0
  private attempt = 0
  private reconnectTimer: unknown
  private pingTimer: unknown
  private lastMessage = 0

  /** Validates and copies the options: a URL is required, the durations are finite and not negative. */
  constructor(options: LiveOptions) {
    if (typeof options.url !== 'function') requireText(options.url, 'LiveOptions.url')
    requireDuration(options.reconnectDelay, 'LiveOptions.reconnectDelay', 0)
    requireDuration(options.maximumReconnectDelay, 'LiveOptions.maximumReconnectDelay', options.reconnectDelay ?? 0)
    requireDuration(options.pingInterval, 'LiveOptions.pingInterval', 1)
    requireDuration(options.requestTimeout, 'LiveOptions.requestTimeout', 1)
    requireDuration(options.reconcileWindow, 'LiveOptions.reconcileWindow', 0)
    this.options = { ...options }
    this.clock = options.clock ?? systemClock
    this.filters = options.filters ?? new FilterMemory()
    this.servedIdentity = options.identity?.() ?? ''
  }

  get status(): LiveStatus {
    return this.currentStatus
  }

  get ended(): boolean {
    return this.isEnded
  }

  /** The identity the connection serves, fixed at construction, "" for nobody. */
  identity(): string {
    return this.servedIdentity
  }

  /** Calls listener with the status now and on every change; the result removes it. */
  onStatus(listener: (status: LiveStatus) => void): () => void {
    this.statusListeners.add(listener)
    listener(this.currentStatus)
    return () => this.statusListeners.delete(listener)
  }

  /** Ends the connection when the session's identity is no longer the one it serves (D66). */
  checkIdentity(): void {
    if (!this.ended && this.options.identity !== undefined && this.options.identity() !== this.servedIdentity) this.end()
  }

  /**
   * Ends the connection for good (D66), in one step: it is marked ended, the reconnect stops,
   * the socket closes, every queued and pending request fails with an EndedError, the FilterMemory
   * ends, every subscription drops its data and every presence registration's ready fails; only
   * then are the listeners (end()) and the status listeners ('ended') told, each under recovery, so
   * a callback that throws or ends again changes nothing of the ending. Logout and an identity
   * change call it; end the pair before the request that changes the session cookie (login,
   * logout, switch), since the connection cannot see the cookie, and make a new pair for the next
   * identity.
   */
  end(): void {
    if (this.isEnded) return
    this.isEnded = true
    this.cancelReconnect()
    const socket = this.socket
    if (socket !== undefined) {
      this.detach(socket)
      socket.close(CloseCode.normal)
    }
    const error = new EndedError('livewire: the connection ended', true)
    const lost = [...this.pending.values()]
    this.pending.clear()
    this.outbox.length = 0
    this.filters.end()
    for (const request of lost) request.reject(error)
    const subscriptions = [...this.subscriptions]
    this.subscriptions.clear()
    this.byServerID.clear()
    for (const subscription of subscriptions) {
      // Without data nothing is delivered again: not a rollback, a settle or a reconcile window.
      delete subscription.data
    }
    for (const record of this.presences.values()) {
      for (const holder of record.holders) holder.settle?.(error)
    }
    this.presences.clear()
    this.currentStatus = 'ended'
    for (const subscription of subscriptions) this.notify(() => subscription.listener.end?.())
    for (const listener of [...this.statusListeners]) this.notify(() => listener('ended'))
  }

  /** Runs a callback of the application; what it throws goes to onError, else is thrown again in a microtask. */
  private notify(callback: () => void): void {
    try {
      callback()
    } catch (error) {
      if (this.options.onError !== undefined) this.options.onError(error)
      else queueMicrotask(() => {
        throw error
      })
    }
  }

  subscribe<Parameters extends object, Data>(
    target: LiveTarget<Parameters, Data>,
    parameters: NoInfer<Parameters>,
    listener: LiveListener<Data>,
  ): LiveSubscription {
    this.checkIdentity()
    const subscription: Subscription = {
      target, parameters: wireParameters(parameters), listener: listener as LiveListener<unknown>,
      generation: 0, sequence: 0, overlay: [], ended: false, retriedInline: false,
    }
    if (this.ended) {
      listener.error(new EndedError('livewire: subscribe on an ended connection'))
      return { unsubscribe: () => {}, retry: () => {} }
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

  /** One read over the socket; an evicted unnamed filterId is sent inline once (D54), by this connection only. */
  async query<Parameters extends object, Data>(target: ReadTarget<Parameters, Data>, parameters: NoInfer<Parameters>, options: RequestOptions = {}): Promise<Data> {
    const frame = await recoveringEvictedFilter(wireParameters(parameters), this.filters, () => this.ended, (wire) =>
      this.request({ op: 'query', target: target.target, params: wire }, options))
    return frame.payload as Data
  }

  /**
   * Runs a mutation over the socket. A mutation with an Upserts effect shows the optimistic item
   * (if given) at once, one with a Removes effect hides the item named by the `id` parameter at
   * once; both are pending until the server answers. Whether or not an item was shown, a list
   * that got a push while the call was in flight keeps the push (the server already answered);
   * any other shows the answer until its next push or the reconcile window; an error takes the
   * change back (D36, D65).
   */
  async call<Parameters extends object, Body, Response>(
    target: MutationTarget<Parameters, Body, Response>,
    parameters: NoInfer<Parameters>,
    body: NoInfer<Body>,
    options: LiveCallOptions<NoInfer<Response>> = {},
  ): Promise<Response> {
    this.checkIdentity()
    const call = ++this.lastCall
    const wire = wireParameters(parameters)
    const effect = target.effect
    const starts = new Map<Subscription, CallStart>()
    if (effect !== undefined && !this.ended) {
      for (const subscription of this.affected(effect)) starts.set(subscription, { generation: subscription.generation, sequence: subscription.sequence })
    }
    if (effect?.kind === 'removes' && wire['id'] !== undefined) this.applyEffect(starts, { call, id: wire['id'], pending: true })
    if (effect?.kind === 'upserts' && options.optimistic !== undefined) {
      this.applyEffect(starts, { call, id: itemID(options.optimistic), item: options.optimistic, pending: true })
    }
    const prepared = prepareCall(target, body, options)
    const frame: Omit<RequestFrame, 'id'> = { op: 'call', target: target.name, params: wire, idempotencyKey: prepared.idempotencyKey }
    if (target.body) frame.body = prepared.body
    let response: ResponseFrame
    try {
      response = await this.request(frame, options)
    } catch (error) {
      this.rollBack(starts, call)
      throw error
    }
    if (effect !== undefined) this.settle(effect, starts, call, wire['id'], response.payload)
    return response.payload as Response
  }

  async batch(calls: readonly BatchCall[], options: { idempotencyKey?: string; signal?: AbortSignal } = {}): Promise<BatchResult[]> {
    const frame = await this.request({ op: 'batch', body: calls, idempotencyKey: options.idempotencyKey ?? idempotencyKey() }, options)
    return frame.payload as BatchResult[]
  }

  /**
   * Announces this socket on the record of a presence read (D37) for as long as the
   * registration lives. Registrations on one record share the socket's presence: its state is
   * the last one sent, and the socket leaves when the last registration leaves; a registration
   * that joins while that leave waits for a join's answer cancels it. The join goes out on the
   * open socket, and again after every reconnect; a refusal reaches `ready` and onError, every
   * accepted join onJoined.
   */
  join<Parameters extends object>(
    target: LiveTarget<Parameters, ListResponse<PresenceItem>>,
    parameters: NoInfer<Parameters>,
    state: unknown,
    options: PresenceOptions = {},
  ): PresenceRegistration {
    this.checkIdentity()
    const holder: PresenceHolder = { options: { ...options } }
    const ready = new Promise<void>((resolve, reject) => {
      holder.settle = (error) => (error === undefined ? resolve() : reject(error))
    })
    ready.catch(() => {}) // the refusal reaches onError too; an unread ready must not be an unhandled rejection
    if (this.ended) {
      holder.settle?.(new EndedError('livewire: join on an ended connection'))
      return { ready, update: () => {}, leave: () => {} }
    }
    const declaration = target.target.split('.')[0] ?? target.target
    const wire = wireParameters(parameters)
    const key = JSON.stringify([declaration, Object.entries(wire).sort()])
    let record = this.presences.get(key)
    if (record === undefined) {
      record = { key, declaration, parameters: wire, state, holders: new Set(), joins: 0, inFlight: false, dirty: false, leaving: false }
      this.presences.set(key, record)
    }
    record.leaving = false
    record.holders.add(holder)
    this.updatePresence(record, state)
    const joined = record
    return {
      ready,
      update: (next) => {
        if (joined.holders.has(holder)) this.updatePresence(joined, next)
      },
      leave: () => this.leavePresence(joined, holder),
    }
  }

  async ping(): Promise<void> {
    await this.request({ op: 'ping' })
  }

  private setStatus(status: LiveStatus): void {
    if (status === this.currentStatus || this.isEnded) return
    this.currentStatus = status
    for (const listener of [...this.statusListeners]) this.notify(() => listener(status))
  }

  private needed(): boolean {
    return this.outbox.length > 0 || this.presences.size > 0 || [...this.subscriptions].some((subscription) => !subscription.ended)
  }

  /** Connects unless a socket is there, a reconnect is waiting or the connection ended. */
  private ensureConnecting(): void {
    if (this.socket === undefined && this.reconnectTimer === undefined && !this.ended) this.connect()
  }

  private cancelReconnect(): void {
    this.clock.clearTimeout(this.reconnectTimer)
    this.reconnectTimer = undefined
  }

  /** Sends a request and answers its response: refused it rejects with an ApiError, unanswered within requestTimeout, aborted or lost with the socket with a NetworkError, on an ended connection with an EndedError. */
  private request(frame: Omit<RequestFrame, 'id'>, options: RequestOptions = {}): Promise<ResponseFrame> {
    this.checkIdentity()
    const identified: RequestFrame = { ...frame, id: String(++this.lastID) }
    const signal = options.signal
    return new Promise<ResponseFrame>((resolve, reject) => {
      if (this.ended) {
        reject(new EndedError(`livewire: ${frame.op} on an ended connection`))
        return
      }
      if (signal?.aborted === true) {
        reject(new NetworkError(`livewire: ${frame.op} was aborted`, { cause: signal.reason }))
        return
      }
      const timeout = this.options.requestTimeout ?? 30_000
      const finish = (): void => {
        this.clock.clearTimeout(timer)
        signal?.removeEventListener('abort', abort)
        this.pending.delete(identified.id)
        const queued = this.outbox.findIndex((entry) => entry.id === identified.id)
        if (queued >= 0) this.outbox.splice(queued, 1)
      }
      const timer = this.clock.setTimeout(() => {
        finish()
        reject(new NetworkError(`livewire: ${frame.op} got no answer within ${timeout} ms`))
      }, timeout)
      const abort = (): void => {
        finish()
        reject(new NetworkError(`livewire: ${frame.op} was aborted`, { cause: signal?.reason }))
      }
      signal?.addEventListener('abort', abort, { once: true })
      this.pending.set(identified.id, {
        frame: identified,
        resolve: (response) => {
          finish()
          resolve(response)
        },
        reject: (error) => {
          finish()
          reject(error)
        },
      })
      this.send(identified)
    }).then((answer) => {
      if (!answer.ok) throw new ApiError(answer.status, answer.error)
      return answer
    })
  }

  /** Sends a frame on the open socket, or queues it for the next socket. */
  private send(frame: RequestFrame): void {
    const encoded = JSON.stringify(frame)
    if (this.socket?.readyState === socketOpen) {
      this.socket.send(encoded)
      return
    }
    this.outbox.push({ id: frame.id, encoded })
    this.ensureConnecting()
  }

  /** Opens a socket for the identity the connection serves; under another one the connection ends instead. */
  private connect(): void {
    this.checkIdentity()
    if (this.ended) return
    this.setStatus(this.attempt === 0 ? 'connecting' : 'reconnecting')
    if (this.ended) return // a status listener ended it: no socket
    const url = typeof this.options.url === 'function' ? this.options.url() : this.options.url
    const SocketClass = this.options.WebSocket ?? (globalThis.WebSocket as unknown as new (url: string) => WebSocketLike)
    const socket = new SocketClass(url)
    this.socket = socket
    socket.onopen = () => this.opened(socket)
    socket.onmessage = (event) => {
      this.lastMessage = this.clock.now()
      this.receive(JSON.parse(String(event.data)) as ResponseFrame | PushFrame)
    }
    socket.onclose = (event) => {
      this.detach(socket)
      this.closed(event.code)
    }
  }

  /** A socket finished its handshake: unless the identity changed meanwhile (then the connection ends), it sends what waited for it. */
  private opened(socket: WebSocketLike): void {
    this.checkIdentity()
    if (this.ended) return
    this.attempt = 0
    this.setStatus('open')
    this.lastMessage = this.clock.now()
    for (const entry of this.outbox.splice(0)) socket.send(entry.encoded)
    for (const subscription of this.subscriptions) if (!subscription.ended) this.sendSubscribe(subscription)
    for (const record of this.presences.values()) this.sendJoin(record)
    this.schedulePing(socket)
  }

  /** Pings every interval; a socket silent for two intervals is half open (a phone that switched networks) and replaced. */
  private schedulePing(socket: WebSocketLike): void {
    const interval = this.options.pingInterval ?? 25_000
    this.pingTimer = this.clock.setTimeout(() => {
      if (this.socket !== socket) return
      this.checkIdentity()
      if (this.socket !== socket) return
      if (this.clock.now() - this.lastMessage > 2 * interval) {
        this.detach(socket)
        socket.close()
        this.closed(1006)
        return
      }
      // A ping that fails fails with its socket, whose close is handled: nothing to report.
      this.ping().catch(() => {})
      this.schedulePing(socket)
    }, interval)
  }

  private detach(socket: WebSocketLike): void {
    socket.onopen = socket.onmessage = socket.onclose = socket.onerror = null
    if (this.socket === socket) this.socket = undefined
    this.clock.clearTimeout(this.pingTimer)
    this.pingTimer = undefined
  }

  /**
   * A socket closed: every answer it owed is lost, the subscriptions and presences start over on
   * the next socket, requests queued for it wait for that socket. A 4401 ends the connection.
   */
  private closed(code: number): void {
    if (code === CloseCode.identityGone) {
      // The identity is gone: end first (everything owed fails with EndedError), then tell the application.
      this.end()
      this.notify(() => this.options.onUnauthorized?.())
      return
    }
    const error = new NetworkError(`livewire: the socket closed (${code}) before the request was answered`)
    const lost = [...this.pending.values()]
    this.pending.clear()
    this.byServerID.clear()
    for (const subscription of this.subscriptions) {
      delete subscription.serverID
      subscription.sequence = 0
    }
    for (const record of [...this.presences.values()]) {
      record.inFlight = false
      if (record.leaving) this.presences.delete(record.key) // its presence is gone with the socket
    }
    for (const request of lost) request.reject(error)
    if (!this.needed()) {
      this.attempt = 0
      this.setStatus('idle')
      return
    }
    this.setStatus('reconnecting')
    this.reconnectTimer = this.clock.setTimeout(() => {
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
    if (request !== undefined) {
      request.resolve(frame)
      return
    }
    if (this.abandoned.delete(frame.id) && frame.ok && frame.subscription !== undefined) {
      this.unsubscribeServer(frame.subscription) // a subscribe answered after its deadline
    }
  }

  /** Subscribes now on an open socket; otherwise the next open sends every subscription. */
  private startSubscription(subscription: Subscription): void {
    if (this.socket?.readyState === socketOpen) this.sendSubscribe(subscription)
    else this.ensureConnecting()
  }

  /** Sends a subscribe on the open socket; unanswered within requestTimeout, the subscription ends with a NetworkError. */
  private sendSubscribe(subscription: Subscription): void {
    const frame: RequestFrame = { id: String(++this.lastID), op: 'subscribe', target: subscription.target.target, params: subscription.parameters }
    const timeout = this.options.requestTimeout ?? 30_000
    const timer = this.clock.setTimeout(() => {
      if (!this.pending.delete(frame.id)) return
      this.abandoned.add(frame.id)
      this.fail(subscription, new NetworkError(`livewire: subscribe got no answer within ${timeout} ms`))
    }, timeout)
    this.pending.set(frame.id, {
      frame,
      resolve: (response) => {
        this.clock.clearTimeout(timer)
        this.pending.delete(frame.id)
        this.subscribed(subscription, response)
      },
      reject: () => this.clock.clearTimeout(timer), // lost with the socket: the next socket subscribes again
    })
    this.send(frame)
  }

  private subscribed(subscription: Subscription, response: ResponseFrame): void {
    if (response.ok && response.subscription !== undefined) {
      if (!this.subscriptions.has(subscription)) {
        this.unsubscribeServer(response.subscription)
        return
      }
      subscription.serverID = response.subscription
      subscription.generation++
      subscription.sequence = 0
      this.byServerID.set(response.subscription, subscription)
      if (response.filterId !== undefined) {
        subscription.filterId = response.filterId
        const inline = subscription.parameters['filter']
        if (inline !== undefined) this.filters.remember(response.filterId, inline)
      }
      return
    }
    const error = new ApiError(response.status, response.error)
    const filterId = subscription.parameters['filterId']
    const document = filterId === undefined ? undefined : this.filters.document(filterId)
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

  /** Ends a server subscription. Its failure needs no report: the server ends a socket's subscriptions with the socket. */
  private unsubscribeServer(serverID: string): void {
    if (this.socket?.readyState !== socketOpen) return
    this.request({ op: 'unsubscribe', target: serverID }).catch(() => {})
  }

  private fail(subscription: Subscription, error: ApiError | NetworkError): void {
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
    this.unsubscribeServer(serverID)
  }

  private push(frame: PushFrame): void {
    const subscription = this.byServerID.get(frame.sub)
    if (subscription === undefined) return
    if (frame.seq !== subscription.sequence + 1) {
      // A lost push: the list cannot be diffed any more; a new subscription starts with a snapshot.
      this.byServerID.delete(frame.sub)
      this.unsubscribeServer(frame.sub)
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
    this.deliver(subscription, frame.kind === 'diff' ? 'change' : 'snapshot')
  }

  private deliver(subscription: Subscription, cause: LiveState<unknown>['cause']): void {
    if (subscription.data === undefined) return
    const pending = subscription.overlay.filter((entry) => entry.pending).map((entry) => entry.id)
    const state: LiveState<unknown> = { data: overlaid(subscription), pending, identity: this.servedIdentity, cause }
    if (subscription.filterId !== undefined) state.filterId = subscription.filterId
    subscription.listener.next(state)
  }

  private affected(effect: Effect): Subscription[] {
    return [...this.subscriptions].filter((subscription) => subscription.target.target === effect.target && subscription.target.list)
  }

  private applyEffect(starts: Map<Subscription, CallStart>, entry: OverlayEntry): void {
    for (const subscription of starts.keys()) {
      subscription.overlay.push({ ...entry })
      this.deliver(subscription, 'optimistic')
    }
  }

  /**
   * The call answered (D65): a list that got a push while the call was in flight already shows
   * the server's state, so the change is dropped; any other keeps it, the answer for an upsert,
   * in the order the calls started (a newer pending change stays on top), until its next push or
   * the reconcile window.
   */
  private settle(effect: Effect, starts: Map<Subscription, CallStart>, call: number, removedID: string | undefined, response: unknown): void {
    for (const [subscription, started] of starts) {
      if (!this.subscriptions.has(subscription)) continue
      subscription.overlay = subscription.overlay.filter((entry) => entry.call !== call)
      const pushed = started.generation !== subscription.generation || started.sequence !== subscription.sequence
      const id = effect.kind === 'upserts' ? itemID(response) : removedID
      if (!pushed && id !== undefined) {
        const settled: OverlayEntry = { call, id, pending: false }
        if (effect.kind === 'upserts') settled.item = response
        const later = subscription.overlay.findIndex((entry) => entry.call > call)
        subscription.overlay.splice(later < 0 ? subscription.overlay.length : later, 0, settled)
        this.clock.setTimeout(() => {
          if (!subscription.overlay.includes(settled)) return
          subscription.overlay = subscription.overlay.filter((entry) => entry !== settled)
          this.deliver(subscription, 'optimistic')
        }, this.options.reconcileWindow ?? 1000)
      }
      this.deliver(subscription, 'optimistic')
    }
  }

  private rollBack(starts: Map<Subscription, CallStart>, call: number): void {
    for (const subscription of starts.keys()) {
      subscription.overlay = subscription.overlay.filter((entry) => entry.call !== call)
      this.deliver(subscription, 'optimistic')
    }
  }

  /** Records the state a registration sends and sends it when the socket is open and no join of the record is in flight. */
  private updatePresence(record: PresenceRecord, state: unknown): void {
    record.state = state
    record.dirty = true
    if (this.socket?.readyState === socketOpen) this.sendJoin(record)
    else this.ensureConnecting()
  }

  /** Sends the record's join; one join of a record is in flight at a time, a newer state follows its answer. */
  private sendJoin(record: PresenceRecord): void {
    if (record.inFlight) return
    record.inFlight = true
    record.dirty = false
    const join = ++record.joins
    const holders = [...record.holders]
    this.request({ op: 'join', target: record.declaration, params: record.parameters, body: record.state }).then(
      () => this.joined(record, join, holders, undefined),
      (error: ApiError | NetworkError) => this.joined(record, join, holders, error),
    )
  }

  private joined(record: PresenceRecord, join: number, holders: PresenceHolder[], error: ApiError | NetworkError | undefined): void {
    if (join !== record.joins || !record.inFlight || this.ended) return // lost with its socket: the next socket joins again and answers
    record.inFlight = false
    for (const holder of holders) {
      if (record.holders.has(holder)) {
        if (error === undefined) holder.options.onJoined?.()
        else holder.options.onError?.(error)
      }
      holder.settle?.(error)
      delete holder.settle
    }
    if (record.leaving) {
      // Every registration left while the join was in flight and none joined again: leave now.
      this.presences.delete(record.key)
      if (error === undefined) this.sendLeave(record)
      return
    }
    if (record.dirty && this.socket?.readyState === socketOpen) this.sendJoin(record)
  }

  private leavePresence(record: PresenceRecord, holder: PresenceHolder): void {
    if (!record.holders.delete(holder) || record.holders.size > 0) return
    if (record.inFlight) {
      record.leaving = true
      return
    }
    this.presences.delete(record.key)
    this.sendLeave(record)
  }

  /**
   * Sends a leave. No registration is left to hear its failure, and none is needed: a presence
   * the server keeps ends with the socket's close or the sweep after PresenceExpiry (D37).
   */
  private sendLeave(record: PresenceRecord): void {
    if (this.socket?.readyState !== socketOpen) return // a closed socket's presences are gone with it
    this.request({ op: 'leave', target: record.declaration, params: record.parameters }).catch(() => {})
  }
}

/** The server's list with the optimistic changes not yet replaced by a push, applied in the order the calls started. */
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

/** The Client and LiveConnection of one identity, sharing one FilterMemory (D66). */
export interface LivePair {
  readonly identity: string
  readonly client: Client
  readonly connection: LiveConnection
  /** The FilterMemory both share; it ends with the first of them to end. */
  readonly filters: FilterMemory
  /** Ends both, and with them their FilterMemory. */
  end(): void
}

export interface LivePairOptions {
  /** Who the application's session acts as now; the pair serves the identity it answers at creation. */
  identity?: () => string
  client: Omit<ClientOptions, 'identity' | 'filters'>
  connection: Omit<LiveOptions, 'identity' | 'filters'>
}

/**
 * Makes the pair of the identity the session has now. On every identity change (login, logout,
 * account switch): end the old pair first, before the request that changes the session cookie,
 * since neither instance can see the cookie and a request in between would go out with the new
 * one; then make the new pair once the session answers the new identity, and put it into the
 * reference livePlugin provides, which the Vue composables follow. The connection ending by
 * itself (checkIdentity, a 4401) ends the client too.
 */
export function createLivePair(options: LivePairOptions): LivePair {
  const filters = new FilterMemory()
  const identity = options.identity?.() ?? ''
  const clientOptions: ClientOptions = { ...options.client, filters }
  if (options.identity !== undefined) clientOptions.identity = options.identity
  const client = new Client(clientOptions)
  const connectionOptions: LiveOptions = { ...options.connection, filters }
  if (options.identity !== undefined) connectionOptions.identity = options.identity
  const connection = new LiveConnection(connectionOptions)
  connection.onStatus((status) => {
    if (status === 'ended') client.end()
  })
  return {
    identity, client, connection, filters,
    end: () => {
      connection.end()
      client.end()
    },
  }
}
