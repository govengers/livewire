// How the example's frontend uses the generated client (D62, D66): one pair of a REST Client
// and a LiveConnection per signed-in identity, provided to Vue through livePlugin, replaced on
// every login and logout. The example ships no Vue page; this module is type-checked by the
// example's tests under the generator's strict settings.

import { shallowRef } from 'vue'
import { calls, getTodos, live, uploadAttachment } from './api/api'
import { ApiError, createLivePair, prepareCall, type LivePair } from './api/runtime'
import type { ActivityItem, SummaryItem, TodoItem } from './api/types'
import { livePlugin, useLive, useLiveList, useLivePresence, useLiveStatus } from './api/vue'

/** Who the session acts as, "provider:subject", as the login answered; "" signed out. */
let signedIn = ''

const pairOf = (): LivePair => createLivePair({
  identity: () => signedIn,
  client: { baseURL: '/api/v1' },
  connection: { url: () => `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/api/v1/socket` },
})

/** The pair every composable follows: app.use(plugin); a login or logout replaces it. */
export const pair = shallowRef(pairOf())
export const plugin = livePlugin(pair)

interface IdentityResponse {
  provider: string
  subject: string
}

/**
 * Logs in with govengers/auth's password route. The old pair ends first, before the request
 * that sets the session cookie: neither instance can see the cookie, so a request in between
 * would go out as the new user (D66). The new pair serves whoever the session is afterwards,
 * the old user when the login failed.
 */
export async function login(identifier: string, password: string): Promise<void> {
  pair.value.end()
  try {
    const response = await fetch('/auth/password/login', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ identifier, password }),
    })
    if (!response.ok) throw new Error(`the login answered ${response.status}`)
    const identity = await response.json() as IdentityResponse
    signedIn = `${identity.provider}:${identity.subject}`
  } finally {
    pair.value = pairOf()
  }
}

/** Logs out: the pair ends before the session does, and the next one serves nobody. */
export async function logout(): Promise<void> {
  pair.value.end()
  try {
    await fetch('/auth/password/logout', { method: 'POST' })
    signedIn = ''
  } finally {
    pair.value = pairOf()
  }
}

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

/** Who else looks at a todo, with the part of it the user edits. */
export function useTodoPresence(id: string, editing: () => string) {
  return useLivePresence(live['todos.presence'], { key: id }, () => ({ editing: editing() }))
}

/**
 * Creates a todo on the socket with an optimistic effect (D36): the list shows it at once,
 * pending, under the id the client chose (D18), and the same id and Idempotency-Key go out on a
 * retry, so a retried create runs once (D16).
 */
export async function createTodo(title: string): Promise<TodoItem> {
  const connection = pair.value.connection
  const prepared = prepareCall(calls.createTodo, { title, priority: 'high' })
  try {
    return await connection.call(calls.createTodo, {}, prepared.body, { idempotencyKey: prepared.idempotencyKey })
  } catch (error) {
    if (error instanceof ApiError && error.code === 'conflict') {
      return connection.call(calls.createTodo, {}, prepared.body, { idempotencyKey: prepared.idempotencyKey })
    }
    throw error
  }
}

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
