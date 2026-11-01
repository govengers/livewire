// Calls the generated client of the fixture registry the way an application does; tsc checks
// it under the strictest settings, and every @ts-expect-error line must stay an error.

import { effectScope, shallowRef } from 'vue'
import {
  calls,
  createPart,
  deletePart,
  downloadPartsExport,
  getDashboard,
  getLabels,
  getParts,
  getPartsDistinct,
  getPartsHistory,
  getPartsItem,
  getStats,
  live,
  partsExportURL,
  reads,
  rebuild,
  saveFilter,
  uploadAttachment,
  batch,
  chargePart,
  listFilters,
  deleteFilter,
} from './api/api'
import type { PartsFilter, HistoryFilter } from './api/filters'
import { ApiError, EndedError, NetworkError, batchCall, createLivePair, prepareCall, uuidv7, type LivePair } from './api/runtime'
import type { ErrorCode, FixturePart, Kind, ListResponse, PresenceItem, Stat, Version } from './api/types'
import { livePlugin, useLive, useLiveConnection, useLiveList, useLivePresence, useLiveStatus } from './api/vue'

let session = 'password:user'
const pairOf = (): LivePair => createLivePair({
  identity: () => session,
  client: { baseURL: 'https://example.com/api/v1', headers: () => ({ Authorization: 'Bearer key' }) },
  connection: { url: 'wss://example.com/api/v1/socket' },
})
/** The plugin's reference: replacing its pair is how every composable moves to a new identity. */
const pair = shallowRef(pairOf())
export const plugin = livePlugin(pair)

/**
 * An account switch (login and logout alike): end the old pair first, before the request that
 * changes the session cookie, since neither instance can see the cookie and a request in between
 * would go out with the new one; then provide the new pair through the plugin's reference.
 */
export async function switchAccount(next: string, changeSession: () => Promise<void>): Promise<void> {
  pair.value.end()
  await changeSession()
  session = next
  pair.value = pairOf()
}

export async function rest(): Promise<void> {
  const client = pair.value.client
  const page: ListResponse<FixturePart> = await getParts(client)
  const next: string | undefined = page.next
  const filter: PartsFilter = {
    where: {
      and: [
        { field: 'kind', op: 'in', value: ['bolt'] },
        { field: 'createdAt', op: 'gte', value: { relative: '-7d' } },
        { not: { field: 'tags', op: 'hasAny', value: ['red', 'blue'] } },
        { or: [{ field: 'price', op: 'lt', value: 10 }, { field: 'price', op: 'isNull', value: true }] },
      ],
    },
    search: 'bolt',
  }
  await getParts(client, { filter, sort: ['-createdAt', 'name'], limit: 50, after: next ?? '' })
  const item: FixturePart = await getPartsItem(client, { id: uuidv7() })
  const secret: string | undefined = item.secret
  const price: number | null = item.price
  const kind: Kind = item.kind
  const distinct = await getPartsDistinct(client, { field: 'kind' })
  const history: ListResponse<Version<FixturePart>> = await getPartsHistory(client, { id: item.id, filter: { where: { field: 'operation', op: 'eq', value: 'update' } } })
  const historyFilter: HistoryFilter = { where: { field: 'validTo', op: 'isNull', value: true } }
  const stats: ListResponse<Stat> = await getStats(client, { board: 'b', state: 'open', tag: ['a'], since: new Date().toISOString(), kind: 'nut' })
  const hits: number | undefined = stats.items[0]?.hits
  const labels: ListResponse<string> = await getLabels(client)
  const dashboard = await getDashboard(client, { board: 'b' })
  const created: FixturePart = await createPart(client, { name: 'bolt', kind: 'bolt' }, { idempotencyKey: uuidv7() })
  const nothing: void = await deletePart(client, { id: created.id })
  await chargePart(client, {}, { body: { reason: 'r' } })
  await rebuild(client)
  await uploadAttachment(client, { id: created.id }, new Blob(['x'], { type: 'image/png' }), { contentType: 'image/png', multipart: true })
  const url: string = partsExportURL(client, { filter, format: 'csv' })
  const download = await downloadPartsExport(client)
  const rows: number | null = download.rows
  const stored = await saveFilter(client, { resource: 'parts', filter })
  const storedFilter: PartsFilter = stored.filter
  await saveFilter(client, { resource: 'parts.history', filter: historyFilter, name: 'open versions' })
  await listFilters(client, 'parts')
  await deleteFilter(client, stored.id)
  await batch(client, [batchCall(calls.createPart, {}, { name: 'a', kind: 'nut' }), batchCall(calls.deletePart, { id: created.id }, undefined)])
  try {
    await client.read(reads.notes, {})
  } catch (error) {
    if (error instanceof ApiError) {
      const code: ErrorCode = error.code
      console.log(code, error.status, error.fields['name'])
    }
    if (error instanceof NetworkError) console.log(error.message)
  }
  console.log(secret, price, kind, distinct[0]?.count, history.items[0]?.data?.name, hits, labels.total, dashboard.tree.name, nothing, url, rows, storedFilter)

  // @ts-expect-error a list read takes no unknown parameter
  await getParts(client, { unknown: 1 })
  // @ts-expect-error a field the resource does not filter on
  await getParts(client, { filter: { where: { field: 'extra', op: 'eq', value: 'x' } } })
  // @ts-expect-error an operator of another field type
  await getParts(client, { filter: { where: { field: 'kind', op: 'contains', value: 'b' } } })
  // @ts-expect-error a value of another type
  await getParts(client, { filter: { where: { field: 'price', op: 'eq', value: '10' } } })
  // @ts-expect-error an enum value the field does not list
  await getParts(client, { filter: { where: { field: 'kind', op: 'eq', value: 'screw' } } })
  // @ts-expect-error a field that does not sort
  await getParts(client, { sort: ['tags'] })
  // @ts-expect-error notes has no searchable field
  await client.read(reads.notes, { filter: { search: 'x' } })
  // @ts-expect-error notes pages by offset only
  await client.read(reads.notes, { after: 'cursor' })
  // @ts-expect-error a path parameter is required
  await getPartsItem(client, {})
  // @ts-expect-error a distinct read names a filterable scalar field
  await getPartsDistinct(client, { field: 'tags' })
  // @ts-expect-error an enum parameter
  await getStats(client, { board: 'b', state: 'closed' })
  // @ts-expect-error a body of the wrong shape
  await createPart(client, { title: 'x' })
  // @ts-expect-error a content type the upload does not declare
  await uploadAttachment(client, { id: created.id }, new Blob(), { contentType: 'text/plain' })
  // @ts-expect-error a stored filter's document belongs to its target
  await saveFilter(client, { resource: 'parts.history', filter })
  // @ts-expect-error a guarded field may be absent
  const required: string = item.secret
  // @ts-expect-error a nullable field may be null
  const present: number = item.price
  console.log(required, present)
}

