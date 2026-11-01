// The livewire Vue 3 composables (D21): live reads with an identity-keyed snapshot, live lists
// with filter, sort and page state, the connection status and presence. The generator copies
// this file next to runtime.ts. Vue only, no component library.

import {
  computed,
  hasInjectionContext,
  inject,
  onScopeDispose,
  ref,
  shallowRef,
  toValue,
  watch,
  type ComputedRef,
  type InjectionKey,
  type MaybeRefOrGetter,
  type Plugin,
  type Ref,
  type ShallowRef,
} from 'vue'
import {
  itemID,
  wireParameters,
  type ApiError,
  type ListResponse,
  type LiveConnection,
  type LiveState,
  type LiveStatus,
  type LiveSubscription,
  type LiveTarget,
  type PresenceItem,
} from './runtime'

/** The key the application provides its LiveConnection under (livePlugin does). */
export const liveConnectionKey: InjectionKey<LiveConnection> = Symbol('livewire connection')

/** A Vue plugin that provides the connection to every component: app.use(livePlugin(connection)). */
export function livePlugin(connection: LiveConnection): Plugin {
  return { install: (app) => void app.provide(liveConnectionKey, connection) }
}

/** The connection given, else the provided one. */
export function useLiveConnection(connection?: LiveConnection): LiveConnection {
  const resolved = connection ?? (hasInjectionContext() ? inject(liveConnectionKey, undefined) : undefined)
  if (resolved === undefined) throw new Error('livewire: no LiveConnection; app.use(livePlugin(connection)) or pass options.connection')
  return resolved
}

