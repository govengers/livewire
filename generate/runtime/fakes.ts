// Test doubles of the runtime's tests: a WebSocket the test drives frame by frame and a fetch
// that answers from a queue. Not copied by the generator.

import type { WebSocketLike } from './runtime'

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
  init: RequestInit
}

/** A fetch that records its calls and answers them from responses, in order. */
export function fakeFetch(...responses: (Response | Error)[]): { fetch: typeof fetch; calls: FetchCall[] } {
  const calls: FetchCall[] = []
  const fetcher = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    calls.push({ url: String(input), init: init ?? {} })
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

/** Waits until condition holds, polling every millisecond, at most a second. */
export async function waitFor(condition: () => boolean): Promise<void> {
  const deadline = Date.now() + 1000
  while (!condition()) {
    if (Date.now() > deadline) throw new Error('the condition did not hold within a second')
    await new Promise((resolve) => setTimeout(resolve, 1))
  }
}

/** Resolves after the pending promise callbacks ran. */
export function settle(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0))
}