export function socket(): void {
  const connection = pair.value.connection
  connection.onStatus((status) => console.log(status))
  const subscription = connection.subscribe(live.parts, { limit: 10 }, {
    next: (state) => console.log(state.data.items.length, state.pending, state.filterId, state.identity),
    error: (error) => console.log(error instanceof ApiError ? error.code : error.message),
  })
  subscription.unsubscribe()
  void connection.call(calls.createPart, {}, { name: 'a', kind: 'bolt' }, { optimistic: undefined as unknown as FixturePart })
  void connection.call(calls.deletePart, { id: uuidv7() }, undefined)
  void connection.query(reads.notes, {})
  void connection.batch([batchCall(calls.rebuild, {}, undefined)])
  const registration = connection.join(live['parts.presence'], { key: 'k' }, { cursor: 3 }, { onError: (error) => console.log(error.message) })
  registration.update({ cursor: 4 })
  void registration.ready.then(() => registration.leave())
  const prepared = prepareCall(calls.createPart, { name: 'a', kind: 'bolt' })
  void connection.call(calls.createPart, {}, prepared.body, { idempotencyKey: prepared.idempotencyKey, signal: new AbortController().signal })
  connection.checkIdentity()
  console.log(connection.ended, pair.value.client.ended, pair.value.client.identity, new EndedError('x', true).sent)
  // @ts-expect-error notes is not live
  connection.subscribe(live.notes, {}, { next: () => {}, error: () => {} })
  // @ts-expect-error a mutation's body
  void connection.call(calls.createPart, {}, { name: 1 })
}

export function composables(): void {
  const connection = () => pair.value.connection
  effectScope().run(() => {
    const parts = useLive(live.parts, { limit: 10 }, { cache: true, connection })
    const first: FixturePart | undefined = parts.data.value?.items[0]
    const loading: boolean = parts.loading.value
    const dashboard = useLive(live.dashboard, () => ({ board: 'b' }), { connection })
    console.log(useLiveConnection(connection).value.identity())
    const list = useLiveList(live.parts, { connection, mode: 'cursor', limit: 20, sort: ['-createdAt'] })
    list.filter.value = { where: { field: 'name', op: 'startsWith', value: 'b' } }
    list.loadMore()
    const items: FixturePart[] = list.items.value
    const status = useLiveStatus(connection)
    const presence = useLivePresence(live['parts.presence'], { key: 'part-1' }, () => ({ cursor: 1 }), { connection })
    const others: PresenceItem<{ cursor: number }>[] = presence.presences.value
    console.log(first, loading, dashboard.data.value?.title, items, status.value, others)
    // @ts-expect-error useLive needs the parameters a read requires
    useLive(live.dashboard)
    // @ts-expect-error a list's filter is its resource's
    list.filter.value = { where: { field: 'unknown', op: 'eq', value: 1 } }
  })
}