/** The part of the Web Storage API the snapshots need; localStorage by default. */
export interface SnapshotStorage {
  readonly length: number
  key(index: number): string | null
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

export interface UseLiveOptions {
  connection?: LiveConnection
  /** Keep the last data in storage and show it, stale, before the socket answers (D21). */
  cache?: boolean
  storage?: SnapshotStorage
}

export interface LiveRead<Data> {
  data: ShallowRef<Data | undefined>
  error: ShallowRef<ApiError | undefined>
  /** True until the first push and whenever the socket is not open. */
  stale: Ref<boolean>
  /** No data yet and no error: the one condition a skeleton should show for. */
  loading: ComputedRef<boolean>
  /** The ids an optimistic call has not settled yet (D36). */
  pending: ShallowRef<readonly string[]>
  /** The stored filter an inline filter was registered as (D13), for a link to this view. */
  filterId: Ref<string | undefined>
  /** Subscribes again, after an error ended the subscription. */
  retry(): void
}

const snapshotPrefix = 'livewire:'
const identityKey = `${snapshotPrefix}identity`

function defaultStorage(): SnapshotStorage | undefined {
  return typeof localStorage === 'undefined' ? undefined : localStorage
}

/**
 * Reads the snapshot under key for the connection's identity; a snapshot of another identity
 * is never shown: an identity change clears every livewire snapshot first (D21).
 */
function readSnapshot(storage: SnapshotStorage, identity: string, key: string): unknown {
  forgetOtherIdentities(storage, identity)
  const stored = storage.getItem(key)
  if (stored === null) return undefined
  try {
    return JSON.parse(stored)
  } catch {
    storage.removeItem(key)
    return undefined
  }
}

function writeSnapshot(storage: SnapshotStorage, identity: string, key: string, data: unknown): void {
  forgetOtherIdentities(storage, identity)
  try {
    storage.setItem(key, JSON.stringify(data))
  } catch {
    // a full storage keeps the old snapshot: the next push tries again
  }
}

function forgetOtherIdentities(storage: SnapshotStorage, identity: string): void {
  if (storage.getItem(identityKey) === identity) return
  const keys: string[] = []
  for (let index = 0; index < storage.length; index++) {
    const key = storage.key(index)
    if (key?.startsWith(snapshotPrefix) === true) keys.push(key)
  }
  for (const key of keys) storage.removeItem(key)
  storage.setItem(identityKey, identity)
}

function snapshotKey(identity: string, target: string, parameters: Record<string, string>): string {
  return `${snapshotPrefix}${identity}:${target}:${JSON.stringify(Object.entries(parameters).sort())}`
}

/** The arguments after the target: the parameters, optional when the read needs none, then the options. */
type ReadArguments<Parameters, Options> = Record<string, never> extends Parameters
  ? [parameters?: MaybeRefOrGetter<Parameters>, options?: Options]
  : [parameters: MaybeRefOrGetter<Parameters>, options?: Options]

/**
 * Subscribes to a live read for as long as the calling scope lives; a change of the reactive
 * parameters subscribes again and keeps the old data, stale, meanwhile.
 */
export function useLive<Parameters extends object, Data>(target: LiveTarget<Parameters, Data>, ...rest: ReadArguments<Parameters, UseLiveOptions>): LiveRead<Data> {
  const [parameters, options = {}] = rest as [MaybeRefOrGetter<Parameters>?, UseLiveOptions?]
  return liveRead(target, parameters, options)
}

function liveRead<Parameters extends object, Data>(target: LiveTarget<Parameters, Data>, parameters: MaybeRefOrGetter<Parameters> | undefined, options: UseLiveOptions): LiveRead<Data> {
  const connection = useLiveConnection(options.connection)
  const storage = options.cache === true ? (options.storage ?? defaultStorage()) : undefined
  const data = shallowRef<Data>()
  const error = shallowRef<ApiError>()
  const stale = ref(true)
  const pending = shallowRef<readonly string[]>([])
  const filterId = ref<string>()
  let subscription: LiveSubscription | undefined

  const current = (): Parameters => (toValue(parameters) ?? {}) as Parameters
  const subscribe = (): void => {
    subscription?.unsubscribe()
    const wire = wireParameters(current())
    const cacheKey = snapshotKey(connection.identity(), target.target, wire)
    if (storage !== undefined) {
      const cached = readSnapshot(storage, connection.identity(), cacheKey)
      if (cached !== undefined) data.value = cached as Data
    }
    error.value = undefined
    stale.value = true
    subscription = connection.subscribe(target, current(), {
      next: (state: LiveState<Data>) => {
        data.value = state.data
        pending.value = state.pending
        filterId.value = state.filterId
        error.value = undefined
        stale.value = connection.status !== 'open'
        if (storage !== undefined) writeSnapshot(storage, connection.identity(), cacheKey, state.data)
      },
      error: (refusal) => {
        error.value = refusal
      },
    })
  }
  watch(() => JSON.stringify(wireParameters(current())), subscribe, { immediate: true })
  const stopStatus = connection.onStatus((status: LiveStatus) => {
    if (status !== 'open') stale.value = true
  })
  onScopeDispose(() => {
    subscription?.unsubscribe()
    stopStatus()
  })
  return {
    data, error, stale, pending, filterId,
    loading: computed(() => error.value === undefined && stale.value && data.value === undefined),
    retry: () => {
      error.value = undefined
      subscription?.retry()
    },
  }
}

/** The reserved parameters of a filterable list read the list state drives. */
interface ListQuery {
  filter?: unknown
  sort?: readonly string[]
  limit?: number
  offset?: number
  after?: string
}

export interface UseLiveListOptions<Query extends ListQuery> extends UseLiveOptions {
  /** The initial filter document. */
  filter?: Query['filter']
  /** The initial sort, "-field" for descending; none is the resource's default sort. */
  sort?: Query['sort']
  /** The page size; none is the resource's MaximumLimit. */
  limit?: number
  /** offset (default): one page at a time, `page` moves it; cursor: pages tiled with loadMore() (D38). */
  mode?: 'offset' | 'cursor'
  /** The read's other parameters, if any. */
  parameters?: MaybeRefOrGetter<Omit<Query, keyof ListQuery | 'filterId'>>
}

export interface LiveList<Query extends ListQuery, Item> {
  items: ComputedRef<Item[]>
  /** The total under scope and filter (at most CountCap + 1 under a cap, D14). */
  total: ComputedRef<number | undefined>
  error: ComputedRef<ApiError | undefined>
  stale: ComputedRef<boolean>
  loading: ComputedRef<boolean>
  pending: ComputedRef<readonly string[]>
  filter: Ref<Query['filter']>
  sort: Ref<Query['sort']>
  limit: Ref<number | undefined>
  /** The page in offset mode, from 1; a filter or sort change goes back to 1. */
  page: Ref<number>
  /** The pages in offset mode, undefined before the first push or without a limit. */
  pages: ComputedRef<number | undefined>
  /** Whether loadMore() has a page to add (cursor mode). */
  hasMore: ComputedRef<boolean>
  /** Adds the next page below the loaded ones (cursor mode). */
  loadMore(): void
  retry(): void
}

/** One live page of a list: offset mode has one, cursor mode tiles several. */
interface Tile<Item> {
  after: string | undefined
  /** The cursor after the page: its first push's, then what a read of the page answered (a diff carries none). */
  next: Ref<string | undefined>
  data: ShallowRef<ListResponse<Item> | undefined>
  error: ShallowRef<ApiError | undefined>
  pending: ShallowRef<readonly string[]>
  received: Ref<boolean>
  subscription: LiveSubscription
}

/**
 * A live list with its filter, sort and page state (D21). In cursor mode (D38) the first tile
 * is the first page, loadMore() subscribes the page after the last tile's `next`, and when a
 * tile's membership changes (a row arrived or left) its `next` is read again so the tile below
 * follows it and no row falls between two tiles.
 */
export function useLiveList<Query extends ListQuery, Item>(target: LiveTarget<Query, ListResponse<Item>>, options: UseLiveListOptions<Query> = {}): LiveList<Query, Item> {
  const connection = useLiveConnection(options.connection)
  const filter = ref(options.filter) as Ref<Query['filter']>
  const sort = ref(options.sort) as Ref<Query['sort']>
  const limit = ref(options.limit)
  const page = ref(1)
  const tiles = shallowRef<Tile<Item>[]>([])
  const open = ref(connection.status === 'open')
  const cursor = options.mode === 'cursor'

  const queryOf = (after: string | undefined): Query => {
    const query: Record<string, unknown> = { ...(toValue(options.parameters) ?? {}) }
    if (filter.value !== undefined) query['filter'] = filter.value
    if (sort.value !== undefined && sort.value.length > 0) query['sort'] = sort.value
    if (limit.value !== undefined) query['limit'] = limit.value
    if (cursor) {
      if (after !== undefined) query['after'] = after
    } else if (limit.value !== undefined && page.value > 1) {
      query['offset'] = (page.value - 1) * limit.value
    }
    return query as Query
  }

  const startTile = (after: string | undefined, position: number): Tile<Item> => {
    const tile: Tile<Item> = {
      after, next: ref(), data: shallowRef(), error: shallowRef(), pending: shallowRef([]), received: ref(false),
      subscription: undefined as unknown as LiveSubscription,
    }
    tile.subscription = connection.subscribe(target, queryOf(after), {
      next: (state) => {
        const before = tile.data.value
        tile.data.value = state.data
        tile.pending.value = state.pending
        tile.error.value = undefined
        tile.received.value = true
        if (!cursor || tiles.value[position] !== tile) return
        if (before === undefined) follow(position, (tile.next.value = state.data.next))
        else if (membershipChanged(before, state.data)) refreshNext(position)
      },
      error: (refusal) => {
        tile.error.value = refusal
      },
    })
    return tile
  }

  // The tile below a tile starts at its `next`: a tile that moved gets a new one there, which
  // in turn moves the tile below it once its first page arrived.
  const follow = (position: number, next: string | undefined): void => {
    const below = tiles.value[position + 1]
    if (below === undefined || below.after === next) return
    below.subscription.unsubscribe()
    const replaced = [...tiles.value]
    if (next === undefined) {
      for (const dropped of replaced.splice(position + 1)) if (dropped !== below) dropped.subscription.unsubscribe()
    } else {
      replaced[position + 1] = startTile(next, position + 1)
    }
    tiles.value = replaced
  }

  // A diff carries no `next`: a tile whose rows changed reads its page again for it.
  const refreshNext = (position: number): void => {
    const tile = tiles.value[position]
    if (tile === undefined) return
    connection.query(target, queryOf(tile.after)).then((fresh) => {
      if (tiles.value[position] !== tile) return
      tile.next.value = fresh.next
      follow(position, fresh.next)
    }, () => {})
  }

  const restart = (): void => {
    for (const tile of tiles.value) tile.subscription.unsubscribe()
    tiles.value = [startTile(undefined, 0)]
  }
  watch(() => JSON.stringify([toValue(options.parameters), filter.value, sort.value, limit.value]), () => {
    if (page.value !== 1 && !cursor) page.value = 1 // the page watcher restarts
    else restart()
  })
  watch(page, restart, { immediate: true })
  const stopStatus = connection.onStatus((status) => {
    open.value = status === 'open'
  })
  onScopeDispose(() => {
    for (const tile of tiles.value) tile.subscription.unsubscribe()
    stopStatus()
  })

  const items = computed(() => {
    const seen = new Set<string>()
    const merged: Item[] = []
    for (const tile of tiles.value) {
      for (const item of tile.data.value?.items ?? []) {
        const id = itemID(item)
        if (!seen.has(id)) {
          seen.add(id)
          merged.push(item)
        }
      }
    }
    return merged
  })
  const first = computed(() => tiles.value[0])
  const error = computed(() => tiles.value.find((tile) => tile.error.value !== undefined)?.error.value)
  const stale = computed(() => !open.value || tiles.value.some((tile) => !tile.received.value))
  const total = computed(() => first.value?.data.value?.total)
  return {
    items, total, error, stale, filter, sort, limit, page,
    loading: computed(() => error.value === undefined && first.value?.data.value === undefined),
    pending: computed(() => tiles.value.flatMap((tile) => tile.pending.value)),
    pages: computed(() => (total.value === undefined || limit.value === undefined || limit.value === 0 ? undefined : Math.max(1, Math.ceil(total.value / limit.value)))),
    hasMore: computed(() => cursor && tiles.value.at(-1)?.next.value !== undefined),
    loadMore: () => {
      const next = tiles.value.at(-1)?.next.value
      if (cursor && next !== undefined) tiles.value = [...tiles.value, startTile(next, tiles.value.length)]
    },
    retry: () => {
      for (const tile of tiles.value) {
        if (tile.error.value !== undefined) {
          tile.error.value = undefined
          tile.subscription.retry()
        }
      }
    },
  }
}

function membershipChanged(before: ListResponse<unknown>, after: ListResponse<unknown>): boolean {
  return before.items.length !== after.items.length || before.items.some((item, index) => itemID(item) !== itemID(after.items[index]))
}

/** The connection's status, for an online indicator. */
export function useLiveStatus(connection?: LiveConnection): Ref<LiveStatus> {
  const resolved = useLiveConnection(connection)
  const status = ref<LiveStatus>(resolved.status)
  const stop = resolved.onStatus((changed) => {
    status.value = changed
  })
  onScopeDispose(stop)
  return status
}

export interface LivePresence<State> {
  presences: ComputedRef<PresenceItem<State>[]>
  error: ShallowRef<ApiError | undefined>
  stale: Ref<boolean>
}

/**
 * Joins the record of a presence read with the reactive state and subscribes to who is there
 * (D37), for as long as the calling scope lives: a state change joins again (an update), a
 * parameter change leaves the old record and joins the new one, the end of the scope leaves.
 */
export function useLivePresence<Parameters extends object, State>(
  target: LiveTarget<Parameters, ListResponse<PresenceItem>>,
  parameters: MaybeRefOrGetter<Parameters>,
  state: MaybeRefOrGetter<State>,
  options: UseLiveOptions = {},
): LivePresence<State> {
  const connection = useLiveConnection(options.connection)
  const live = liveRead(target, parameters, { ...options, connection })
  let joined: Parameters | undefined
  const failed = (refusal: unknown): void => {
    live.error.value = refusal as ApiError
  }
  watch(
    () => JSON.stringify([wireParameters(toValue(parameters)), toValue(state)]),
    () => {
      const next = toValue(parameters)
      if (joined !== undefined && JSON.stringify(wireParameters(joined)) !== JSON.stringify(wireParameters(next))) {
        connection.leave(target, joined).catch(() => {})
      }
      joined = next
      connection.join(target, next, toValue(state)).catch(failed)
    },
    { immediate: true },
  )
  onScopeDispose(() => {
    if (joined !== undefined) connection.leave(target, joined).catch(() => {})
  })
  return {
    presences: computed(() => (live.data.value?.items ?? []) as PresenceItem<State>[]),
    error: live.error,
    stale: live.stale,
  }
}
