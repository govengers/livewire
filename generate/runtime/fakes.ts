// Test doubles of the runtime's tests: a WebSocket the test drives frame by frame, a fetch that
// answers from a queue and an XMLHttpRequest for uploads with progress. Not copied by the generator.

import type { Clock, WebSocketLike } from './runtime'

export interface SentFrame {
  id: string
  op: string
  target?: string
  params?: Record<string, string>
  body?: unknown
  idempotencyKey?: string
}

export class FakeSocket implements WebSocketLike {
  readyState = 0
  onopen: ((event: unknown) => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  onclose: ((event: { code: number; reason: string }) => void) | null = null
  onerror: ((event: unknown) => void) | null = null
  readonly sent: SentFrame[] = []
  closedWith: number | undefined

  constructor(readonly url: string) {}

  send(data: string): void {
    this.sent.push(JSON.parse(data) as SentFrame)
  }

  close(code?: number): void {
    this.readyState = 3
    this.closedWith = code ?? 1005
  }

  open(): void {
    this.readyState = 1
    this.onopen?.({})
  }

  receive(frame: unknown): void {
    this.onmessage?.({ data: JSON.stringify(frame) })
  }

  /** The server closes the socket with code. */
  end(code: number): void {
    this.readyState = 3
    this.onclose?.({ code, reason: '' })
  }

  /** The last frame sent with the operation. */
  last(op: string): SentFrame {
    const frame = this.sent.filter((sent) => sent.op === op).at(-1)
    if (frame === undefined) throw new Error(`no ${op} frame was sent; sent: ${JSON.stringify(this.sent)}`)
    return frame
  }

  /** Answers the last frame of the operation. */
  answer(op: string, response: Record<string, unknown> = {}): SentFrame {
    const frame = this.last(op)
    this.receive({ id: frame.id, ok: true, status: 200, ...response })
    return frame
  }
}

/** A WebSocket constructor whose instances the test can reach. */
export function fakeSockets(): { Socket: new (url: string) => FakeSocket; sockets: FakeSocket[]; current(): FakeSocket } {
  const sockets: FakeSocket[] = []
  class Socket extends FakeSocket {
    constructor(url: string) {
      super(url)
      sockets.push(this)
    }
  }
  return {
    Socket,
    sockets,
    current: () => {
      const socket = sockets.at(-1)
      if (socket === undefined) throw new Error('no socket was opened')
      return socket
    },
  }
}

export interface FetchCall {
  url: string
  request: RequestInit
}

/** A fetch that records its calls and answers them from responses, in order. */
export function fakeFetch(...responses: (Response | Error)[]): { fetch: typeof fetch; calls: FetchCall[] } {
  const calls: FetchCall[] = []
  const fetcher = async (input: string | URL | Request, request?: RequestInit): Promise<Response> => {
    calls.push({ url: String(input), request: request ?? {} })
    const response = responses.shift()
    if (response === undefined) throw new Error(`no response left for ${String(input)}`)
    if (response instanceof Error) throw response
    return response
  }
  return { fetch: fetcher as typeof fetch, calls }
}

export function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

/** A clock the test moves: timers run only in advance(), in the order they are due. */
export class FakeClock implements Clock {
  private current = 0
  private lastHandle = 0
  private timers: { at: number; handle: number; callback: () => void }[] = []

  now(): number {
    return this.current
  }

  setTimeout(callback: () => void, milliseconds: number): unknown {
    const handle = ++this.lastHandle
    this.timers.push({ at: this.current + milliseconds, handle, callback })
    return handle
  }

  clearTimeout(handle: unknown): void {
    this.timers = this.timers.filter((timer) => timer.handle !== handle)
  }

  /** Moves the clock by milliseconds and runs every timer due on the way, timers they set included. */
  advance(milliseconds: number): void {
    const end = this.current + milliseconds
    for (;;) {
      const due = this.timers.filter((timer) => timer.at <= end).sort((left, right) => left.at - right.at || left.handle - right.handle)[0]
      if (due === undefined) break
      this.timers = this.timers.filter((timer) => timer !== due)
      this.current = due.at
      due.callback()
    }
    this.current = end
  }
}

/** Resolves after the pending promise callbacks ran. */
export function settle(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0))
}

/** An XMLHttpRequest the test drives: it records the request and answers when told. */
export class FakeXMLHttpRequest {
  method = ''
  url = ''
  readonly headers: Record<string, string> = {}
  withCredentials = false
  body: unknown
  aborted = false
  status = 0
  statusText = ''
  responseText = ''
  readonly upload: { onprogress: ((event: { loaded: number; total: number; lengthComputable: boolean }) => void) | null } = { onprogress: null }
  onload: (() => void) | null = null
  onerror: (() => void) | null = null
  onabort: (() => void) | null = null

  open(method: string, url: string | URL): void {
    this.method = method
    this.url = String(url)
  }

  setRequestHeader(name: string, value: string): void {
    this.headers[name] = value
  }

  send(body: unknown): void {
    this.body = body
  }

  abort(): void {
    this.aborted = true
    this.onabort?.()
  }

  /** The body went out up to loaded bytes of total; without a total the platform cannot tell. */
  progress(loaded: number, total?: number): void {
    this.upload.onprogress?.({ loaded, total: total ?? 0, lengthComputable: total !== undefined })
  }

  respond(status: number, body = ''): void {
    this.status = status
    this.responseText = body
    this.onload?.()
  }

  /** No answer came: offline, refused. */
  fail(): void {
    this.onerror?.()
  }
}

/** Puts a FakeXMLHttpRequest on globalThis until restore(); requests are the ones made, in order. */
export function fakeXMLHttpRequests(): { requests: FakeXMLHttpRequest[]; current(): FakeXMLHttpRequest; restore(): void } {
  const requests: FakeXMLHttpRequest[] = []
  class Request extends FakeXMLHttpRequest {
    constructor() {
      super()
      requests.push(this)
    }
  }
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'XMLHttpRequest')
  Object.defineProperty(globalThis, 'XMLHttpRequest', { configurable: true, writable: true, value: Request })
  return {
    requests,
    current: () => {
      const request = requests.at(-1)
      if (request === undefined) throw new Error('no XMLHttpRequest was made')
      return request
    },
    restore: () => {
      if (descriptor === undefined) Reflect.deleteProperty(globalThis, 'XMLHttpRequest')
      else Object.defineProperty(globalThis, 'XMLHttpRequest', descriptor)
    },
  }
}
