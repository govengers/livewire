// The session transitions of live.ts: whatever a login or logout answers, the next pair serves
// the identity the server's session answers afterwards (D66).

import { afterEach, describe, expect, test } from 'bun:test'
import { getTodos } from './api/api'
import { EndedError } from './api/runtime'
import { login, logout, pair, resume } from './live'

/** The fake server: who the session is, and what the transition answers. */
let session = ''
let transition: (path: string) => Promise<Response> = () => Promise.resolve(new Response(null, { status: 200 }))
let sessionReadable = true
/** When set, the next /auth/me waits for it: a slow answer. */
let slowSession: Promise<void> | undefined

const realFetch = globalThis.fetch
globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
  const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : new URL(input.url).pathname
  if (path === '/auth/me') {
    const slow = slowSession
    slowSession = undefined
    if (slow !== undefined) await slow
    if (!sessionReadable) throw new TypeError('the network is down')
    if (session === '') return new Response('{"code":"unauthenticated"}', { status: 401 })
    const [provider, subject] = session.split(':')
    return Response.json({ provider, subject })
  }
  return transition(init?.body === undefined ? path : `${path} ${String(init.body)}`)
}) as typeof fetch

afterEach(() => {
  session = ''
  sessionReadable = true
  transition = () => Promise.resolve(new Response(null, { status: 200 }))
})

describe('the pair after a session change', () => {
  test('serves the session the login set', async () => {
    transition = () => {
      session = 'password:bob'
      return Promise.resolve(new Response('{"provider":"password","subject":"bob"}'))
    }
    await login('bob', 'secret')
    expect(pair.value.identity).toBe('password:bob')
  })

  test('serves the new session when the login answer is lost after the cookie changed', async () => {
    session = 'password:alice'
    await resume()
    transition = () => {
      session = 'password:bob' // the cookie changed, then the connection broke
      return Promise.reject(new TypeError('the connection broke'))
    }
    const before = pair.value
    await expect(login('bob', 'secret')).rejects.toThrow('the connection broke')
    expect(before.connection.identity()).toBe('password:alice')
    expect(pair.value.identity).toBe('password:bob')
  })

  test('keeps serving the session a failed logout left', async () => {
    session = 'password:alice'
    await resume()
    transition = () => Promise.resolve(new Response('{"code":"internal"}', { status: 500 }))
    await expect(logout()).rejects.toThrow('500')
    expect(pair.value.identity).toBe('password:alice')
  })

  test('builds no pair while a later login changes the cookie, and keeps the last one', async () => {
    let releaseSession = (): void => {}
    slowSession = new Promise((resolve) => { releaseSession = resolve })
    let releaseCarol = (): void => {}
    const carolsLogin = new Promise<void>((resolve) => { releaseCarol = resolve })
    const liveWhileCookieChanged: string[] = []
    transition = async (request) => {
      if (request.includes('carol')) {
        await carolsLogin
        if (!pair.value.client.ended) liveWhileCookieChanged.push(pair.value.identity)
        session = 'password:carol'
      } else {
        session = 'password:bob'
      }
      return new Response('{}')
    }
    const bobs = login('bob', 'secret') // its /auth/me answers slowly
    await new Promise((resolve) => setTimeout(resolve, 5))
    const carols = login('carol', 'secret') // while bob's pair is not built yet
    releaseSession()
    await new Promise((resolve) => setTimeout(resolve, 5))
    releaseCarol()
    await Promise.all([bobs, carols])
    expect(liveWhileCookieChanged).toEqual([])
    expect(pair.value.identity).toBe('password:carol')
  })

  test('serves nobody once nobody knows the session, until resume reads it', async () => {
    session = 'password:alice'
    await resume()
    sessionReadable = false
    const before = pair.value
    await expect(logout()).rejects.toThrow('the network is down')
    expect(pair.value).toBe(before) // ended: it serves no request
    await expect(getTodos(pair.value.client)).rejects.toBeInstanceOf(EndedError)
    sessionReadable = true
    session = ''
    await resume()
    expect(pair.value.identity).toBe('')
  })
})

process.on('exit', () => { globalThis.fetch = realFetch })
