// How the example's frontend uses the generated client (D62, D66): one pair of a REST Client
// and a LiveConnection per signed-in identity, provided to Vue through livePlugin, replaced on
// every login and logout. The example ships no Vue page; this module is type-checked by the
// example's tests under the generator's strict settings.

import { shallowRef } from 'vue'
import { calls, getTodos, live, uploadAttachment } from './api/api'
import { EndedError, NetworkError, createLivePair, prepareCall, type LivePair } from './api/runtime'
import type { ActivityItem, SummaryItem, TodoItem } from './api/types'
import { livePlugin, useLive, useLiveList, useLivePresence, useLiveStatus } from './api/vue'

// snippet:begin pair

/** The pair of one identity, "provider:subject" as the server's session answers it, "" for nobody. */
const pairOf = (identity: string): LivePair => createLivePair({
  identity: () => identity,
  client: { baseURL: '/api/v1' },
  connection: { url: () => `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/v1/socket` },
})

/** The pair every composable follows: app.use(plugin), then await resume(); a login or logout replaces it. */
export const pair = shallowRef(pairOf(''))
export const plugin = livePlugin(pair)

// snippet:end pair

interface IdentityResponse {
  provider: string
  subject: string
}

/**
 * Who the server's session acts as now: GET /auth/me answers the identity, a refusal means
 * nobody. A failure to ask throws: then nobody knows whom a cookie names.
 */
async function sessionIdentity(): Promise<string> {
  const response = await fetch('/auth/me')
  if (response.status === 401 || response.status === 403) return ''
  if (!response.ok) throw new Error(`GET /auth/me answered ${response.status}`)
  const identity = await response.json() as IdentityResponse
  return `${identity.provider}:${identity.subject}`
}

/**
 * Provides the pair of the session the server has now, at the start and after a transition
 * the session could not be read after; while it throws, the ended pair stays and serves nothing.
 */
export async function resume(): Promise<void> {
  pair.value = pairOf(await sessionIdentity())
}

/**
 * Changes the session: the old pair ends first, before the request that changes the cookie,
 * since neither instance can see the cookie and a request in between would go out as the next
 * user (D66). Whatever the change did, a failed or unreadable answer included, the next pair
 * serves the identity the server's session answers afterwards, never the one the change meant.
 * Changes run one after the other: a second login or logout waits until the first built its
 * pair, so no pair is live while a cookie changes, and the last change's pair stays.
 */
function changeSession(change: () => Promise<Response>): Promise<void> {
  const changed = transitions.then(() => changeSessionNow(change))
  transitions = changed.catch(() => undefined)
  return changed
}

/** The session changes in flight, one after the other. */
let transitions: Promise<void> = Promise.resolve()

// snippet:begin change-session

async function changeSessionNow(change: () => Promise<Response>): Promise<void> {
  pair.value.end()
  let failure: unknown
  try {
    const response = await change()
    if (!response.ok) failure = new Error(`the session change answered ${response.status}`)
  } catch (error) {
    failure = error
  }
  await resume()
  if (failure !== undefined) throw failure
}

// snippet:end change-session

/** Logs in with govengers/auth's password route. */
export function login(identifier: string, password: string): Promise<void> {
  return changeSession(() => fetch('/auth/password/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ identifier, password }),
  }))
}

/** Logs out; the next pair serves whoever the session is then, nobody once the logout went through. */
export function logout(): Promise<void> {
  return changeSession(() => fetch('/auth/password/logout', { method: 'POST' }))
}

// snippet:begin views

/** The todo list view: the filterable, paged live list with the summary and the activity. */
export function useTodoList() {
  const todos = useLiveList(live.todos, { sort: ['-createdAt'], limit: 25 })
  todos.filter.value = { where: { field: 'done', op: 'eq', value: false }, search: 'milk' }
  const summary = useLive(live.summary, {})
  const activity = useLive(live.activity, {})
  const status = useLiveStatus()
  const first: TodoItem | undefined = todos.items.value[0]
  const open: number | undefined = (summary.data.value as SummaryItem | undefined)?.open
  const latest: ActivityItem | undefined = activity.data.value?.items[0]
  return { todos, summary, activity, status, first, open, latest }
}

// snippet:end views

/** Who else looks at a todo, with the part of it the user edits. */
export function useTodoPresence(id: string, editing: () => string) {
  return useLivePresence(live['todos.presence'], { key: id }, () => ({ editing: editing() }))
}

// snippet:begin optimistic-create

/**
 * Creates a todo on the socket with an optimistic effect (D36): the live lists show the
 * `optimistic` item at once, pending, under the id the client chose (D18), until the answer and
 * the next push replace it; without `optimistic` they show the answer, pending until that push.
 * The same id and Idempotency-Key go out on a retry, so a retried create runs once (D16). Only a
 * lost answer is retried: a refusal (an ApiError, `conflict` when the id exists) answers the same
 * again, and an ended pair serves no one any more.
 */
export async function createTodo(title: string): Promise<TodoItem> {
  const connection = pair.value.connection
  const prepared = prepareCall(calls.createTodo, { title, priority: 'high' })
  const now = new Date().toISOString()
  const optimistic: TodoItem = { id: prepared.body.id ?? '', title, priority: 'high', done: false, dueAt: null, createdAt: now, updatedAt: now, version: 1 }
  const options = { idempotencyKey: prepared.idempotencyKey, optimistic }
  try {
    return await connection.call(calls.createTodo, {}, prepared.body, options)
  } catch (error) {
    if (error instanceof NetworkError && !(error instanceof EndedError)) {
      return connection.call(calls.createTodo, {}, prepared.body, options)
    }
    throw error
  }
}

// snippet:end optimistic-create

/** Completes a todo; the server enqueues the completion job, whose activity line arrives live. */
export function completeTodo(id: string): Promise<TodoItem> {
  return pair.value.connection.call(calls.updateTodo, { id }, { done: true })
}

/** Attaches a file over REST: uploads have no socket op (D17). */
export function attach(id: string, file: File) {
  return uploadAttachment(pair.value.client, { id }, file, { contentType: 'text/plain' })
}

/** One REST read with a stored-filter-free inline filter. */
export function highPriority() {
  return getTodos(pair.value.client, { filter: { where: { field: 'priority', op: 'eq', value: 'high' } }, limit: 10 })
}
